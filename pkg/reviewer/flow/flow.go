// Package flow provides orchestration for the review pipeline used by the
// GitHub PR worker. It is a sibling of pkg/reviewer/ctl and does not replace it;
// ctl handles the GitLab path. Task 13 will collapse the duplication once
// the GitLab integration is deleted.
package flow

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"reviewsrv/pkg/rest"
	"reviewsrv/pkg/reviewer"
	"reviewsrv/pkg/reviewer/ctl"
)

//go:embed summary.tmpl
var summaryTmplStr string

var summaryTemplate = template.Must(template.New("summary").Parse(summaryTmplStr))

// Runner abstracts the Claude CLI subprocess.
// *ctl.ExecClaudeRunner satisfies this interface directly.
type Runner interface {
	Run(ctx context.Context, prompt string) (*ctl.ClaudeResult, error)
}

// Issue mirrors the fields of rest.ReviewDraftIssue used by the Commenter.
// Using a local type keeps flow free of a direct dependency on the rest package
// in the interface boundary; callers convert at the call site.
type Issue struct {
	LocalID      string
	Severity     string
	Title        string
	Description  string
	File         string
	Lines        string
	IssueType    string
	SuggestedFix string
}

// Commenter posts review comments to a code hosting platform (GitHub, GitLab, …).
type Commenter interface {
	PostSummary(ctx context.Context, body string) error
	PostInline(ctx context.Context, iss Issue) error
	CleanupStale(ctx context.Context) error
}

// Input bundles everything Run needs.
type Input struct {
	// Prompt is the fully-rendered review prompt to pass to Claude.
	Prompt string
	// Runner executes Claude and returns the structured result.
	Runner Runner
	// Commenter posts the review comments to the hosting platform.
	Commenter Commenter
	// Dir is the working directory containing review.json and R*.md files.
	Dir string
	// ReviewURL is shown as "Full review →" in the summary comment.
	ReviewURL string
	// Model is the model alias used as a fallback in ModelInfo when modelUsage is absent.
	Model string
	// Log is an optional logger. When non-nil, non-fatal failures (e.g. HTML
	// generation) are logged at warn level. May be nil — Run never panics on it.
	Log *slog.Logger
}

// Result holds the outputs of a successful Run.
type Result struct {
	Draft *rest.ReviewDraft
}

// Run orchestrates a single review: run Claude → read review.json → comment → HTML.
// It does NOT upload to the server; the caller is responsible for uploading
// (separating IO concerns makes the worker easier to test).
func Run(ctx context.Context, in Input) (*Result, error) {
	// 1. Run Claude.
	claudeResult, err := in.Runner.Run(ctx, in.Prompt)
	if err != nil {
		return nil, fmt.Errorf("flow: run claude: %w", err)
	}

	// 2. Read review.json written by Claude into in.Dir.
	draft, err := ctl.ReadReviewJSON(in.Dir)
	if err != nil {
		return nil, fmt.Errorf("flow: read review.json: %w", err)
	}

	// 3. Merge cost/duration from Claude result into the draft.
	draft.Review.ModelInfo = claudeResult.ToModelInfo(in.Model)
	draft.Review.DurationMs = claudeResult.DurationMs

	// 4. Cleanup stale comments from previous runs.
	if err := in.Commenter.CleanupStale(ctx); err != nil {
		return nil, fmt.Errorf("flow: cleanup stale: %w", err)
	}

	// 5. Render and post summary comment.
	summaryBody, err := renderSummary(draft, in.ReviewURL)
	if err != nil {
		return nil, fmt.Errorf("flow: render summary: %w", err)
	}
	if err := in.Commenter.PostSummary(ctx, summaryBody); err != nil {
		return nil, fmt.Errorf("flow: post summary: %w", err)
	}

	// 6. Post inline comments for critical and high severity issues.
	for _, iss := range draft.Issues {
		if !isInlineSeverity(iss.Severity) {
			continue
		}
		flowIss := Issue{
			LocalID:      iss.LocalID,
			Severity:     iss.Severity,
			Title:        iss.Title,
			Description:  iss.Description,
			File:         iss.File,
			Lines:        iss.Lines,
			IssueType:    iss.IssueType,
			SuggestedFix: iss.SuggestedFix,
		}
		if err := in.Commenter.PostInline(ctx, flowIss); err != nil {
			return nil, fmt.Errorf("flow: post inline %s: %w", iss.LocalID, err)
		}
	}

	// 7. Generate HTML review report.
	mdFiles, err := ctl.FindMDFiles(in.Dir)
	if err != nil {
		return nil, fmt.Errorf("flow: find md files: %w", err)
	}
	if err := ctl.GenerateHTML(in.Dir, draft.Review.Title, mdFiles); err != nil {
		// Non-fatal: HTML generation failure should not abort a successful review.
		if in.Log != nil {
			in.Log.WarnContext(ctx, "generate HTML failed", "err", err)
		}
	}

	return &Result{Draft: draft}, nil
}

