package worker

import (
	"context"
	"errors"
	"testing"

	"reviewsrv/pkg/db"
	dbtest "reviewsrv/pkg/db/test"
)

// seedReview creates a Review (with a fake project) and registers cleanup via t.Cleanup.
func seedReview(t *testing.T, dbo db.DB) int {
	t.Helper()
	review, cleanReview := dbtest.Review(t, dbo, nil, dbtest.WithReviewRelations, dbtest.WithFakeReview)
	t.Cleanup(cleanReview)
	return review.ID
}

// cleanupJob deletes a reviewJob row after the test.
func cleanupJob(t *testing.T, dbo db.DB, jobID int) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = dbo.ExecContext(context.Background(),
			`DELETE FROM "reviewJobs" WHERE "reviewJobId" = ?`, jobID)
	})
}

func TestEnqueueAndClaim(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)
	reviewID := seedReview(t, dbo)

	jobID, err := q.Enqueue(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	cleanupJob(t, dbo, jobID)

	job, err := q.Claim(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job.ID != jobID {
		t.Errorf("ID = %d, want %d", job.ID, jobID)
	}
	if job.Status != "running" {
		t.Errorf("Status = %s, want running", job.Status)
	}
	if job.LockedBy == nil || *job.LockedBy != "worker-1" {
		t.Errorf("LockedBy = %v, want worker-1", job.LockedBy)
	}

	second, err := q.Claim(context.Background(), "worker-2")
	if !errors.Is(err, ErrNoJob) {
		t.Errorf("expected ErrNoJob on second Claim, got err=%v job=%v", err, second)
	}
}

func TestFinishMarksDone(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)
	reviewID := seedReview(t, dbo)

	jobID, err := q.Enqueue(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	cleanupJob(t, dbo, jobID)

	if _, err := q.Claim(context.Background(), "w"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := q.Finish(context.Background(), jobID); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var got db.ReviewJob
	if err := dbo.Model(&got).Where(`"reviewJobId" = ?`, jobID).Select(); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("Status = %s, want done", got.Status)
	}
}

func TestFailWithRetryReQueues(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)
	reviewID := seedReview(t, dbo)

	jobID, err := q.Enqueue(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	cleanupJob(t, dbo, jobID)

	job, err := q.Claim(context.Background(), "w")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	attemptsAfterClaim := job.Attempts

	if err := q.Fail(context.Background(), jobID, "transient error", true); err != nil {
		t.Fatalf("Fail(retry=true): %v", err)
	}

	var got db.ReviewJob
	if err := dbo.Model(&got).Where(`"reviewJobId" = ?`, jobID).Select(); err != nil {
		t.Fatalf("select: %v", err)
	}

	if got.Status != "pending" {
		t.Errorf("Status = %s, want pending", got.Status)
	}
	if got.LockedBy != nil {
		t.Errorf("LockedBy = %v, want nil", got.LockedBy)
	}
	if got.LockedAt != nil {
		t.Errorf("LockedAt = %v, want nil", got.LockedAt)
	}
	if got.LastError == nil || *got.LastError != "transient error" {
		t.Errorf("LastError = %v, want 'transient error'", got.LastError)
	}
	if got.Attempts != attemptsAfterClaim {
		t.Errorf("Attempts = %d, want %d (unchanged by Fail)", got.Attempts, attemptsAfterClaim)
	}
}

func TestFailWithoutRetryMarksFailed(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)
	reviewID := seedReview(t, dbo)

	jobID, err := q.Enqueue(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	cleanupJob(t, dbo, jobID)

	if _, err := q.Claim(context.Background(), "w"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := q.Fail(context.Background(), jobID, "permanent error", false); err != nil {
		t.Fatalf("Fail(retry=false): %v", err)
	}

	var got db.ReviewJob
	if err := dbo.Model(&got).Where(`"reviewJobId" = ?`, jobID).Select(); err != nil {
		t.Fatalf("select: %v", err)
	}

	if got.Status != "failed" {
		t.Errorf("Status = %s, want failed", got.Status)
	}
	if got.LastError == nil || *got.LastError != "permanent error" {
		t.Errorf("LastError = %v, want 'permanent error'", got.LastError)
	}
}

func TestClaimSkipsRunningJobs(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)
	reviewID := seedReview(t, dbo)

	jobID, err := q.Enqueue(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	cleanupJob(t, dbo, jobID)

	// First claim puts the job into running state.
	if _, err := q.Claim(context.Background(), "worker-1"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	// Second claim should find no pending jobs.
	second, err := q.Claim(context.Background(), "worker-2")
	if err != ErrNoJob {
		t.Errorf("expected ErrNoJob, got err=%v job=%v", err, second)
	}
}

func TestClaimFIFO(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	q := NewQueue(dbo)

	reviewID1 := seedReview(t, dbo)
	jobID1, err := q.Enqueue(context.Background(), reviewID1)
	if err != nil {
		t.Fatalf("Enqueue job1: %v", err)
	}
	cleanupJob(t, dbo, jobID1)

	reviewID2 := seedReview(t, dbo)
	jobID2, err := q.Enqueue(context.Background(), reviewID2)
	if err != nil {
		t.Fatalf("Enqueue job2: %v", err)
	}
	cleanupJob(t, dbo, jobID2)

	// Claim the first job.
	job1, err := q.Claim(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if job1.ID != jobID1 {
		t.Errorf("first claimed ID = %d, want %d (FIFO violated)", job1.ID, jobID1)
	}

	// Finish job1 so it's no longer running.
	if err := q.Finish(context.Background(), job1.ID); err != nil {
		t.Fatalf("Finish job1: %v", err)
	}

	// Claim the second job.
	job2, err := q.Claim(context.Background(), "worker-2")
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if job2.ID != jobID2 {
		t.Errorf("second claimed ID = %d, want %d (FIFO violated)", job2.ID, jobID2)
	}

	if err := q.Finish(context.Background(), job2.ID); err != nil {
		t.Fatalf("Finish job2: %v", err)
	}
}
