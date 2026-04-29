# reviewer

AI-powered code review platform using Claude. Triggers reviews from the admin panel, posts inline GitHub PR comments, and stores results in PostgreSQL.

## Features

- **Multi-project support** with configurable prompts per project
- **5 review types**: architecture, code, security, tests, operability
- **Severity levels**: critical, high, medium, low with traffic light system (red/yellow/green)
- **Manual triggering** — paste a GitHub PR URL in the admin panel; worker pool picks it up
- **GitHub App integration** — installation tokens are minted server-side, comments are posted as the App identity
- **GitHub PR inline comments** — critical and high issues posted directly in the diff with cleanup on re-runs
- **Worker pool** — Postgres-backed job queue with `FOR UPDATE SKIP LOCKED`; concurrency configurable
- **Repo cache** — bare clones reused across reviews; per-PR worktrees
- **Session caching for reviewctl** — Claude prompt cache reused across local re-runs (~90% token savings)
- **Auto-migrations** — pgmigrator integrated as Go library, runs SQL patches on server startup
- **Slack notifications** for completed reviews
- **VT admin panel** for managing projects, prompts, users, Slack channels, and triggering reviews
- **REST + JSON-RPC API** with auto-generated TypeScript clients and OpenRPC schema

## Architecture

```
Admin user (browser, /vt/)
  -> POST /v1/vt/ rpc Review.Trigger { prUrl }
       -> validate user session
       -> parse PR URL, look up project by GitHub repo
       -> insert review row + reviewJobs row (status=pending)
  -> Worker pool claims pending jobs (FOR UPDATE SKIP LOCKED)
       -> mint GitHub App installation token
       -> ensure bare clone of repo on disk
       -> fetch refs/pull/<n>/head, create per-PR worktree
       -> build prompt from project config
       -> claude --print --output-format json
       -> parse review.json + R*.md files
       -> post GitHub PR summary comment + inline comments for critical/high issues
       -> generate HTML report
       -> Slack notification for completed review
```

## Prerequisites

- Go 1.25+
- PostgreSQL
- Node.js 20+ (for frontend build)

## Quick Start

```bash
# 1. Initialize config files
make init

# 2. Edit configuration
#    Set database credentials in Makefile.mk and cfg/local.toml

# 3. Create and seed database
make db

# 4. Install frontend dependencies and build
make frontend-install
make frontend-build

# 5. Run the server
make run
```

Default admin credentials: `admin` / `12345`

The server starts at `http://localhost:8075`. The review UI is available at `/reviews/`, the admin panel at `/vt/`.

## Docker Compose

Run locally with Docker Compose — builds the image from source and initializes the database automatically:

```bash
docker compose up -d
```

This starts PostgreSQL (exposed on port `6432`) and the reviewer app on `http://localhost:8080`. The database schema and seed data (`docs/reviewsrv.sql`, `docs/init.sql`) are applied on first run.

To rebuild the image after code changes:

```bash
docker compose up -d --build
```

To stop and remove containers (add `-v` to also remove the database volume):

```bash
docker compose down
```

## Configuration

Configuration file: `cfg/local.toml`

```toml
[Server]
Host    = "localhost"
Port    = 8075
IsDevel = true
BaseURL = "http://localhost:8075"

[Database]
Addr     = "localhost:5432"
User     = "postgres"
Database = "reviewsrv"
Password = ""
PoolSize = 5

[Sentry]
DSN         = ""
Environment = ""
```

## API Endpoints

