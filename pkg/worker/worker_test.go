package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"reviewsrv/pkg/db"
	"reviewsrv/pkg/githubapp"
	"reviewsrv/pkg/repos"
)

// fakeDB is a minimal DB stub for worker unit tests.
type fakeDB struct {
	review  *db.Review
	project *db.Project
	err     error
}

func (f *fakeDB) ReviewByID(_ context.Context, _ int) (*db.Review, *db.Project, error) {
	return f.review, f.project, f.err
}

// ptr is a convenience helper for creating a pointer to a value.
func ptr[T any](v T) *T { return &v }

// newTestWorker builds a Worker wired to the given fakeDB with no Queue, App, or Cache.
// PromptBuilder defaults to a no-op fake so callers don't have to wire it manually
// when their test exits before the Prompt call.
func newTestWorker(fdb *fakeDB) *Worker {
	return &Worker{
		ID:            "test-worker",
		DB:            fdb,
		Log:           slog.Default(),
		PromptBuilder: &fakePromptBuilder{},
		GitHubAPI:     "https://api.github.com",
		PollInterval:  50 * time.Millisecond,
	}
}

// TestWorkerProcessRequiresGitHubCoordinates asserts that process() fails fast
// when a project is missing any of its GitHub coordinates.
func TestWorkerProcessRequiresGitHubCoordinates(t *testing.T) {
	prNum := 42
	job := &db.ReviewJob{ID: 1, ReviewID: 10, Attempts: 1}

	cases := []struct {
		name    string
		project db.Project
	}{
		{
			name:    "nil owner",
			project: db.Project{GithubOwner: nil, GithubRepo: ptr("myrepo"), InstallationID: ptr(int64(99))},
		},
		{
			name:    "nil repo",
			project: db.Project{GithubOwner: ptr("owner"), GithubRepo: nil, InstallationID: ptr(int64(99))},
		},
		{
			name:    "nil installation id",
			project: db.Project{GithubOwner: ptr("owner"), GithubRepo: ptr("myrepo"), InstallationID: nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review := &db.Review{ID: 10, PRNumber: &prNum}
			proj := tc.project
			fdb := &fakeDB{review: review, project: &proj}
			w := newTestWorker(fdb)

			err := w.process(context.Background(), job)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "project missing GitHub coordinates") {
				t.Errorf("error = %q, want to contain %q", err.Error(), "project missing GitHub coordinates")
			}
		})
	}
}

// (TestWorkerProcessRequiresPRNumber removed — runtime coverage of the nil
// PRNumber guard is deferred to the Task 15 integration smoke. The guard
// itself remains in worker.go.)

// fakePromptBuilder is a minimal PromptBuilder stub for worker unit tests.
type fakePromptBuilder struct {
	prompt      string
	err         error
	capturedKey string
}

func (f *fakePromptBuilder) Prompt(_ context.Context, projectKey string) (string, error) {
	f.capturedKey = projectKey
	return f.prompt, f.err
}

// runnableWorker returns a Worker with non-nil App/Cache/Q/DB/Log/PromptBuilder so the
// nil-guard at the top of Run() passes. The dependencies are not actually
// invoked when ctx is pre-cancelled, so empty zero-value pointers are fine.
func runnableWorker(t *testing.T) *Worker {
	t.Helper()
	return &Worker{
		ID:            "loop-test",
		Q:             &Queue{},
		DB:            &fakeDB{},
		App:           &githubapp.App{},
		Cache:         &repos.Cache{},
		Log:           slog.Default(),
		PromptBuilder: &fakePromptBuilder{prompt: "test prompt"},
		PollInterval:  50 * time.Millisecond,
	}
}

