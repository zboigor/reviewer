package flow

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"reviewsrv/pkg/githubapp"
)

// GitHubCommenter implements Commenter by wrapping *githubapp.Comments.
type GitHubCommenter struct {
	Client   *githubapp.Comments
	Owner    string
	Repo     string
	PRNumber int
	HeadSHA  string
}

func (g *GitHubCommenter) PostSummary(ctx context.Context, body string) error {
	return g.Client.PostSummary(ctx, g.Owner, g.Repo, g.PRNumber, body)
}

func (g *GitHubCommenter) PostInline(ctx context.Context, iss Issue) error {
	line := firstLine(iss.Lines)
	if line == 0 || iss.File == "" {
		// Fallback: post as issue comment instead of inline.
		return g.Client.PostSummary(ctx, g.Owner, g.Repo, g.PRNumber, formatIssue(iss))
	}
	return g.Client.PostInline(ctx, g.Owner, g.Repo, g.PRNumber, g.HeadSHA, iss.File, line, formatIssue(iss))
}

func (g *GitHubCommenter) CleanupStale(ctx context.Context) error {
	return g.Client.CleanupStale(ctx, g.Owner, g.Repo, g.PRNumber)
}

// firstLine extracts the first line number from "42-45" or "42".
// Returns 0 if s is empty, not parseable, or non-positive. Treating 0 and
// negative values as "no valid position" lets callers fall back to summary
// comments instead of attempting an invalid inline post.
func firstLine(s string) int {
	if s == "" {
		return 0
	}
	parts := strings.SplitN(s, "-", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// formatIssue renders the per-issue comment body.
func formatIssue(iss Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 **%s. %s** (%s)\n\n", iss.LocalID, iss.Title, iss.IssueType)
	fmt.Fprintf(&b, "%s\n", iss.Description)
	if iss.SuggestedFix != "" {
		fmt.Fprintf(&b, "\n**Suggested fix:**\n%s\n", iss.SuggestedFix)
	}
	fmt.Fprintf(&b, "\n%s\n", githubapp.Marker)
	return b.String()
}
