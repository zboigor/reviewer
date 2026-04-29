package app

import (
	"context"
	"fmt"
	"time"

	"reviewsrv/pkg/db"
	"reviewsrv/pkg/githubapp"
	"reviewsrv/pkg/repos"
	"reviewsrv/pkg/reviewer"
	"reviewsrv/pkg/rpc"
	"reviewsrv/pkg/vt"
	"reviewsrv/pkg/worker"

	"github.com/go-pg/pg/v10"
	monitor "github.com/hypnoglow/go-pg-monitor"
	"github.com/labstack/echo/v4"
	"github.com/vmkteam/appkit"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/rpcgen/v2"
	"github.com/vmkteam/rpcgen/v2/typescript"
	"github.com/vmkteam/zenrpc/v2"
)

type Config struct {
	Database *pg.Options
	Server   struct {
		Host      string
		Port      int
		IsDevel   bool
		EnableVFS bool
		BaseURL   string
	}
	Sentry struct {
		Environment string
		DSN         string
	}
	GitHub struct {
		AppID          int64
		PrivateKeyPath string
		APIBaseURL     string
	}
	Repos struct {
		BaseDir string
	}
	Worker struct {
		DefaultModel   string // Claude model name; falls back to "opus" when empty
		Enabled        bool
		Concurrency    int
		PollIntervalMs int
	}
}

type App struct {
	embedlog.Logger
	appName string
	version string
	cfg     Config
	db      db.DB
	dbc     *pg.DB
	mon     *monitor.Monitor
	echo    *echo.Echo
	queue   *worker.Queue
	vtsrv   *zenrpc.Server
	srv     *zenrpc.Server
}

func New(appName, version string, sl embedlog.Logger, cfg Config, db db.DB, dbc *pg.DB) *App {
	a := &App{
		appName: appName,
		version: version,
		cfg:     cfg,
		db:      db,
		dbc:     dbc,
		echo:    appkit.NewEcho(),
		Logger:  sl,
		queue:   worker.NewQueue(db),
	}

	// add services
	a.vtsrv = vt.New(a.db, a.queue, a.Logger, a.cfg.Server.IsDevel, a.cfg.Server.BaseURL)
	a.srv = rpc.New(a.db, a.Logger, a.cfg.Server.IsDevel, a.version)

	return a
}

// Run is a function that runs application.
func (a *App) Run(ctx context.Context) error {
	if a.cfg.Worker.Enabled {
		if err := a.startWorkers(ctx); err != nil {
			return fmt.Errorf("start workers: %w", err)
		}
	}

	a.registerMetrics()
	a.registerHandlers()
	a.registerDebugHandlers()
	a.registerAPIHandlers()
	a.registerVTApiHandlers()
	if err := a.registerFrontendHandlers(); err != nil {
		return err
	}
	if err := a.registerVTFrontendHandlers(); err != nil {
		return err
	}
	a.registerMetadata()

	return a.runHTTPServer(ctx, a.cfg.Server.Host, a.cfg.Server.Port)
}

