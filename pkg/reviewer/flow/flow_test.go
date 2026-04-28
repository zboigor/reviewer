package flow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"reviewsrv/pkg/reviewer/ctl"
)

// fakeRunner is a test double for Runner.
type fakeRunner struct {
	called bool
	err    error
	result *ctl.ClaudeResult
}

func (f *fakeRunner) Run(_ context.Context, _ string) (*ctl.ClaudeResult, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &ctl.ClaudeResult{
		Type:       "result",
		DurationMs: 1000,
	}, nil
}

// fakeCommenter is a test double for Commenter.
type fakeCommenter struct {
	summaries int
	inlines   int
	cleanups  int
	err       error
}

func (f *fakeCommenter) PostSummary(_ context.Context, _ string) error {
	f.summaries++
	return f.err
}
func (f *fakeCommenter) PostInline(_ context.Context, _ Issue) error {
	f.inlines++
	return f.err
}
func (f *fakeCommenter) CleanupStale(_ context.Context) error {
	f.cleanups++
	return f.err
}

// writeReviewJSON writes a minimal valid review.json to dir.
func writeReviewJSON(t *testing.T, dir string, issues []map[string]any) {
	t.Helper()
	draft := map[string]any{
		"review": map[string]any{
			"externalId":   "pr-1",
			"title":        "Test PR",
			"description":  "test",
			"commitHash":   "abc123",
			"sourceBranch": "feature/test",
			"targetBranch": "main",
			"author":       "dev",
			"createdAt":    "2026-04-28T00:00:00Z",
			"durationMs":   1000,
			"modelInfo": map[string]any{
				"model":        "claude-opus-4-6",
				"inputTokens":  100,
				"outputTokens": 200,
				"costUsd":      0.5,
			},
		},
		"files": []map[string]any{
			{"reviewType": "code", "summary": "ok", "isAccepted": true},
		},
		"issues": issues,
	}
	b, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal review.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.json"), b, 0o644); err != nil {
		t.Fatalf("write review.json: %v", err)
	}
}

// writeMDFile writes a minimal R2.code.md to dir so FindMDFiles + GenerateHTML succeed.
func writeMDFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "R2.code.md"), []byte("# Code review\n\nAll good.\n"), 0o644); err != nil {
		t.Fatalf("write md file: %v", err)
	}
}

func TestRunInvokesRunnerAndCommenter(t *testing.T) {
	dir := t.TempDir()
	writeReviewJSON(t, dir, []map[string]any{
		{
			"localId":     "C1",
			"severity":    "critical",
			"title":       "Missing error handling",
			"description": "Handler ignores errors",
			"file":        "pkg/api/handler.go",
			"lines":       "42-45",
			"issueType":   "error-handling",
			"fileType":    "code",
		},
		{
			"localId":     "C2",
			"severity":    "high",
			"title":       "Missing tests",
			"description": "No tests for new handler",
			"file":        "pkg/api/handler_test.go",
			"lines":       "",
			"issueType":   "tests",
			"fileType":    "code",
		},
		{
			"localId":     "C3",
			"severity":    "low",
			"title":       "Nit: unused import",
			"description": "fmt is imported but unused",
			"file":        "pkg/api/handler.go",
			"lines":       "1",
			"issueType":   "style",
			"fileType":    "code",
		},
	})
	writeMDFile(t, dir)

	runner := &fakeRunner{}
	commenter := &fakeCommenter{}

	result, err := Run(context.Background(), Input{
		Prompt:    "review this",
		Runner:    runner,
		Commenter: commenter,
		Dir:       dir,
		ReviewURL: "https://reviewer.example.com/reviews/1/",
		Model:     "claude-opus-4-6",
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !runner.called {
		t.Error("runner.Run was not called")
	}
	if commenter.cleanups != 1 {
		t.Errorf("CleanupStale called %d times, want 1", commenter.cleanups)
	}
	if commenter.summaries != 1 {
		t.Errorf("PostSummary called %d times, want 1", commenter.summaries)
	}
	// critical + high issues = 2 inline posts; low is skipped.
	if commenter.inlines != 2 {
		t.Errorf("PostInline called %d times, want 2", commenter.inlines)
	}
	if result == nil || result.Draft == nil {
		t.Fatal("result.Draft is nil")
	}
	if result.Draft.Review.Title != "Test PR" {
		t.Errorf("draft title = %q, want %q", result.Draft.Review.Title, "Test PR")
	}
}

func TestRunReturnsErrorWhenRunnerFails(t *testing.T) {
	dir := t.TempDir()
	writeReviewJSON(t, dir, nil)

	runner := &fakeRunner{err: errors.New("claude crashed")}
	commenter := &fakeCommenter{}

	_, err := Run(context.Background(), Input{
		Prompt:    "review this",
		Runner:    runner,
		Commenter: commenter,
		Dir:       dir,
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, runner.err) {
		t.Errorf("error does not wrap runner error: %v", err)
	}
	// Commenter should not be called after runner fails.
	if commenter.cleanups != 0 {
		t.Errorf("CleanupStale called %d times, want 0", commenter.cleanups)
	}
}

func TestRunReturnsErrorWhenReviewJSONMissing(t *testing.T) {
	dir := t.TempDir()
	// Do NOT write review.json — Claude would normally create it, but our fake doesn't.

	_, err := Run(context.Background(), Input{
		Prompt:    "review this",
		Runner:    &fakeRunner{},
		Commenter: &fakeCommenter{},
		Dir:       dir,
	})

	if err == nil {
		t.Fatal("expected error when review.json is missing, got nil")
	}
}

func TestRunNoInlinesForLowSeverityOnly(t *testing.T) {
	dir := t.TempDir()
	writeReviewJSON(t, dir, []map[string]any{
		{
			"localId":     "L1",
			"severity":    "low",
			"title":       "Style nit",
			"description": "formatting",
			"file":        "main.go",
			"lines":       "1",
			"issueType":   "style",
			"fileType":    "code",
		},
	})
	writeMDFile(t, dir)

	commenter := &fakeCommenter{}
	_, err := Run(context.Background(), Input{
		Prompt:    "p",
		Runner:    &fakeRunner{},
		Commenter: commenter,
		Dir:       dir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if commenter.inlines != 0 {
		t.Errorf("PostInline called %d times for low-only issues, want 0", commenter.inlines)
	}
}
