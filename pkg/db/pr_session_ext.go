package db

import (
	"context"
	"errors"
	"time"

	"github.com/go-pg/pg/v10"
)

// PRSessionRepo provides session-cache operations.
type PRSessionRepo struct{ db DB }

func NewPRSessionRepo(d DB) *PRSessionRepo { return &PRSessionRepo{db: d} }

// Get returns the cached Claude session for a PR, or (nil, nil) if absent.
func (r *PRSessionRepo) Get(ctx context.Context, projectID, prNumber int) (*PRSession, error) {
	s := &PRSession{}
	err := r.db.ModelContext(ctx, s).
		Where(`"projectId" = ?`, projectID).
		Where(`"prNumber" = ?`, prNumber).
		First()
	if errors.Is(err, pg.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// Upsert sets the cached session for a PR. Updates updatedAt, preserves createdAt.
func (r *PRSessionRepo) Upsert(ctx context.Context, projectID, prNumber int, sessionID string) error {
	s := &PRSession{
		ProjectID:       projectID,
		PRNumber:        prNumber,
		ClaudeSessionID: sessionID,
		// CreatedAt is set by the DB default on INSERT and preserved on UPDATE
		// (the ON CONFLICT SET clause only touches claudeSessionId + updatedAt).
		UpdatedAt: time.Now(),
	}
	_, err := r.db.ModelContext(ctx, s).
		OnConflict(`("projectId", "prNumber") DO UPDATE`).
		Set(`"claudeSessionId" = EXCLUDED."claudeSessionId", "updatedAt" = EXCLUDED."updatedAt"`).
		Insert()
	return err
}
