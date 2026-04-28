package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"reviewsrv/pkg/db"
	"reviewsrv/pkg/githubapp"
	"reviewsrv/pkg/repos"
	"reviewsrv/pkg/reviewer/ctl"
	"reviewsrv/pkg/reviewer/flow"
)

// DB is the narrow interface the worker needs from the project's database layer.
//
// It does NOT match db.ReviewRepo.ReviewByID directly — Task 11 wires it via a
// small adapter that calls reviewRepo.ReviewByID(ctx, id, db.FullReview()) and
// returns (review, review.Project, err). The interface is kept narrow so the
// worker can be unit-tested without standing up a full repo.
type DB interface {
	ReviewByID(ctx context.Context, id int) (*db.Review, *db.Project, error)
}

// PromptBuilder builds the assembled review prompt for a project.
// Production wiring: (*reviewer.ProjectManager).Prompt.
type PromptBuilder interface {
	Prompt(ctx context.Context, projectKey string) (string, error)
}

// Worker polls the queue, processes jobs, and posts review comments via GitHub.
type Worker struct {
	ID            string
	Q             *Queue
	DB            DB
	App           *githubapp.App
	Cache         *repos.Cache
	Log           *slog.Logger
	PromptBuilder PromptBuilder
	DefaultModel  string        // Claude model name; falls back to "opus"
	GitHubAPI     string        // default: https://api.github.com
	PollInterval  time.Duration // default: 5s
}

// Run polls the queue in a loop until ctx is cancelled.
// It uses a select-based sleep so context cancellation is responded to promptly.
func (w *Worker) Run(ctx context.Context) error {
	if w.App == nil || w.Cache == nil || w.Q == nil || w.DB == nil || w.Log == nil || w.PromptBuilder == nil {
		return errors.New("worker.Run: App, Cache, Q, DB, Log, PromptBuilder are required")
	}
	if w.DefaultModel == "" {
		w.DefaultModel = "opus"
	}
	if w.PollInterval == 0 {
		w.PollInterval = 5 * time.Second
	}
	if w.GitHubAPI == "" {
		w.GitHubAPI = "https://api.github.com"
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		job, err := w.Q.Claim(ctx, w.ID)
		if errors.Is(err, ErrNoJob) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.PollInterval):
			}
			continue
		}
		if err != nil {
			w.Log.ErrorContext(ctx, "claim", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.PollInterval):
			}
			continue
		}

		if err := w.process(ctx, job); err != nil {
			retry := job.Attempts < 3
			if failErr := w.Q.Fail(ctx, job.ID, err.Error(), retry); failErr != nil {
				w.Log.WarnContext(ctx, "fail job", "jobId", job.ID, "err", failErr)
			}
			w.Log.ErrorContext(ctx, "process", "jobId", job.ID, "retry", retry, "err", err)
			continue
		}
		if err := w.Q.Finish(ctx, job.ID); err != nil {
			w.Log.WarnContext(ctx, "finish job", "jobId", job.ID, "err", err)
		}
	}
}

// process handles a single job: fetch PR, clone/update repo, run flow.
func (w *Worker) process(ctx context.Context, job *db.ReviewJob) error {
	review, project, err := w.DB.ReviewByID(ctx, job.ReviewID)
	if err != nil {
		return fmt.Errorf("worker.process: fetch review: %w", err)
	}

	// Cheap validation first — fail fast before any expensive git/HTTP work.
	if project.GithubOwner == nil || project.GithubRepo == nil || project.InstallationID == nil {
		return errors.New("project missing GitHub coordinates")
	}
	if review.PRNumber == nil {
		return errors.New("review missing PR number")
	}

	// Build the prompt before touching the repo cache: a misconfigured project
	// or template should fail fast, before we spend time cloning/fetching.
	prompt, err := w.PromptBuilder.Prompt(ctx, project.ProjectKey)
	if err != nil {
		return fmt.Errorf("worker.process: build prompt: %w", err)
	}

	// upstream is GitHub.com only. GitHub Enterprise deployments would need to
	// derive this from project metadata (e.g. project.VcsURL) — not in scope here.
	upstream := "https://github.com/" + *project.GithubOwner + "/" + *project.GithubRepo + ".git"

	repoPath, err := w.Cache.Ensure(ctx, *project.InstallationID, *project.GithubOwner, *project.GithubRepo, upstream)
	if err != nil {
		return fmt.Errorf("worker.process: ensure repo: %w", err)
	}

	headSHA, err := w.Cache.FetchPR(ctx, repoPath, *project.InstallationID, upstream, *review.PRNumber)
	if err != nil {
		return fmt.Errorf("worker.process: fetch PR: %w", err)
	}

	wt, cleanup, err := w.Cache.Worktree(ctx, repoPath, headSHA)
	if err != nil {
		return fmt.Errorf("worker.process: worktree: %w", err)
	}
	defer cleanup()

	commenter := &flow.GitHubCommenter{
		Client: &githubapp.Comments{
			API:   w.GitHubAPI, // defaulted in Run() before the loop starts
			Token: func(c context.Context) (string, error) { return w.App.InstallationToken(c, *project.InstallationID) },
			HTTP:  nil, // uses http.DefaultClient
		},
		Owner:    *project.GithubOwner,
		Repo:     *project.GithubRepo,
		PRNumber: *review.PRNumber,
		HeadSHA:  headSHA,
	}

	runner := &ctl.ExecClaudeRunner{
		Model: w.DefaultModel,
		Dir:   wt,
		Log:   w.Log,
	}

	_, err = flow.Run(ctx, flow.Input{
		Prompt:    prompt,
		Runner:    runner,
		Commenter: commenter,
		Dir:       wt,
		Log:       w.Log,
	})
	return err
}
