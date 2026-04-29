package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const Marker = "<!-- reviewer -->"

type TokenFunc func(context.Context) (string, error)

type Comments struct {
	API   string // e.g. https://api.github.com
	Token TokenFunc
	HTTP  *http.Client
}

func (c *Comments) PostSummary(ctx context.Context, owner, repo string, pr int, body string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.API, owner, repo, pr)
	return c.postJSON(ctx, url, map[string]string{"body": ensureMarker(body)})
}

func (c *Comments) PostInline(ctx context.Context, owner, repo string, pr int, commitSHA, path string, line int, body string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments", c.API, owner, repo, pr)
	return c.postJSON(ctx, url, map[string]any{
		"body":      ensureMarker(body),
		"commit_id": commitSHA,
		"path":      path,
		"line":      line,
		"side":      "RIGHT",
	})
}

// CleanupStale deletes our previous review-comments without replies.
// Note: only the first 100 review comments are inspected (GitHub max per page);
// pagination is not implemented — acceptable while review threads stay small.
func (c *Comments) CleanupStale(ctx context.Context, owner, repo string, pr int) error {
	listURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments?per_page=100", c.API, owner, repo, pr)
	var comments []struct {
		ID          int64  `json:"id"`
		Body        string `json:"body"`
		InReplyToID *int64 `json:"in_reply_to_id"`
	}
	if err := c.getJSON(ctx, listURL, &comments); err != nil {
		return err
	}
	repliedTo := map[int64]bool{}
	for _, x := range comments {
		if x.InReplyToID != nil {
			repliedTo[*x.InReplyToID] = true
		}
	}
	for _, x := range comments {
		if !strings.Contains(x.Body, Marker) || repliedTo[x.ID] {
			continue
		}
		del := fmt.Sprintf("%s/repos/%s/%s/pulls/comments/%d", c.API, owner, repo, x.ID)
		_ = c.do(ctx, http.MethodDelete, del, nil, nil)
	}
	return nil
}

func ensureMarker(body string) string {
	if strings.Contains(body, Marker) {
		return body
	}
	return body + "\n\n" + Marker
}

func (c *Comments) postJSON(ctx context.Context, url string, payload any) error {
	return c.do(ctx, http.MethodPost, url, payload, nil)
}

func (c *Comments) getJSON(ctx context.Context, url string, out any) error {
	return c.do(ctx, http.MethodGet, url, nil, out)
}

func (c *Comments) do(ctx context.Context, method, url string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, method, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