### REST

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/prompt/:projectKey/` | Get review prompt for a project |
| POST | `/v1/upload/:projectKey/` | Create a new review |
| POST | `/v1/upload/:projectKey/:reviewId/:reviewType/` | Upload a review file |

### JSON-RPC

| Path | Description |
|------|-------------|
| `/v1/rpc/` | Review API (projects, reviews, issues, feedback) |
| `/v1/rpc/doc/` | Review API documentation (SMDBox) |
| `/v1/vt/` | Admin API (users, projects, prompts, Slack channels, task trackers) |
| `/v1/vt/doc/` | Admin API documentation (SMDBox) |

TypeScript clients are auto-generated at `/v1/rpc/api.ts` and `/v1/vt/api.ts`.

## Review Types and Severity

**Review types:** `architecture`, `code`, `security`, `tests`, `operability`

**Severity levels:** `critical`, `high`, `medium`, `low`

**Traffic light system:**
- Red: 1+ critical OR 2+ high issues
- Yellow: 1+ high OR 3+ medium issues
- Green: all other cases

## reviewctl

`reviewctl` is a Go CLI for **local prompt iteration** — it runs Claude against
the current working directory and writes review.json + R*.md files plus an HTML
report. The production review path is via the admin panel; reviewctl is for
authoring/debugging prompts on your laptop.

```bash
reviewctl review    # Fetch prompt -> Claude -> upload to reviewsrv
reviewctl upload    # Upload local review.json + R*.md to server
reviewctl version   # Print version
```

Key flags: `--key`, `--url`, `--public-url`, `--model`, `--dir`, `--verbose`,
`--session` (Claude session reuse), `--continue`. All flags have env variable
equivalents. See `reviewctl --help` for details.

```bash
make build-reviewctl   # Build reviewctl binary
```

## Auto-migrations

The server can apply SQL patches automatically on startup using pgmigrator (integrated as Go library):

```bash
reviewsrv -config config.toml -patches /patches
```

Patches are stored in `docs/patches/*.sql` with `YYYY-MM-DD-description.sql` naming. The Docker image includes patches at `/patches/`. Docker Compose runs with `--patches` by default.

## Set up a project

1. **Install the Reviewer GitHub App** on the GitHub org that owns your target repos. The App needs:
   - Pull requests: read & write
   - Contents: read
2. **Configure the server** with the App's credentials in `cfg/local.toml`:
   ```toml
   [GitHub]
   AppID          = 123456                                 # numeric App ID
   PrivateKeyPath = "/etc/reviewer/app.private-key.pem"    # path to PEM file
   APIBaseURL     = "https://api.github.com"

   [Repos]
   BaseDir = "/var/lib/reviewer/repos"                     # bare-clone storage

   [Worker]
   Enabled        = true
   Concurrency    = 1
   PollIntervalMs = 5000
   DefaultModel   = "opus"
   ```
3. **Create a project in the admin panel** (`/vt/projects/new`) and set:
   - **GitHub owner / repo** (e.g. `synthesized-io / tdk`)
   - **Installation ID** — visible in the App's "Configure" page on GitHub after install
4. **Trigger reviews** from `/vt/reviews/new` by pasting a PR URL. The worker pool picks the job up within `PollIntervalMs` and posts results to the PR.

## Administration

### URL Access Control

When deploying behind a reverse proxy, URLs should be split by access level:

**Public (available within the closed network):**

| Path | Description |
|------|-------------|
| `/reviews/` | Review results UI |
| `/vt/` | Admin panel |
| `/v1/rpc/` | Review JSON-RPC API |
| `/v1/vt/` | Admin JSON-RPC API |

**Internal (reviewctl only, must not be exposed externally):**

| Path | Description |
|------|-------------|
| `/v1/upload/` | Review upload endpoint (reviewctl) |
| `/v1/prompt/` | Prompt fetch endpoint (reviewctl) |

Example nginx configuration:

```nginx
# Public URLs — accessible within the closed network
location /reviews/ { proxy_pass http://reviewer:8075; }
location /vt/       { proxy_pass http://reviewer:8075; }
location /v1/rpc/   { proxy_pass http://reviewer:8075; }
location /v1/vt/    { proxy_pass http://reviewer:8075; }

# Internal URLs — accessible only from reviewctl (local runs)
location /v1/upload/ { deny all; }
location /v1/prompt/ { deny all; }
```

## Development

```bash
make run              # Run server in dev mode
make build            # Build server binary
make build-reviewctl  # Build reviewctl CLI
make frontend-dev     # Run frontend dev server (Vite)
make frontend-build   # Build frontend (main + admin)
make generate         # Generate RPC/VT code
make lint             # Run golangci-lint
make test             # Run tests with coverage
make tools            # Install dev tools
make mod              # Tidy and vendor Go modules
```

## Tech Stack

**Backend:** Go, Echo, zenrpc, go-pg, Prometheus, Sentry

**Frontend:** Vue 3, TypeScript, Tailwind CSS, Vite, Headless UI

## License

MIT
