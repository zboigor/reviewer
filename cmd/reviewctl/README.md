# reviewctl

Go CLI orchestrator for AI code review. Single binary: prompt → Claude → upload → HTML.

## Subcommands

| Command | Description |
|---------|-------------|
| `reviewctl review` | Full cycle: fetch prompt → Claude → parse → upload → HTML |
| `reviewctl upload` | Upload local `review.json` + `R*.md` to server |
| `reviewctl version` | Print version |

## Flags & Environment Variables

| Flag | Env Variable | Default | Description |
|------|-------------|---------|-------------|
| `--key` | `$PROJECT_KEY` | *required* | Project key (UUID) |
| `--url` | `$REVIEWSRV_URL` | *required* | Reviewer server URL |
| `--public-url` | `$REVIEWSRV_PUBLIC_URL` | *falls back to `--url`* | Browser-facing base URL for links |
| `--model` | `$REVIEW_MODEL` | `opus` | Claude model |
| `--dir` | `$REVIEW_DIR` | `.` | Working directory with review files |
| `--verbose` | `$REVIEW_VERBOSE` | `false` | Verbose output |
| `--session` | — | — | Claude session ID for `--resume` (reuses prompt cache) |
| `--continue` | — | `false` | Continue last Claude session (auto-detect) |
| `--source-branch` | — | — | Source branch |
| `--target-branch` | — | — | Target branch |
| `--commit` | — | — | Commit SHA |
| `--author` | — | — | PR author |
| `--pr-title` | — | — | PR title |
| `--external-id` | — | — | External ID |

## Usage

### Local Run

```bash
export PROJECT_KEY="your-project-uuid"
export REVIEWSRV_URL="https://reviewer.example.com"

# Full review: prompt → Claude → upload → HTML
reviewctl review

# Resume previous session (reuses prompt cache, ~90% cheaper)
reviewctl review --session <session-id>

# Upload only (after manual Claude run)
reviewctl upload
```

## Output Files

| File | Description |
|------|-------------|
| `review.json` | Structured review data (created by Claude) |
| `R1.*.md` — `R5.*.md` | Review files: architecture, code, security, tests, operability |
| `review.html` | HTML artifact with syntax highlighting and mermaid diagrams |
| `claude-output.json` | Raw Claude CLI output for diagnostics |

## Build

```bash
make build-reviewctl          # builds bin/reviewctl
go test ./pkg/reviewer/ctl/... # run tests
```

## Docker CI Image

```dockerfile
FROM vmkteam/reviewer:latest AS source

FROM node:20-alpine
RUN apk add --no-cache git bash curl
RUN npm install -g @anthropic-ai/claude-code
COPY --from=source /reviewctl /usr/local/bin/reviewctl
WORKDIR /workspace
```
