package worker

import (
	"context"
	"errors"
	"time"

	"reviewsrv/pkg/db"

	"github.com/go-pg/pg/v10"
)

// ErrNoJob is returned by Claim when no pending job is available.
var ErrNoJob = errors.New("no job available")

// Queue is a Postgres-backed job queue using FOR UPDATE SKIP LOCKED.
type Queue struct{ db db.DB }

// NewQueue returns a Queue backed by the given DB connection.
func NewQueue(d db.DB) *Queue { return &Queue{db: d} }

// Enqueue inserts a new pending job for the given reviewID and returns its ID.
func (q *Queue) Enqueue(ctx context.Context, reviewID int) (int, error) {
	j := &db.ReviewJob{
		ReviewID:  reviewID,
		Status:    "pending",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if _, err := q.db.ModelContext(ctx, j).Insert(); err != nil {
		return 0, err
	}
	return j.ID, nil
}

// Claim atomically picks the oldest pending job and marks it running.
// Returns (nil, ErrNoJob) if no pending jobs are available.
func (q *Queue) Claim(ctx context.Context, workerID string) (*db.ReviewJob, error) {
	var job db.ReviewJob
	_, err := q.db.QueryOneContext(ctx, &job, `
		UPDATE "reviewJobs" SET
			"status"    = 'running',
			"lockedBy"  = ?,
			"lockedAt"  = now(),
			"updatedAt" = now(),
			"attempts"  = "attempts" + 1
		WHERE "reviewJobId" IN (
			SELECT "reviewJobId" FROM "reviewJobs"
			WHERE "status" = 'pending'
			ORDER BY "createdAt", "reviewJobId"
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING *
	`, workerID)
	if errors.Is(err, pg.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// Finish marks a job as done.
func (q *Queue) Finish(ctx context.Context, jobID int) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE "reviewJobs" SET "status" = 'done', "updatedAt" = now() WHERE "reviewJobId" = ?`, jobID)
	return err
}

// Fail marks a job as failed (or re-queues it as pending when retry is true).
// It clears the lock fields and records the lastError.
func (q *Queue) Fail(ctx context.Context, jobID int, lastErr string, retry bool) error {
	status := "failed"
	if retry {
		status = "pending"
	}
	_, err := q.db.ExecContext(ctx, `
		UPDATE "reviewJobs" SET
			"status"    = ?,
			"lastError" = ?,
			"lockedBy"  = NULL,
			"lockedAt"  = NULL,
			"updatedAt" = now()
		WHERE "reviewJobId" = ?`, status, lastErr, jobID)
	return err
}