func isInlineSeverity(severity string) bool {
	return severity == reviewer.SeverityCritical || severity == reviewer.SeverityHigh
}

// summaryData holds template data for the summary comment.
type summaryData struct {
	TrafficLightEmoji string
	TrafficLightText  string
	Model             string
	CostUsd           float64
	Duration          string
	EffortMinutes     int
	Description       string
	Files             []summaryFile
	CriticalIssues    []summaryIssue
	ReviewURL         string
}

type summaryFile struct {
	ReviewType        string
	Summary           string
	IssuesSummary     string
	TrafficLightEmoji string
}

type summaryIssue struct {
	LocalID     string
	Title       string
	File        string
	Lines       string
	IssueType   string
	Description string
}

func renderSummary(draft *rest.ReviewDraft, reviewURL string) (string, error) {
	issuesByType := make(map[string]map[string]int)
	for _, iss := range draft.Issues {
		if issuesByType[iss.FileType] == nil {
			issuesByType[iss.FileType] = make(map[string]int)
		}
		issuesByType[iss.FileType][iss.Severity]++
	}

	data := summaryData{
		Model:       draft.Review.ModelInfo.Model,
		CostUsd:     draft.Review.ModelInfo.CostUsd,
		Duration:    formatDuration(draft.Review.DurationMs),
		Description: draft.Review.Description,
		ReviewURL:   reviewURL,
	}

	if draft.Review.EffortMinutes > 0 {
		data.EffortMinutes = draft.Review.EffortMinutes
	}

	var totalCritical, totalHigh, totalMedium int
	for _, counts := range issuesByType {
		totalCritical += counts[reviewer.SeverityCritical]
		totalHigh += counts[reviewer.SeverityHigh]
		totalMedium += counts[reviewer.SeverityMedium]
	}
	data.TrafficLightEmoji, data.TrafficLightText = trafficLightDisplay(totalCritical, totalHigh, totalMedium)

	for _, f := range draft.Files {
		counts := issuesByType[f.ReviewType]
		sf := summaryFile{
			ReviewType:    capitalizeFirst(f.ReviewType),
			Summary:       f.Summary,
			IssuesSummary: formatIssueCounts(counts),
		}
		fc, fh, fm := counts[reviewer.SeverityCritical], counts[reviewer.SeverityHigh], counts[reviewer.SeverityMedium]
		sf.TrafficLightEmoji, _ = trafficLightDisplay(fc, fh, fm)
		data.Files = append(data.Files, sf)
	}

	for _, iss := range draft.Issues {
		if !isInlineSeverity(iss.Severity) {
			continue
		}
		data.CriticalIssues = append(data.CriticalIssues, summaryIssue{
			LocalID:     iss.LocalID,
			Title:       iss.Title,
			File:        iss.File,
			Lines:       iss.Lines,
			IssueType:   iss.IssueType,
			Description: iss.Description,
		})
	}

	var buf bytes.Buffer
	if err := summaryTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return buf.String(), nil
}

func trafficLightDisplay(critical, high, medium int) (emoji, text string) {
	tl := reviewer.CalcTrafficLight(reviewer.IssueStats{
		Critical: critical,
		High:     high,
		Medium:   medium,
	})
	switch tl {
	case "red":
		return "🔴", "Red Light"
	case "yellow":
		return "🟡", "Yellow Light"
	default:
		return "🟢", "Green Light"
	}
}

func formatDuration(ms int) string {
	d := time.Duration(ms) * time.Millisecond
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatIssueCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "0"
	}

	var parts []string
	for _, sev := range reviewer.Severities {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
