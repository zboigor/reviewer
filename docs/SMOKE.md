# End-to-End Smoke Test

Use this once after deploying the service-mode reviewer for the first time. It
exercises the full path: GitHub App → admin panel → trigger → worker → PR comment.

## Prerequisites

- Postgres running with the v0.2 schema (`make db && reviewsrv -patches docs/patches`).
- GitHub App created on github.com with these permissions:
  - Pull requests: read & write
  - Contents: read
- App installed on a sandbox repo you control.
- App private key (`.private-key.pem`) downloaded to disk.
- Anthropic API key in `ANTHROPIC_API_KEY` env var (used by Claude CLI).
- Claude CLI installed (`claude --version`).

## Steps

1. **Configure the server.**
   ```toml
   # cfg/local.toml
   [GitHub]
   AppID          = 123456                                  # numeric App ID from GitHub App page
   PrivateKeyPath = "/etc/reviewer/app.private-key.pem"
   APIBaseURL     = "https://api.github.com"

   [Repos]
   BaseDir = "/var/lib/reviewer/repos"

   [Worker]
   Enabled        = true
   Concurrency    = 1
   PollIntervalMs = 5000
   DefaultModel   = "opus"
   ```

2. **Start the server:**
   ```
   make run    # starts on :8075 by default
   ```
   Watch the logs — you should see `"worker-0"` starting (one line per worker), no panics.

3. **Start the frontend dev server (or use the embedded build):**
   ```
   make frontend-dev
   ```

4. **Sign in:** open `http://localhost:8075/vt/`, log in as `admin / 12345`
   (default seed credentials — change them in production).

5. **Create a project:** `/vt/projects/new`. Fill in:
   - **Title:** `<your sandbox repo name>`
   - **Project key:** any UUID (will be auto-generated if blank).
   - **GitHub Owner:** the GitHub user/org that owns the sandbox repo.
   - **GitHub Repo:** the repo name.
   - **GitHub Installation ID:** find this on the GitHub App's page under "Install App" → click your org → URL shows `installations/<id>`.
   - **Prompt:** select an existing prompt or create a new one.
   - **Status:** enabled.

6. **Open a PR** in the sandbox repo with a small change (e.g. add a typo to a README, or change one line of code).

7. **Trigger a review:** `/vt/reviews/new`. Paste the PR URL
   (`https://github.com/<owner>/<repo>/pull/<n>`). Click Trigger.

8. **Watch the worker logs.** Within ~5 seconds (poll interval), you should see:
   - `worker-0: claimed job ...`
   - Repo clone (first time per repo) — may take 10-60s for large repos.
   - `claude --print` invocation.
   - GitHub API calls posting comments.

9. **Verify on the PR.**
   - Open the PR on github.com.
   - You should see one summary comment ("Reviewer summary" with traffic-light, file breakdown, critical issues).
   - For each critical/high issue, an inline comment on the relevant file/line.

10. **Trigger again** with the same PR URL. Verify:
   - Old single-note inline comments without replies are deleted.
   - A new summary comment is posted.
   - New inline comments appear on the latest commit.

11. **Test the failure path.** Edit the project to set Installation ID to a wrong number. Trigger again. Verify:
    - The job ends up in `reviewJobs` with status='failed' or 'pending' (depending on retry count).
    - The reviewer logs show the GitHub auth failure.
    - The PR has no new comments.

## Troubleshooting

- **No comments appear and no logs:** worker may not be enabled. Check `cfg.Worker.Enabled = true` and the server log mentions "worker-0".
- **"GitHub App authentication failed":** App ID, private key path, or installation ID is wrong.
- **"project missing GitHub coordinates":** project's `githubOwner`, `githubRepo`, or `installationId` is blank.
- **"review missing PR number":** something's off in the Trigger flow — file an issue.
- **`TestClaimFIFO` flakes during `make test`:** old `reviewJobs` rows in `test-reviewsrv`. Run `psql -h localhost -p 6432 -U postgres -d test-reviewsrv -c 'TRUNCATE "reviewJobs" CASCADE;'`.

## Known Gaps (post-launch)

- Worker pool has no graceful shutdown — in-flight reviews may be killed on `make run` Ctrl+C. Track for follow-up.
- Summary comments accumulate on re-trigger. `CleanupStale` only deletes inline PR diff review-comments (the `/pulls/<n>/comments` endpoint); summary issue comments at `/issues/<n>/comments` are not cleaned up. Re-running a review on the same PR adds a fresh summary instead of replacing the previous one. Old summaries remain visible in the PR's main conversation thread.
- A successful job claim is not logged. Watching the worker logs, "no output for ~5 seconds after a trigger" usually means the worker polled and found no jobs — a successful claim only logs if `process` fails. Use the `reviewJobs` table directly to verify job state.
- Token mint has a brief double-mint window under high concurrency: two concurrent workers for the same `installationID` can both miss the cache and both mint a fresh token (one wasted call). Acceptable at `Concurrency = 1`; consider `singleflight` if scaling up.
- `capitalizeFirst` helper is duplicated between `pkg/reviewer/ctl/html.go` and `pkg/reviewer/flow/flow.go`. Track for cleanup.
- **Session caching for the worker is per-PR.** First trigger creates a Claude session; subsequent triggers on the same PR resume it (~90% token savings). The mapping lives in the `prSessions` table keyed by `(projectId, prNumber)`. To start a fresh session on a PR, run `psql -c 'DELETE FROM "prSessions" WHERE "projectId" = X AND "prNumber" = Y'` — the next trigger creates a new session. `createdAt` on the row records when the current session lineage started.
- `model_search.go` is a generated file with hand-edits for `GithubOwner`/`GithubRepo` filters. Future regeneration must preserve those (NOTE comments are in place).
