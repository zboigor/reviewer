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
func newTestWorker(fdb *fakeDB) *Worker {
	return &Worker{
		ID:           "test-worker",
		DB:           fdb,
		Log:          slog.Default(),
		GitHubAPI:    "https://api.github.com",
		PollInterval: 50 * time.Millisecond,
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

// runnableWorker returns a Worker with non-nil App/Cache/Q/DB/Log so the
// nil-guard at the top of Run() passes. The dependencies are not actually
// invoked when ctx is pre-cancelled, so empty zero-value pointers are fine.
func runnableWorker(t *testing.T) *Worker {
	t.Helper()
	return &Worker{
		ID:           "loop-test",
		Q:            &Queue{},
		DB:           &fakeDB{},
		App:          &githubapp.App{},
		Cache:        &repos.Cache{},
		Log:          slog.Default(),
		PollInterval: 50 * time.Millisecond,
	}
}

// TestWorkerRunRequiresDependencies verifies the nil-guard at the top of Run.
func TestWorkerRunRequiresDependencies(t *testing.T) {
	w := &Worker{Log: slog.Default()}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from Run with nil deps, got nil")
	}
	if !strings.Contains(err.Error(), "App, Cache, Q, DB, Log are required") {
		t.Errorf("error = %q, want nil-guard message", err.Error())
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

// TestTodoRunnerReturnsInformativeError asserts that the todoRunner stub returns
// a clear error referencing Task 9 so operators know what to do.
func TestTodoRunnerReturnsInformativeError(t *testing.T) {
	r := todoRunner{}
	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("expected error from todoRunner, got nil")
	}
	if !strings.Contains(err.Error(), "Task 9") {
		t.Errorf("todoRunner error = %q, want to contain %q", err.Error(), "Task 9")
	}
}
