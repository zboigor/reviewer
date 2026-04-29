package db_test

import (
	"context"
	"testing"
	"time"

	"reviewsrv/pkg/db"
	dbtest "reviewsrv/pkg/db/test"
)

func TestPRSession_GetReturnsNilWhenAbsent(t *testing.T) {
	dbo, _ := dbtest.Setup(t)
	repo := db.NewPRSessionRepo(dbo)

	s, err := repo.Get(context.Background(), 999999, 12345)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil, got %+v", s)
	}
}

func TestPRSession_UpsertCreatesThenUpdates(t *testing.T) {
	dbo, _ := dbtest.Setup(t)

	// Need a real project row for the FK.
	project, cleanProject := dbtest.Project(t, dbo, nil, dbtest.WithProjectRelations, dbtest.WithFakeProject)
	defer cleanProject()

	repo := db.NewPRSessionRepo(dbo)

	// First upsert = INSERT
	if err := repo.Upsert(context.Background(), project.ID, 42, "session-1"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	s1, err := repo.Get(context.Background(), project.ID, 42)
	if err != nil || s1 == nil {
		t.Fatalf("get after insert: %v %v", s1, err)
	}
	if s1.ClaudeSessionID != "session-1" {
		t.Errorf("session = %q", s1.ClaudeSessionID)
	}
	firstCreated := s1.CreatedAt
	firstUpdated := s1.UpdatedAt

	// Sleep a hair so timestamps differ deterministically.
	time.Sleep(2 * time.Millisecond)

	// Second upsert = UPDATE
	if err := repo.Upsert(context.Background(), project.ID, 42, "session-2"); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	s2, err := repo.Get(context.Background(), project.ID, 42)
	if err != nil || s2 == nil {
		t.Fatalf("get after update: %v %v", s2, err)
	}
	if s2.ClaudeSessionID != "session-2" {
		t.Errorf("session = %q, want session-2", s2.ClaudeSessionID)
	}
	if !s2.CreatedAt.Equal(firstCreated) {
		t.Errorf("createdAt changed: %v -> %v", firstCreated, s2.CreatedAt)
	}
	if !s2.UpdatedAt.After(firstUpdated) {
		t.Errorf("updatedAt didn't advance: %v -> %v", firstUpdated, s2.UpdatedAt)
	}
	if s2.ID != s1.ID {
		t.Errorf("id changed: %d -> %d (should upsert same row)", s1.ID, s2.ID)
	}

	// Cleanup
	_, _ = dbo.Exec(`DELETE FROM "prSessions" WHERE "prSessionId" = ?`, s1.ID)
}
