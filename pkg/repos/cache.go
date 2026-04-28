package repos

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// TokenFn returns an installation token for the given install ID.
type TokenFn func(ctx context.Context, installationID int64) (string, error)

// Cache manages bare-clone git repositories on disk, one per owner/repo pair.
// It is safe for concurrent use across different repositories.
type Cache struct {
	BaseDir string
	Token   TokenFn

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// tokenURLRe matches https URLs that embed an x-access-token credential so
// they can be replaced before being included in error messages or logs.
var tokenURLRe = regexp.MustCompile(`https://x-access-token:[^@]+@`)

// redactToken replaces token values in URLs so they don't leak into errors/logs.
func redactToken(s string) string {
	return tokenURLRe.ReplaceAllString(s, "https://x-access-token:<redacted>@")
}

// Ensure clones (bare) or reuses the cached repo and returns its on-disk path.
// upstreamURL is the https URL (without token); for tests pass a file:// URL
// and a Token function returning any non-empty string.
func (c *Cache) Ensure(ctx context.Context, installationID int64, owner, repo, upstreamURL string) (string, error) {
	repoPath := filepath.Join(c.BaseDir, owner, repo+".git")
	repoLock := c.lockFor(repoPath)
	repoLock.Lock()
	defer repoLock.Unlock()

	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(repoPath), 0o750); err != nil {
			return "", err
		}
		url, err := c.urlWithToken(ctx, installationID, upstreamURL)
		if err != nil {
			return "", err
		}
		if err := runGit(ctx, "", "clone", "--bare", url, repoPath); err != nil {
			// Clean up partial clone so a retry can start fresh.
			_ = os.RemoveAll(repoPath)
			return "", fmt.Errorf("clone: %w", err)
		}
	}
	return repoPath, nil
}

// FetchPR fetches refs/pull/<pr>/head into the cached bare repo and returns the head SHA.
func (c *Cache) FetchPR(ctx context.Context, repoPath string, installationID int64, upstreamURL string, pr int) (string, error) {
	repoLock := c.lockFor(repoPath)
	repoLock.Lock()
	defer repoLock.Unlock()

	url, err := c.urlWithToken(ctx, installationID, upstreamURL)
	if err != nil {
		return "", err
	}
	refspec := fmt.Sprintf("+refs/pull/%d/head:refs/pull/%d/head", pr, pr)
	if err := runGit(ctx, repoPath, "fetch", "--depth=200", url, refspec); err != nil {
		return "", fmt.Errorf("fetch pr: %w", err)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", fmt.Sprintf("refs/pull/%d/head", pr)).Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse refs/pull/%d/head: %w", pr, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Worktree creates a temporary worktree at the given SHA and returns (path, cleanup).
// The caller must call cleanup() when done to remove the worktree and its directory.
func (c *Cache) Worktree(ctx context.Context, repoPath, sha string) (string, func(), error) {
	repoLock := c.lockFor(repoPath)
	repoLock.Lock()
	defer repoLock.Unlock()

	wt, err := os.MkdirTemp("", "reviewer-wt-*")
	if err != nil {
		return "", nil, err
	}
	if err := runGit(ctx, repoPath, "worktree", "add", "--detach", wt, sha); err != nil {
		_ = os.RemoveAll(wt)
		return "", nil, fmt.Errorf("worktree add: %w", err)
	}
	cleanup := func() {
		// Re-acquire the per-repo lock: cleanup runs in a separate critical
		// section from the original Worktree call.
		l := c.lockFor(repoPath)
		l.Lock()
		defer l.Unlock()
		_ = runGit(context.Background(), repoPath, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
	}
	return wt, cleanup, nil
}

// urlWithToken returns the URL unchanged for file:// URLs; for https:// URLs it
// embeds an x-access-token credential so git can authenticate without prompting.
func (c *Cache) urlWithToken(ctx context.Context, installationID int64, upstreamURL string) (string, error) {
	if strings.HasPrefix(upstreamURL, "file://") {
		return upstreamURL, nil
	}
	if c.Token == nil {
		return "", fmt.Errorf("repos.Cache: Token function not set")
	}
	tok, err := c.Token(ctx, installationID)
	if err != nil {
		return "", err
	}
	// https://x-access-token:<token>@github.com/owner/repo.git
	rest := strings.TrimPrefix(upstreamURL, "https://")
	return fmt.Sprintf("https://x-access-token:%s@%s", tok, rest), nil
}

// lockFor returns the per-repo mutex, creating it on first access.
func (c *Cache) lockFor(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	if l, ok := c.locks[key]; ok {
		return l
	}
	l := &sync.Mutex{}
	c.locks[key] = l
	return l
}

// runGit runs git with the provided arguments. When dir is non-empty the
// command is run inside that directory via git -C <dir>.
// GIT_TERMINAL_PROMPT=0 prevents git from blocking on credential prompts.
// Tokens embedded in URLs are redacted from error messages.
func runGit(ctx context.Context, dir string, args ...string) error {
	fullArgs := args
	if dir != "" {
		fullArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		safeArgs := make([]string, len(fullArgs))
		for i, a := range fullArgs {
			safeArgs[i] = redactToken(a)
		}
		const maxStderr = 1024
		text := redactToken(string(out))
		if len(text) > maxStderr {
			text = text[:maxStderr] + "...(truncated)"
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(safeArgs, " "), err, text)
	}
	return nil
}
