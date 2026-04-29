package githubapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestPostSummaryComment(t *testing.T) {
	var captured struct {
		path string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("accept header = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("api version header = %q", r.Header.Get("X-GitHub-Api-Version"))
		}
		captured.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &captured.body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := &Comments{API: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()}
	if err := c.PostSummary(context.Background(), "octo", "repo", 7, "hello"); err != nil {
		t.Fatalf("PostSummary: %v", err)
	}
	if captured.path != "/repos/octo/repo/issues/7/comments" {
		t.Errorf("path = %s", captured.path)
	}
	if !strings.Contains(captured.body["body"].(string), "hello") {
		t.Errorf("body = %v", captured.body["body"])
	}
	if !strings.Contains(captured.body["body"].(string), Marker) {
		t.Errorf("marker missing")
	}
}

func TestPostInlineComment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/repo/pulls/7/comments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		if body["path"] != "src/x.go" || body["line"].(float64) != 42 || body["side"] != "RIGHT" {
			t.Errorf("body = %v", body)
		}
		if body["commit_id"] != "deadbeef" {
			t.Errorf("commit_id = %v", body["commit_id"])
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":2}`))
	}))
	defer srv.Close()

	c := &Comments{API: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()}
	err := c.PostInline(context.Background(), "octo", "repo", 7, "deadbeef", "src/x.go", 42, "msg")
	if err != nil {
		t.Fatalf("PostInline: %v", err)
	}
}

func TestEnsureMarkerIdempotent(t *testing.T) {
	once := ensureMarker("x")
	twice := ensureMarker(once)
	if once != twice {
		t.Errorf("ensureMarker not idempotent:\nonce = %q\ntwice = %q", once, twice)
	}
}

func TestCleanupStaleDeletesUnreliedMarkerComments(t *testing.T) {
	listJSON := `[
		{"id":1,"body":"first ` + Marker + `","in_reply_to_id":null},
		{"id":2,"body":"second ` + Marker + `","in_reply_to_id":null},
		{"id":3,"body":"plain reply, no marker","in_reply_to_id":null},
		{"id":4,"body":"fourth ` + Marker + `","in_reply_to_id":null},
		{"id":5,"body":"reply to id 2","in_reply_to_id":2}
	]`

	var (
		mu        sync.Mutex
		deletedID []int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/pulls/7/comments":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(listJSON))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/octo/repo/pulls/comments/"):
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-1]
			mu.Lock()
			switch id {
			case "1":
				deletedID = append(deletedID, 1)
			case "4":
				deletedID = append(deletedID, 4)
			default:
				t.Errorf("unexpected delete id = %s", id)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Comments{API: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()}
	if err := c.CleanupStale(context.Background(), "octo", "repo", 7); err != nil {
		t.Fatalf("CleanupStale: %v", err)
	}
	sort.Slice(deletedID, func(i, j int) bool { return deletedID[i] < deletedID[j] })
	if len(deletedID) != 2 || deletedID[0] != 1 || deletedID[1] != 4 {
		t.Errorf("deleted = %v, want [1 4]", deletedID)
	}
}

func TestCleanupStaleSwallowsDeleteErrors(t *testing.T) {
	listJSON := `[{"id":1,"body":"x ` + Marker + `","in_reply_to_id":null}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(listJSON))
		case r.Method == http.MethodDelete:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := &Comments{API: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()}
	if err := c.CleanupStale(context.Background(), "octo", "repo", 7); err != nil {
		t.Errorf("CleanupStale: want nil, got %v", err)
	}
}

func TestCleanupStalePropagatesListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Comments{API: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }, HTTP: srv.Client()}
	if err := c.CleanupStale(context.Background(), "octo", "repo", 7); err == nil {
		t.Errorf("CleanupStale: want error, got nil")
	}
}
