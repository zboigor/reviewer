package flow

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reviewsrv/pkg/githubapp"
)

func TestFirstLineParsesRanges(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"42", 42},
		{"42-45", 42},
		{"abc", 0},
		{"0", 0},
		{"1", 1},
		{"-1", 0},
	}
	for _, tt := range tests {
		got := firstLine(tt.input)
		if got != tt.want {
			t.Errorf("firstLine(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func makeComments(t *testing.T, srv *httptest.Server) *githubapp.Comments {
	t.Helper()
	return &githubapp.Comments{
		API:   srv.URL,
		Token: func(context.Context) (string, error) { return "tok", nil },
		HTTP:  srv.Client(),
	}
}

func TestGitHubCommenterPostsInline(t *testing.T) {
	type captured struct {
		method string
		path   string
		body   map[string]any
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got.body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	commenter := &GitHubCommenter{
		Client:   makeComments(t, srv),
		Owner:    "octo",
		Repo:     "repo",
		PRNumber: 7,
		HeadSHA:  "deadbeef",
	}

	iss := Issue{
		LocalID:     "C1",
		Severity:    "critical",
		Title:       "Missing error handling",
		Description: "Handler ignores errors",
		File:        "pkg/api/handler.go",
		Lines:       "42-45",
		IssueType:   "error-handling",
	}

	if err := commenter.PostInline(context.Background(), iss); err != nil {
		t.Fatalf("PostInline: %v", err)
	}

	if got.path != "/repos/octo/repo/pulls/7/comments" {
		t.Errorf("path = %s, want /repos/octo/repo/pulls/7/comments", got.path)
	}
	if got.body["path"] != "pkg/api/handler.go" {
		t.Errorf("body.path = %v, want pkg/api/handler.go", got.body["path"])
	}
	if line, ok := got.body["line"].(float64); !ok || line != 42 {
		t.Errorf("body.line = %v, want 42", got.body["line"])
	}
	if got.body["commit_id"] != "deadbeef" {
		t.Errorf("body.commit_id = %v, want deadbeef", got.body["commit_id"])
	}
	if got.body["side"] != "RIGHT" {
		t.Errorf("body.side = %v, want RIGHT", got.body["side"])
	}
	bodyStr, _ := got.body["body"].(string)
	if !strings.Contains(bodyStr, "Missing error handling") {
		t.Errorf("body does not contain issue title: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, githubapp.Marker) {
		t.Errorf("body does not contain marker")
	}
}

func TestGitHubCommenterFallsBackToSummaryWhenLineMissing(t *testing.T) {
	var (
		paths   []string
		methods []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	commenter := &GitHubCommenter{
		Client:   makeComments(t, srv),
		Owner:    "octo",
		Repo:     "repo",
		PRNumber: 7,
		HeadSHA:  "deadbeef",
	}

	// Lines is empty → should fall back to issues comment endpoint.
	iss := Issue{
		LocalID:     "C2",
		Severity:    "high",
		Title:       "Missing tests",
		Description: "no tests",
		File:        "pkg/api/handler_test.go",
		Lines:       "",
		IssueType:   "tests",
	}

	if err := commenter.PostInline(context.Background(), iss); err != nil {
		t.Fatalf("PostInline: %v", err)
	}

	if len(paths) != 1 {
		t.Fatalf("expected 1 request, got %d", len(paths))
	}
	// Falls back to issues/comments endpoint, not pulls/comments.
	if paths[0] != "/repos/octo/repo/issues/7/comments" {
		t.Errorf("path = %s, want /repos/octo/repo/issues/7/comments", paths[0])
	}
}

func TestGitHubCommenterFallsBackToSummaryWhenFileMissing(t *testing.T) {
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	commenter := &GitHubCommenter{
		Client:   makeComments(t, srv),
		Owner:    "octo",
		Repo:     "repo",
		PRNumber: 7,
		HeadSHA:  "deadbeef",
	}

	// File is empty → should fall back to issues comment endpoint.
	iss := Issue{
		LocalID:     "C3",
		Severity:    "critical",
		Title:       "Security hole",
		Description: "bad thing",
		File:        "",
		Lines:       "10",
		IssueType:   "security",
	}

	if err := commenter.PostInline(context.Background(), iss); err != nil {
		t.Fatalf("PostInline: %v", err)
	}

	if len(paths) != 1 {
		t.Fatalf("expected 1 request, got %d", len(paths))
	}
	if paths[0] != "/repos/octo/repo/issues/7/comments" {
		t.Errorf("path = %s, want /repos/octo/repo/issues/7/comments", paths[0])
	}
}

func TestGitHubCommenterPostSummary(t *testing.T) {
	var captured struct {
		path string
		body map[string]any
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &captured.body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	commenter := &GitHubCommenter{
		Client:   makeComments(t, srv),
		Owner:    "octo",
		Repo:     "repo",
		PRNumber: 7,
		HeadSHA:  "deadbeef",
	}

	if err := commenter.PostSummary(context.Background(), "review summary"); err != nil {
		t.Fatalf("PostSummary: %v", err)
	}

	if captured.path != "/repos/octo/repo/issues/7/comments" {
		t.Errorf("path = %s, want /repos/octo/repo/issues/7/comments", captured.path)
	}
	bodyStr, _ := captured.body["body"].(string)
	if !strings.Contains(bodyStr, "review summary") {
		t.Errorf("body does not contain summary text: %s", bodyStr)
	}
}

func TestGitHubCommenterCleanupStale(t *testing.T) {
	listJSON := `[{"id":1,"body":"old comment ` + githubapp.Marker + `","in_reply_to_id":null}]`
	var deleteCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(listJSON))
		case r.Method == http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	commenter := &GitHubCommenter{
		Client:   makeComments(t, srv),
		Owner:    "octo",
		Repo:     "repo",
		PRNumber: 7,
		HeadSHA:  "deadbeef",
	}

	if err := commenter.CleanupStale(context.Background()); err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	if !deleteCalled {
		t.Error("expected stale comment to be deleted")
	}
}