// startWorkers initialises the GitHub App client, repo cache, and project manager,
// then launches cfg.Worker.Concurrency worker goroutines that poll the queue.
// Each goroutine runs until ctx is cancelled.
func (a *App) startWorkers(ctx context.Context) error {
	ghApp, err := githubapp.NewApp(githubapp.Config{
		AppID:          a.cfg.GitHub.AppID,
		PrivateKeyPath: a.cfg.GitHub.PrivateKeyPath,
		APIBaseURL:     a.cfg.GitHub.APIBaseURL,
	})
	if err != nil {
		return fmt.Errorf("github app: %w", err)
	}

	cache := &repos.Cache{
		BaseDir: a.cfg.Repos.BaseDir,
		Token: func(c context.Context, id int64) (string, error) {
			return ghApp.InstallationToken(c, id)
		},
	}

	pm := reviewer.NewProjectManager(a.db)
	workerDB := workerDBAdapter{
		repo:        db.NewReviewRepo(a.db).WithEnabledAndIssueFilters(),
		sessionRepo: db.NewPRSessionRepo(a.db),
	}

	concurrency := a.cfg.Worker.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	pollInterval := time.Duration(a.cfg.Worker.PollIntervalMs) * time.Millisecond
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	// a.Logger.Log() returns the underlying *slog.Logger from the embedlog.Logger
	// embedded in App. This gives workers the same structured logger as the rest
	// of the application.
	log := a.Logger.Log()

	for i := 0; i < concurrency; i++ {
		w := &worker.Worker{
			ID:            fmt.Sprintf("worker-%d", i),
			Q:             a.queue,
			DB:            workerDB,
			App:           ghApp,
			Cache:         cache,
			Log:           log,
			PollInterval:  pollInterval,
			DefaultModel:  a.cfg.Worker.DefaultModel,
			GitHubAPI:     a.cfg.GitHub.APIBaseURL,
			PromptBuilder: pm,
		}
		go func() {
			// Log only if the worker stopped for a reason other than its context
			// being done (covers both Canceled and DeadlineExceeded).
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				a.Logger.Errorf("worker %s stopped: %v", w.ID, err)
			}
		}()
	}
	return nil
}

// workerDBAdapter satisfies worker.DB by joining the review with its project.
// ReviewRepo.ReviewByID returns (*db.Review, error); we need (*db.Review, *db.Project, error).
// Passing FullReview() as an op causes go-pg to populate review.Project via a JOIN.
type workerDBAdapter struct {
	repo        db.ReviewRepo
	sessionRepo *db.PRSessionRepo
}

func (a workerDBAdapter) ReviewByID(ctx context.Context, id int) (*db.Review, *db.Project, error) {
	review, err := a.repo.ReviewByID(ctx, id, a.repo.FullReview())
	if err != nil {
		return nil, nil, err
	}
	if review == nil {
		return nil, nil, fmt.Errorf("review %d not found", id)
	}
	if review.Project == nil {
		return nil, nil, fmt.Errorf("review %d has no project", id)
	}
	return review, review.Project, nil
}

func (a workerDBAdapter) GetPRSession(ctx context.Context, projectID, prNumber int) (*db.PRSession, error) {
	return a.sessionRepo.Get(ctx, projectID, prNumber)
}

func (a workerDBAdapter) UpsertPRSession(ctx context.Context, projectID, prNumber int, sessionID string) error {
	return a.sessionRepo.Upsert(ctx, projectID, prNumber, sessionID)
}

// TypeScriptClient returns TypeScript client for VT or RPC.
func (a *App) TypeScriptClient(client string) ([]byte, error) {
	gen := rpcgen.FromSMD(a.srv.SMD())
	if client == "vt" {
		gen = rpcgen.FromSMD(a.vtsrv.SMD())
	}

	tsSettings := typescript.Settings{ExcludedNamespace: []string{}, WithClasses: true}
	return gen.TSCustomClient(tsSettings).Generate()
}

// Shutdown is a function that gracefully stops HTTP server.
func (a *App) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a.mon.Close()

	return a.echo.Shutdown(ctx)
}

// registerMetadata is a function that registers meta info from service. Must be updated.
func (a *App) registerMetadata() {
	opts := appkit.MetadataOpts{
		HasPublicAPI:  true,
		HasPrivateAPI: true,
		DBs: []appkit.DBMetadata{
			appkit.NewDBMetadata(a.cfg.Database.Database, a.cfg.Database.PoolSize, false),
		},
		Services: []appkit.ServiceMetadata{
			// NewServiceMetadata("srv", MetadataServiceTypeAsync),
		},
	}

	md := appkit.NewMetadataManager(opts)
	md.RegisterMetrics()

	a.echo.GET("/debug/metadata", md.Handler)
}
