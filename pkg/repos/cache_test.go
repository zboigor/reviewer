package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureCloneCreatesBareRepo(t *testing.T) {
	tmp := t.TempDir()

	// Set up a tiny upstream bare repo on disk for the test.
	upstream := filepath.Join(tmp, "upstream.git")
	must(t, exec.Command("git", "init", "--bare", upstream).Run())
	work := filepath.Join(tmp, "work")
	must(t, exec.Command("git", "init", work).Run())
	mustEnv(t, work, "git", "commit", "--allow-empty", "-m", "x", "--no-gpg-sign")
	mustEnv(t, work, "git", "remote", "add", "origin", upstream)
	mustEnv(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	c := &Cache{BaseDir: filepath.Join(tmp, "cache"), Token: func(context.Context, int64) (string, error) { return "x", nil }}
	path, err := c.Ensure(context.Background(), 1, "owner", "repo", "file://"+upstream)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		t.Errorf("expected bare repo HEAD: %v", err)
	}
}

func TestEnsureSecondCallReusesRepo(t *testing.T) {
	tmp := t.TempDir()

	upstream := filepath.Join(tmp, "upstream.git")
	must(t, exec.Command("git", "init", "--bare", upstream).Run())
	work := filepath.Join(tmp, "work")
	must(t, exec.Command("git", "init", work).Run())
	mustEnv(t, work, "git", "commit", "--allow-empty", "-m", "init", "--no-gpg-sign")
	mustEnv(t, work, "git", "remote", "add", "origin", upstream)
	mustEnv(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	cacheDir := filepath.Join(tmp, "cache")
	c := &Cache{
		BaseDir: cacheDir,
		Token:   func(context.Context, int64) (string, error) { return "x", nil },
	}

	path1, err := c.Ensure(context.Background(), 1, "owner", "repo", "file://"+upstream)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	// Drop a sentinel file inside the bare repo. If the second Ensure
	// re-clones, the sentinel will be wiped.
	sentinel := filepath.Join(path1, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("kept"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Capture HEAD mtime as a secondary signal.
	info1, err := os.Stat(filepath.Join(path1, "HEAD"))
	if err != nil {
		t.Fatalf("stat HEAD after first Ensure: %v", err)
	}

	path2, err := c.Ensure(context.Background(), 1, "owner", "repo", "file://"+upstream)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if path1 != path2 {
		t.Errorf("second Ensure returned different path: %q vs %q", path1, path2)
	}

	// Primary assertion: the sentinel survives. A re-clone would have
	// removed/replaced repoPath, so this proves no clone happened.
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel missing after second Ensure (re-clone happened?): %v", err)
	}

	// Secondary assertion: HEAD mtime is unchanged.
	info2, err := os.Stat(filepath.Join(path2, "HEAD"))
	if err != nil {
		t.Fatalf("stat HEAD after second Ensure: %v", err)
	}
	if info2.ModTime() != info1.ModTime() {
		t.Errorf("HEAD mtime changed on second Ensure: %v -> %v", info1.ModTime(), info2.ModTime())
	}
}

func TestRedactTokenScrubsURL(t *testing.T) {
	in := "fatal: could not read from https://x-access-token:ghs_supersecrettoken123@github.com/owner/repo.git"
	got := redactToken(in)
	if strings.Contains(got, "ghs_supersecrettoken123") {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "https://x-access-token:<redacted>@github.com/owner/repo.git") {
		t.Errorf("redaction missing or malformed: %q", got)
	}
	// A plain string without a token URL must pass through untouched.
	plain := "no token here"
	if redactToken(plain) != plain {
		t.Errorf("redactToken altered untokenized string: %q", redactToken(plain))
	}
}

func TestFetchPRReturnsHeadSHA(t *testing.T) {
	tmp := t.TempDir()

	// Build upstream with one commit and a PR ref.
	upstream := filepath.Join(tmp, "upstream.git")
	must(t, exec.Command("git", "init", "--bare", upstream).Run())
	work := filepath.Join(tmp, "work")
	must(t, exec.Command("git", "init", work).Run())
	mustEnv(t, work, "git", "commit", "--allow-empty", "-m", "pr-commit", "--no-gpg-sign")
	mustEnv(t, work, "git", "remote", "add", "origin", upstream)
	mustEnv(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	// Capture the commit SHA from the working repo.
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	wantSHA := strings.TrimSpace(string(out))

	// Create refs/pull/1/head in the upstream bare repo.
	mustEnv(t, upstream, "git", "update-ref", "refs/pull/1/head", wantSHA)

	c := &Cache{BaseDir: filepath.Join(tmp, "cache"), Token: func(context.Context, int64) (string, error) { return "x", nil }}
	repoPath, err := c.Ensure(context.Background(), 1, "owner", "repo", "file://"+upstream)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	gotSHA, err := c.FetchPR(context.Background(), repoPath, 1, "file://"+upstream, 1)
	if err != nil {
		t.Fatalf("FetchPR: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("FetchPR SHA = %q, want %q", gotSHA, wantSHA)
	}
}

func TestWorktreeCreatesAndCleans(t *testing.T) {
	tmp := t.TempDir()

	upstream := filepath.Join(tmp, "upstream.git")
	must(t, exec.Command("git", "init", "--bare", upstream).Run())
	work := filepath.Join(tmp, "work")
	must(t, exec.Command("git", "init", work).Run())
	mustEnv(t, work, "git", "commit", "--allow-empty", "-m", "wt-test", "--no-gpg-sign")
	mustEnv(t, work, "git", "remote", "add", "origin", upstream)
	mustEnv(t, work, "git", "push", "origin", "HEAD:refs/heads/main")

	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(out))

	c := &Cache{BaseDir: filepath.Join(tmp, "cache"), Token: func(context.Context, int64) (string, error) { return "x", nil }}
	repoPath, err := c.Ensure(context.Background(), 1, "owner", "repo", "file://"+upstream)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	wtPath, cleanup, err := c.Worktree(context.Background(), repoPath, sha)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	// Verify worktree exists. A worktree from a bare repo has a .git file
	// (gitdir pointer), not a HEAD file, at its root.
	if _, err := os.Stat(filepath.Join(wtPath, ".git")); err != nil {
		t.Errorf("worktree .git not found: %v", err)
	}

	cleanup()

	// After cleanup the directory should be gone.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after cleanup: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func mustEnv(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}