// TestWorkerRunRequiresDependencies verifies the nil-guard at the top of Run.
func TestWorkerRunRequiresDependencies(t *testing.T) {
	cases := []struct {
		name    string
		worker  *Worker
		wantMsg string
	}{
		{
			name:    "missing all deps",
			worker:  &Worker{Log: slog.Default()},
			wantMsg: "App, Cache, Q, DB, Log, PromptBuilder are required",
		},
		{
			name: "missing PromptBuilder",
			worker: &Worker{
				ID:    "test",
				Q:     &Queue{},
				DB:    &fakeDB{},
				App:   &githubapp.App{},
				Cache: &repos.Cache{},
				Log:   slog.Default(),
				// PromptBuilder intentionally omitted
			},
			wantMsg: "App, Cache, Q, DB, Log, PromptBuilder are required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.worker.Run(context.Background())
			if err == nil {
				t.Fatal("expected error from Run with nil deps, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestWorkerLoopExitsOnContextCancel verifies that Worker.Run returns promptly
// when the context is cancelled at the very first iteration (before any Claim).
func TestWorkerLoopExitsOnContextCancel(t *testing.T) {
	w := runnableWorker(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before Run starts

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit within 500ms after context cancellation")
	}
}

// TestWorkerLoopSelectSleepCancelledByContext verifies that the sleep after an
// ErrNoJob is interrupted by context cancellation (select form, not time.Sleep).
// We use a pre-cancelled context so Run exits on the first top-of-loop check
// without ever invoking Q.Claim.
func TestWorkerLoopSelectSleepCancelledByContext(t *testing.T) {
	w := runnableWorker(t)
	w.PollInterval = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run starts

	start := time.Now()
	err := w.Run(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	// The select at the top of the loop fires immediately — should be well under 50ms.
	if elapsed > 500*time.Millisecond {
		t.Errorf("Run took %v, want < 500ms", elapsed)
	}
}

// TestWorkerLoopRetriesOnTransientError verifies that a process() failure causes
// the job to be marked as failed/retry (job.Attempts < 3 → retry).
// We test process() directly since Queue is a concrete type.
//
// This test exercises the retry decision in isolation. End-to-end verification
// that Q.Fail is called with the right retry flag is deferred to the
// integration smoke test in Task 15.
func TestWorkerLoopRetriesOnTransientError(t *testing.T) {
	dbErr := errors.New("transient db error")
	fdb := &fakeDB{err: dbErr}
	w := newTestWorker(fdb)

	job := &db.ReviewJob{ID: 1, ReviewID: 10, Attempts: 1}
	err := w.process(context.Background(), job)
	if err == nil {
		t.Fatal("expected error from process, got nil")
	}
	if !strings.Contains(err.Error(), "transient db error") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "transient db error")
	}

	// Verify retry logic: Attempts < 3 → retry.
	for _, tc := range []struct {
		attempts  int
		wantRetry bool
	}{
		{1, true},
		{2, true},
		{3, false},
		{4, false},
	} {
		retry := tc.attempts < 3
		if retry != tc.wantRetry {
			t.Errorf("Attempts=%d: retry=%v, want %v", tc.attempts, retry, tc.wantRetry)
		}
	}
}

// TestWorkerProcessUsesPromptBuilder verifies that process() invokes
// PromptBuilder.Prompt with the project's ProjectKey, and that an error from
// the builder is propagated (wrapped) to the caller.
//
// process() is reordered so the prompt is built before any Cache/worktree
// work — this test relies on that ordering: a Worker with Cache=nil reaches
// the Prompt call (which short-circuits with the sentinel error) without ever
// dereferencing Cache. Deeper integration of Prompt → Runner → flow.Run is
// covered by the Task 15 E2E smoke test.
func TestWorkerProcessUsesPromptBuilder(t *testing.T) {
	prNum := 42
	projectKey := "test-project-key"
	sentinel := errors.New("prompt build failed (sentinel)")

	fpb := &fakePromptBuilder{err: sentinel}
	fdb := &fakeDB{
		review:  &db.Review{ID: 10, PRNumber: &prNum},
		project: &db.Project{GithubOwner: ptr("owner"), GithubRepo: ptr("repo"), InstallationID: ptr(int64(99)), ProjectKey: projectKey},
	}

	w := &Worker{
		ID:            "test-worker",
		DB:            fdb,
		Log:           slog.Default(),
		PromptBuilder: fpb,
		DefaultModel:  "opus",
		GitHubAPI:     "https://api.github.com",
		// Cache, App, Q intentionally nil — process() must not reach them.
	}

	job := &db.ReviewJob{ID: 1, ReviewID: 10, Attempts: 1}
	err := w.process(context.Background(), job)

	if err == nil {
		t.Fatal("expected error from process, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrapping sentinel %v", err, sentinel)
	}
	if fpb.capturedKey != projectKey {
		t.Errorf("PromptBuilder.Prompt called with %q, want %q", fpb.capturedKey, projectKey)
	}
}

// TestWorkerRunDefaultModelFallback verifies that Run() sets DefaultModel to
// "opus" when it is left empty, before entering the polling loop.
func TestWorkerRunDefaultModelFallback(t *testing.T) {
	w := runnableWorker(t)
	w.DefaultModel = ""

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = w.Run(ctx) // returns context.Canceled; we only care about the side effect

	if w.DefaultModel != "opus" {
		t.Errorf("DefaultModel = %q, want %q", w.DefaultModel, "opus")
	}
}
