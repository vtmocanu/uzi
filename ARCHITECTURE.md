# Architecture

uzi's current MVP is a docker-compose stack of three always-on services (`web`, `api`, `db`) plus one opt-in, profile-gated service (`agent`, a per-user worker — PRD #4), with three trust boundaries: the inbound edge at nginx, the API's outbound calls to the forge (GitLab), and the API's connection to each user's worker. This document describes that shape; see [docs/auth-design.md](docs/auth-design.md) for the auth surface, and [docs/proc-hardening.md](docs/proc-hardening.md) for the worker/agent process-isolation detail the Secrets and Guardrails sections below summarize.

## Services

```
                  laptop (host network)
                          │
                   127.0.0.1:8080
                          │
┌─────────────────────────┼──────────────────────────────────┐
│ compose network         ▼                                  │
│                 ┌───────────────┐                           │
│                 │      web      │  nginx-unprivileged        │
│                 │  (React SPA)  │  serves static build,      │
│                 └───────┬───────┘  proxies /api/* same-origin│
│                         │                                    │
│                         ▼                                    │
│                 ┌───────────────┐                           │
│                 │      api      │  Go (chi), distroless      │
│                 │  (auth API)   │                             │
│                 └───────┬───────┘                            │
│                         │                                    │
│                         ▼                                    │
│                 ┌───────────────┐                           │
│                 │      db       │  postgres:17               │
│                 │               │  named volume: pgdata      │
│                 └───────────────┘                            │
└──────────────────────────────────────────────────────────────┘
```

- **`web`** (`web/Dockerfile`) — a Vite/React SPA built at image build time and served by `nginxinc/nginx-unprivileged`. Its `nginx.conf` does two jobs: serve the built static assets, and reverse-proxy `/api/*` to `api:8080`. The SPA and API therefore share one origin (`http://127.0.0.1:8080`) — no CORS configuration exists anywhere in the stack.
- **`api`** (`api/Dockerfile`) — a statically linked Go binary (`chi` router) on `gcr.io/distroless/static-debian12`, running as `nonroot`. Serves `/api/auth/*`, `/api/admin/*`, `/api/agent-templates/*`, `/api/me/secrets/*`, `/api/worker/*` (the worker protocol, PRD #4), `/api/ws`, `/api/health`, `/api/version` (unauthenticated build info — version, founding date, uptime, plus the source commit, build time and commit count on a release image; read by the SPA footer and by the `uzi` CLI, [PRD #175](prds/done/175-build-info-popover.md)), among others. Runs its own DB migrations and builtin-template reconciliation at startup before it starts accepting traffic (see below).
- **`db`** — stock `postgres:17`, digest-pinned in `docker-compose.yml`. Data lives in the named volume `pgdata`; it is not a bind mount, so it survives container recreation but is bound to the Docker volume store on this host.
- **`agent`** (`agent/templates/<name>/Dockerfile`, opt-in via `docker compose --profile agent up`) — one worker per user: a Node 24 + git + bash container (not distroless — the Claude Agent SDK's Bash tool needs a real shell) that connects *outbound only* and executes runs. The image is built from a curated **template** selected by `WORKER_TEMPLATE` (PRD #18: `base`, or heavy-dep variants like `jvm`). Its outbound reach is `api`, the forge (git clone/fetch/push over HTTPS, the PAT injected as a Basic-auth header, not a session), **plus**, when a run has tier-1 tool packages to provision, the **nix substituters** the devbox engine fetches from (`cache.nixos.org` and any configured extras) — provisioning runs before the SDK in a secret-scrubbed subprocess (PRD #18 M3, [docs/worker-setup.md](docs/worker-setup.md#tool-provisioning)). The substituters are the one NEW egress PRD #18 adds to that set; the nix store lives on its own `agentnix` volume (at `/nix`) so it is a first-run-only fetch. Every worker also ships the `docker` CLI on `PATH` (PRD #83: `docker-cli`/`-compose`/`-buildx` baked into every template; no `dockerd`, no host `docker.sock`, anywhere in the image) — inert until an optional **docker-capable** worker adds a rootless Docker-in-Docker sidecar: its own container (compose) or pod sidecar (k8s), sharing only the daemon socket. The sidecar mounts none of the worker's join token, `/data`, or `/nix`, so a container the agent launches (`docker run -v ...`) can bind-mount none of them — the universal separate-mount-namespace invariant that holds on every track. Standing the sidecar up opens one genuinely new outbound set, pulled container images from a registry, which this PRD does not filter — that is [PRD #50](prds/50-llm-egress-proxy.md)'s job. See [Agent runtime](#agent-runtime-workers-runs-live-view) below, [docs/worker-setup.md](docs/worker-setup.md) for the operator procedure, and [docs/worker-docker.md](docs/worker-docker.md) for the docker sidecar.

All images are pulled/built by digest or pinned tag in `docker-compose.yml`, not floating `latest`.

## Trust boundaries

- **Only `web` publishes a port**, and only to loopback: `127.0.0.1:8080` in `docker-compose.yml`. `api` and `db` have no `ports:` mapping — they are reachable exclusively over the private compose network, by each other, and are invisible from the host's other network interfaces or from any other machine.
- **nginx is the sole entry point and the sole source of truth for the client's identity.** `web/nginx.conf` sets `X-Forwarded-For: $remote_addr` (not `$proxy_add_x_forwarded_for`), which *overwrites* any `X-Forwarded-For` a client sent rather than appending to it. `api` then only trusts that header when the direct connection comes from a `TRUSTED_PROXIES` CIDR (the compose network's private ranges, by default) — see [docs/auth-design.md](docs/auth-design.md#rate-limiting-and-the-x-forwarded-for-trust-model). Net effect: a client cannot spoof its apparent IP by sending its own `X-Forwarded-For`.
- **The session cookie is the only credential**, and it is `HttpOnly` — never readable by any JavaScript running in the SPA, including an XSS payload. See [docs/auth-design.md](docs/auth-design.md) for the full cookie/CSRF/revocation design.
- **`GET /api/version` is the one endpoint that publishes a runtime fact, and it does so on purpose.** It is unauthenticated *and* unrate-limited: mounted directly under `r.Route("/api")` with only `Recoverer` + `RequestID` above it, pinned `noLimiter` by `route_limiter_mounts_test.go`, and in k8s reachable through `deploy/chart/templates/web-ingress.yaml`, which by default publishes the SPA origin at path `/` with no auth annotation (both are chart values, so a hardened override could narrow them; the enforced property is the route mount, not the chart). Most of the body is public by construction — the version *is* the chart's image tag, the commit is in the repo. `uptime_seconds` is not: it describes this **process**, not this image. It is published deliberately ([PRD #175](prds/done/175-build-info-popover.md), severity Low) because process age is worth real debugging time and discloses no identity, topology or schedule. Two things follow. "It carries no secret" is not the whole test — a field can be secret-free and still be a runtime disclosure. And **any new surface for this body republishes uptime by default**: an `/about` page or a signed-out footer, both named as follow-ups in that PRD, would widen the audience with nothing in the code to notice. Re-decide it there rather than inheriting this decision.

## Request flow: an authenticated API call

1. Browser sends `GET /api/admin/users` with cookies `uzi_auth` (JWT) and `uzi_csrf` attached automatically (same-origin).
2. nginx (`web`) proxies the request to `api:8080/api/admin/users`, setting `X-Forwarded-For: $remote_addr`, `X-Real-IP`, `X-Forwarded-Proto`.
3. `chi` routes it through `RequireAuth` (`api/internal/middleware/auth.go`): validates the JWT signature/expiry, loads the user by ID from `db` (one PK read), rejects if inactive or if `token_version` has been bumped since the token was issued. Past the halfway point of the TTL, it also re-issues both cookies (rolling refresh).
4. Then through `RequireAdmin`: rejects unless `user.IsAdmin`.
5. The handler (`api/internal/handler/admin.go`) queries `db` via `sqlc`-generated, `pgx`-backed queries and returns JSON.

State-changing requests (POST/PATCH) additionally run CSRF validation inside `RequireAuth` before any handler logic executes.

## Startup and migrations

`api`'s entrypoint (`api/cmd/server/main.go`) does not assume the DB is ready or up to date:

1. Loads config (`config.Load`), including the `UZI_SECRET_KEY` boot guard (`secretbox.LoadKey`): boot fails here, before any DB connection is attempted, if the key is missing, malformed, or a low-entropy placeholder (e.g. an all-zero key).
2. Waits for `db` to accept connections (bounded retry loop; compose's `depends_on: condition: service_healthy` on `db`'s `pg_isready` healthcheck already gates container start, this is a second, in-process guard).
3. Runs all pending **goose** migrations, embedded in the binary via `go:embed` (`api/internal/store/migrate.go`, `api/internal/store/migrations/`) — no separate migration step or tool needed at deploy time.
4. Opens the `pgx` connection pool, then reconciles the builtin agent templates through it and, **after them**, the builtin skills (`store.ReconcileBuiltinTemplates` → `store.ReconcileBuiltinSkills`, see below): idempotent, so this is safe to run on every boot. That order is a requirement, not a convention — a builtin skill's default allocation resolves its agent template *by name* and is seeded only on the boot that inserts the skill — so the second call takes a token the first returns, and swapping the two blocks does not compile.
5. Builds the `secretbox.Box` from the already-validated key.
6. Only then starts the HTTP server.

`web` depends on `api`'s healthcheck (`GET /api/health`, which itself pings the DB pool), so `docker compose up` brings the stack up in the correct order: `db` healthy → `api` migrated and healthy → `web` serving.

## Forge integration

uzi's second surface connects each user to a git forge (**GitLab, Forgejo, and GitHub**, behind a forge-generic interface) so the board has real work to show. The Forgejo driver (PRD #65) was the second driver; the GitHub driver (PRD #238) is the third, and — unlike Forgejo — landed with **no interface method-set change at all**, evidence the abstraction ADR-0065 built now genuinely scales to a new forge as "a driver file plus its per-forge seam arms." Every milestone but the last landed dark; the go-live flip (PRD #238's last milestone) now has `handler/forge.go` **advertise and accept `github`**, so a GitHub classic-PAT connection is connectable through the product. Neither driver adds a new service — everything below lives inside `api` — but each does confirm the same second trust boundary: `api` makes authenticated *outbound* calls to a third party (the forge) on top of the inbound boundary at the nginx edge described above. See [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md), [docs/forgejo-bot-setup.md](docs/forgejo-bot-setup.md), and [docs/github-bot-setup.md](docs/github-bot-setup.md) for the per-forge operator/user procedure, [adr/0065-forgejo-driver.md](adr/0065-forgejo-driver.md) and [adr/0238-github-driver.md](adr/0238-github-driver.md) for the second- and third-driver design records, and the PRDs (`prds/done/2-forge-integration-kanban.md`, `prds/done/65-forgejo-support.md`, `prds/done/238-github-forge-support.md`) for the full rationale.

### Forge abstraction

`api/internal/forge` defines the `Forge` interface (`VerifyToken`, `ListProjects`, `ListLabels`, `EnsureLabels`, `ListIssues`, `UpdateIssueLabels`, `UpdateIssueDescription`, the four CI reads, plus the guardrail reads `ProjectRole`/`DefaultBranchProtection`) and a neutral domain vocabulary (`BotIdentity`, `Project`, `Label`, `Issue`, `Role`, `BranchProtection`); `forge.New` selects a driver by `forge.Type` — **`gitlab.go`, `forgejo.go`, or `github.go`** — so no other package ever imports a driver directly. Every driver call goes through an `*http.Client` bounded by `FORGE_HTTP_TIMEOUT` (`timeoutClient` in `forge.go`) — closing the untimeouted-`http.DefaultClient` wart the `multica` inspiration carries — and every returned error is passed through a `redactor` (`redact.go`) that scrubs the PAT and any `Authorization`/`PRIVATE-TOKEN`/`token`-scheme header value before the error can reach a log line or an HTTP response body.

The Forgejo driver (`code.gitea.io/sdk/gitea`, Forgejo ≥16.0.0 — [ADR-65](adr/0065-forgejo-driver.md) for why both) proved the abstraction was more than Go-deep by finding the three places it was **not**: (1) the worker held a second, un-abstracted GitLab client, now a minimal TS forge seam (`agent/src/forge.ts`, `GitLabClient`/`ForgejoClient`/`GitHubClient`); (2) the web reconstructed forge URLs by string surgery, now a per-card/run `forge_type` DTO field mapped only at `web/src/lib/forgeNoun.ts` (`forgeNoun`/`forgePlatform`, one Go twin in `slacksvc/notifier.go`, one CLI twin in `api/cmd/uzi/render.go`); (3) each forge stored its pipeline status verbatim, so `api/internal/pipelinestatus` is the one Go-side classifier that folds all three vocabularies — the domain twin of `web/src/lib/pipelineBadge.ts`, kept in sync by `TestMirrorsWebPipelineBadge`. Merge-permission is now modelled on all three forges (`BranchProtection.WriteRoleCanMerge`/`BotCanMerge`); the drivers **report** it, and **enforcement is implemented** — [PRD #66](prds/done/66-guardrail-enforcement.md) refuses a run whenever the bot could push or merge to the default branch, at repo-enable, at run creation, and at claim, live and fail-closed, with an admin-only per-repo override for the deliberate, audited exception.

The GitHub driver (`github.com/google/go-github/v90`, github.com only, classic PAT — [ADR-238](adr/0238-github-driver.md) for the design) filled the same four per-forge seams: a `privcheck.requiredScopesFor(github)` scope rule (exactly `{repo}`, with a `workflow`-scoped token refused as over-privilege — a deliberate CI-integrity boundary), the Actions two-field (`status`/`conclusion`) status fold into `pipelinestatus`/`pipelineBadge`, the `forgeNoun`/`forgePlatform` "Pull Request"/"PR"/"#" vocabulary, and `agent/src/forge.ts`'s `GitHubClient` (whose one shared-base change was widening the worker's duplicate-PR detection to a **driver-declared** status set, since GitHub signals a duplicate PR with 422 rather than GitLab/Forgejo's 409). GitHub's branch-protection guardrail is materially weaker than Forgejo's: a write-role bot can read GitHub's newer *rulesets* but not classic branch protection, so on a classically-protected repo `BranchProtection` gains an additive `ProtectionUnverified` field rather than a fabricated safe/unsafe answer — see [ADR-238](adr/0238-github-driver.md) for the accepted limitation and the fail-closed requirement it places on PRD #66.

### Issue comments as untrusted worker input (PRD #381)

A worker's context is no longer just an issue's title + description: `api/internal/forge`'s `Forge` interface gained a fourth read, `ListIssueComments`, implemented across all three drivers — the GitLab driver drops forge **system** notes (`Note.System`), Forgejo and GitHub have no such notes to drop — and every driver normalizes to **oldest-first** regardless of the forge's native sort. At run creation, `workersvc.createRun` snapshots the issue's comments into a new nullable `runs.issue_comments` JSONB column via `buildIssueCommentsSnapshot` (`api/internal/workersvc/issue_comments.go`), carried structured on the worker claim next to the description. The snapshot excludes **uzi's own bot-authored comments**, matched against the connection's stored `bot_forge_user_id` — an unknown/zero bot id omits comments entirely rather than risk leaking uzi's own status chatter back into the prompt (see the security note below) — and is bounded to 200 newest comments and 32 KiB of body bytes, with a `truncated` flag when the cap clips the thread. The LEAD's plan prompt renders the snapshot in `agent/src/prompt.ts` (`buildIssueCommentsContext`) immediately after `<issue_description>`, under a per-prompt CSPRNG nonce fence — the same discipline the file already applies to cross-run memory and job logs — with uzi-owned `author`/`timestamp` headers and comment bodies as untrusted data; a comment-less run's prompt is byte-for-byte unchanged from before this landed. The live `get_issue` forge tool (`handler/worker_forge.go`, `assembleForgeIssueComments`) applies the same bot/system filtering and oldest-first ordering with its own 200-item/32 KiB cap, so a mid-run agent pull sees the same shape as the initial snapshot. See [prds/done/381-worker-reads-issue-comments.md](prds/done/381-worker-reads-issue-comments.md) for the full design and decision log.

**Security note.** This widens the injection surface without opening a new trust boundary: a comment body is attacker-influenceable free text exactly like the issue description already was (see [adr/0246-trusted-repo-instructions.md](adr/0246-trusted-repo-instructions.md)) — the multi-author worst case of the same untrusted class, since each comment is independently attacker-authored and a body could otherwise embed a forged closing tag or a spoofed `author: admin (approved)` line. The per-prompt CSPRNG nonce fence defeats that breakout class the same way it already does for every other attacker-authored block: no comment body can predict the nonce, so none can forge the block's close delimiter or spoof the uzi-owned author/timestamp header around it. The numeric forge user id used for the bot filter is read server-side only and is never surfaced to the agent. The bot-exclusion filter and its zero/unknown-id fail-safe (skip the feature rather than risk exposing uzi's own comments) are what keep this from becoming a feedback loop where the agent reacts to uzi's own status notes.

### Bot PATs, encrypted at rest

`api/internal/secretbox` is a generic AES-256-GCM box (12-byte random nonce prepended to the ciphertext) — deliberately not PAT-specific, since the same utility is slated for per-user Anthropic OAuth tokens in the next PRD. `config.Load` calls `secretbox.LoadKey("UZI_SECRET_KEY")` at boot and refuses to start if it is missing, not valid base64, not exactly 32 bytes, or a low-entropy placeholder (e.g. all-zero) — the same refuse-to-start stance as `JWT_SECRET`. The resulting `*secretbox.Box` is constructed once in `main.go` and handed to `forgesvc.Service`, which seals a PAT into `forge_connections.token_ciphertext` on connect and opens it on every forge call thereafter. **Rotating `UZI_SECRET_KEY` invalidates every stored PAT** — there is no re-encrypt path in this MVP, so a rotation means every user reconnects.

### SSRF guard

The server making authenticated outbound calls to a `base_url` supplied at connection time is a classic SSRF surface (cloud metadata endpoints, internal services, loopback). `config.Config.ForgeAllowedBaseURLs` (from `FORGE_ALLOWED_BASE_URLS`, default `https://github.com`) is the only set of base URLs a connection may target; `config.NormalizeForgeBaseURL` requires `https` and canonicalizes to `scheme://host[:port]` before every allowlist comparison, and boot fails if the parsed list is empty or any entry is malformed or non-`https`. The Settings → Forge UI offers only this set (`GET /api/forge/config`), so a user cannot even attempt a free-text URL.

### Data model: forge as source of truth

Migration `00002_forge.sql` adds four tables, all scoped down to `forge_connections.user_id` by FK cascade:

- **`forge_connections`** — one row per (user, forge_type, base_url); carries the encrypted PAT and the verified bot identity.
- **`repos`** — projects discovered via the bot's membership list, keyed by the forge's stable numeric project id (not the path, which can be renamed); upserted with `enabled=false` on every listing call so enable/disable always has a row to target.
- **`board_columns`** — ordered label names per repo; the implicit Open (no column label) and Closed (issue `state`) columns are never stored.
- **`issues`** — a *cache*, never authoritative. uzi's own board state is limited to column configuration; every other field is overwritten from the forge on each sync. `has_prd_link` is computed at fetch time from the issue description (regex match on a `prds/*.md` reference) and stored as a bool — the description itself is never persisted.

### Sync engine

`api/internal/forgesvc.Service` is shared by the HTTP handlers and the background poller: it builds a `Forge` driver from a stored (encrypted) connection, runs the PRD-link check, and implements `IncrementalSync`/`FullSync` against an `IssueStore` interface (narrowed for unit testing against a fake store and a mocked `Forge`, without a live database).

`api/internal/poller.Engine` (`main.go`) is started as a single background goroutine alongside the HTTP server. Each tick, for every enabled repo, it either runs an incremental pull — two fetches, the PRD-labelled set (`state=all`) plus PRD #102 M6's additive open, no-label set, each bounded by its own high-water mark in `forgesvc.Marks{PRD, Open}` (never the client clock; the max `updated_at` that fetch itself returned) — or, every `FORGE_RECONCILE_EVERY`-th tick (and always on a repo's first poll after being enabled), a full reconcile: fetch both complete sets with no lower bound, upsert everything, and evict cache rows absent from their union. Eviction is the only way to observe de-labeling, closing-with-label-removed, or deletion, which an `updated_after`-filtered query structurally cannot report. Per-repo state (`marks forgesvc.Marks`, poll count) lives only in the `Engine`'s in-memory map — a disabled repo simply drops out and a re-enabled one starts over with a fresh full reconcile.

Writes are forge-first: a board move (`POST /api/repos/:id/issues/:iid/move`) calls `UpdateIssueLabels` before touching the cache, and only updates the cache on success — a failed forge write leaves the card where it was (snap-back), never an optimistically-moved card the forge disagrees with.

**PRD-link patch** (`forgesvc.SyncPRDLinkPatches`, PRD #72 M5) rides the same tick and is the one place uzi rewrites an issue's *description*. An `issue` run whose lead reports it moved the PRD file the issue links stores that path plus a pending marker on the run; once the run's MR is observed `merged`, the pass reads the live description (`GetIssue` — the cache has no description column), substitutes only the occurrences matching that path, writes back via `UpdateIssueDescription`, and settles the marker. Edge-triggered, so it never re-fires and never fights a human's later edit; `closed`-unmerged and superseded-while-open settle without patching, so an abandoned branch is not polled forever. Two properties are load-bearing: it is scoped `kind = 'issue'` (a `self_improve` run's issue is a reused backlog container whose description is a live control document), and the **targets come from the run's own queue-time issue snapshot, never from the agent's declaration** — the agent says where the file went, not which link to touch, so it can only ever redirect a link the issue already carried. That bound is the whole claim and it is easy to overstate: targeting is by *basename* against the snapshot's links, so on an issue that links several PRDs the declaration does pick which one is repointed, and nothing verifies the move happened. Bounded to one issue's own links, basename-matched, `prdpath.Validate`-passing — description integrity, not security; tightening it is a design question (uzi has no notion of *the* PRD when an issue links several) rather than a fix. It deliberately does not reuse `mr_watch`'s candidate prefilter: that requires `i.state = 'opened'`, and a merge closes the issue through the MR's `Closes #N` before `SyncMRStates` runs, so the candidate would be evicted deterministically rather than occasionally.

Shutdown ordering matters here specifically because of the poller: `main.go`'s `run()` calls `srv.Shutdown` to drain in-flight HTTP requests, cancels the root context (stopping the poller's next tick), then `pollerWG.Wait()`s for any in-flight sync to finish — all *before* the deferred `pool.Close()` runs, so a mid-tick database query never races the pool shutting down underneath it.

### CI status + fix agent (PRD #6)

The same poller tick that syncs issues also syncs **pipeline status** (`forgesvc.SyncPipelines`, after the issue sync — no second loop, no new interval). For each enabled repo it resolves the **watched refs** — the repo's `default_branch` plus the branches of its recent agent runs (non-terminal, or terminal-with-an-MR within `CI_WATCH_RUN_WINDOW`, capped at `CI_WATCH_MAX_REFS`) — and caches the latest pipeline per ref in `pipeline_statuses` (a latest-per-`(repo, ref)` cache, upserted every tick, reconcile-evicted like the issues cache). A run branch is read via `LatestMRPipeline(mr_iid)` (which catches detached and merged-results pipelines the branch-ref query misses); the default branch via `LatestPipeline`. The forge stays the source of truth; a ref with no CI honestly caches nothing. `CI_WATCH_MAX_REFS=0` disables the whole feature. The four pipeline read methods (`LatestPipeline`/`LatestMRPipeline`/`ListPipelineJobs`/`JobLogTail`) live behind the same `Forge` interface + PAT-redaction discipline as every other forge call.

The board, board header, and repos list render the cached status as badges (`web/src/lib/pipelineBadge.ts` collapses GitLab's status set to five tones). A failed watched pipeline offers a **Fix CI** button that `POST`s to `/api/repos/:id/ci-fix-runs`: the server re-checks the cache shows the ref failed, freezes a **failure snapshot** (up to `CI_FIX_MAX_JOBS` failed jobs, each with a `CI_FIX_LOG_TAIL_BYTES` log tail, PAT- + `glpat-*`-redacted) onto a new run, and queues it.

A `ci_fix` run rides PRD #4's run machinery as a second run **kind** (`runs.kind`, with a nullable `issue_iid` + a `runs_kind_shape` CHECK; the failed pipeline is snapshotted onto the run so it stays self-contained). The worker (`agent/`) diagnoses from the snapshot — job logs are framed as quoted **untrusted evidence**, and every PRD #4 guardrail layer holds verbatim — behind the same plan gate as an issue run. The plan is either a fix (implemented on `ci-fix/pipeline-{id}` for a default-branch failure, or the existing agent branch for a run-branch failure, then pushed + MR'd by the worker) or an explicit **`not_code`** verdict (infra/flaky/secret/runner problem: the run completes with the diagnosis and no MR).

**Verification** ("uzi verifies its work"): the pipeline sync stamps a `ci_fix` run's `fix_verdict` — `verified` when its post-fix pipeline passes, `fix_failed` when it fails — keyed on `runs.branch` (the fix branch, not the failed ref, which differ for a default-branch fix) with an `observed pipeline id > snapshot pipeline id` guard so the original failing pipeline never false-stamps. See [docs/configuration.md](docs/configuration.md#ci-status-integration-prd-6) for the env knobs and the documented residual risks (poll-based staleness, third-party secrets in logs, merge-result false-positives).

**Automatic fixes (PRD #71)** extend this with an opt-in per-user trigger (`users.ci_autofix_enabled`, default off): the same poller tick, via a `CIAutoFix` detector that runs after the pipeline sync, auto-queues the identical `ci_fix` run — auto-approved, skipping the plan gate — when a watched **agent-owned MR branch**'s pipeline fails and its owner opted in. `main`, the repo's default branch, and any non-MR ref are never eligible; only a branch an agent run itself produced is. A loop guard bounds it so a persistently red pipeline can't retry forever: a cap on automatic attempts per branch (`CI_AUTOFIX_MAX_ATTEMPTS`), plus an early halt when a fix attempt's pipeline fails again with the same failure signature as the one before it — either way it stops and notifies rather than retrying on its own, and the manual **Fix CI** button remains the escape hatch. A code-only fix pushes automatically like any auto-approved run; a fix whose diff touches the CI config (`.gitlab-ci.yml`, `.gitlab/`, the project's configured CI config path) is instead parked for human approval, with a fail-closed worker push guard as backstop for an auto-approved plan that turns out to touch those paths anyway. See [docs/ci-autofix.md](docs/ci-autofix.md) for the user-facing behavior and knobs.

### Per-user rate limiting on forge-proxying endpoints

The endpoints that call out to the forge on a user's behalf (`.../verify`, `.../projects`, `.../issues/:iid/move`, `.../sync`) run behind a second instance of the same fixed-window `mw.Limiter` PRD #1 uses for auth endpoints, but keyed **per user** (`PerUserMiddleware`) rather than per client IP, and budgeted separately (`FORGE_RATE_LIMIT_MAX`/`FORGE_RATE_LIMIT_WINDOW`, default 30/minute) from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`. This bounds how hard one uzi user's actions can hit the upstream forge, independent of the local-network IP-sharing caveat already documented for the auth limiter in [auth-design.md](docs/auth-design.md#accepted-limitations).

## Agent templates

An agent template (`agent_templates` table) is the definition of one role an
agent can play: name, description, an optional model override, an optional
tools allowlist, and a prompt body. It is not itself an agent; it is the
recipe a later release renders into a running one.

- **`builtins/` is the single source of truth.** Twelve builtin roles — the
  `lead` orchestrator (`model: opus`) plus eleven subagents (`coder`,
  `reviewer`, `auditor`, `tester`, `architect`, `documenter`, `fact-checker`,
  `spec-keeper`, `researcher`, `ux-designer`, `web-ux`) — are Go-embedded from `api/internal/agenttmpl/builtins/*.md`,
  parsed at package `init()`. This directory is independent of this repo's own
  `.claude/agents/*.md` dev-team roster (PRD #17): that roster is free to
  drift and product changes never touch it. At every boot,
  `store.ReconcileBuiltinTemplates` inserts any builtin row missing from the
  DB and never touches one that already exists, so an admin's edits to a
  builtin survive restarts, and future releases can add or upgrade builtins
  without a SQL seed that can't be re-run; a boot-time warning is logged if a
  non-builtin row already occupies a builtin's name (e.g. a custom `lead`
  template blocks the seed).
- **The `lead` is the main thread, not a subagent.** The worker
  (`agent/src/agents.ts`) partitions templates by name (`LEAD_NAME_RE`,
  matching `lead`/`orchestrator`) and routes the matched template's
  `prompt_body`/`model` into the run's main SDK thread instead of registering
  it as an invokable subagent. Model precedence for that main thread: the run
  owner's per-user default model (`users.default_model`, set from Settings,
  carried through the claim payload) → the `lead` template's `model` → the
  SDK/Anthropic-account default. A subagent's own template `model`, when set,
  always wins for that subagent; unset, it inherits the resolved main-thread
  model. See [docs/worker-model.md](docs/worker-model.md).
- **Scopes + per-user ownership (PRD #18).** Each template has a `scope`
  (`builtin`/`global`/`user`) and, for user scope, a `user_id`, mirroring the
  skills model. Builtin and global are visible to everyone and admin-managed; a
  `user` template is visible to and editable by only its owner. Reads and
  writes are scope-authorized per row (not a blanket `RequireAdmin`): any user
  creates/edits/deletes their own templates, builtin/global management stays
  admin-only, and a non-owner never learns a private template exists (404, not
  403). `is_builtin` is retained as a compat column a CHECK ties to
  `scope='builtin'`. Two partial-unique name indexes (shared names unique
  across builtin+global; per-user names unique) let a user own a name that
  collides with a shared one; `lead`/`orchestrator` are reserved so no
  API-created template (global or user) can take a lead name — the invariant
  behind the worker's "a claim never carries two lead-matching templates" pin.
  This still closes the hole where any user could rewrite the shared prompts.
- **Allocation + claim filtering (PRD #18).** `agent_template_allocations`
  decides which templates ride each run: a global-default layer (admin,
  `user_id NULL`, always enabled) plus a per-user `enabled` overlay. Seeded
  with no empty-means-all cliff — every builtin/global gets an explicit default
  row (at migration, on the reconciler's first insert of a builtin, and on a
  global's creation), so absence of a row is always a deliberate removal. A
  claim delivers only the run owner's **resolved** set (`ListClaimAgentTemplates`:
  the overlay wins, else the global default, else dropped) — builtin∪global
  defaults ± the owner's overlay + the owner's own allocated user templates,
  never another user's rows. A user template whose name collides with a
  builtin/global is dropped from the claim (**shared precedence**), so the
  worker's name-keyed subagent map never has the curated builtin displaced (a
  deliberate divergence from skills' body-precedence; surfaced in the UI as a
  `shadowed` badge). The lead is a normal template in the set — a user may
  disable it, and the worker degrades to its hardcoded guardrail lead prompt.
- **Renderer.** `api/internal/agenttmpl/render.go` turns a template into
  Claude Code's subagent Markdown (fixed-order YAML frontmatter, `tools` as an
  inline comma-separated string, `tools`/`model` omitted when they inherit).
  It is a pure function with no DB dependency; parse/validity tests (not a
  byte-match against `.claude/agents/`, dropped with the source-of-truth
  split above) guard the embedded builtins directly. `GET
  /api/agent-templates/:id/rendered` serves this Markdown directly; nothing
  in this release writes it to a filesystem or spawns anything from it (that
  is a later release's job). See
  [docs/agent-templates.md](docs/agent-templates.md).
- **Validation.** `name` is kebab-case (`^[a-z0-9]+(-[a-z0-9]+)*$`), unique
  within its scope namespace (the two partial-unique indexes above), not a
  reserved lead name for an API create, and immutable after creation (it is the
  subagent's filename and identity; renaming means creating a new template and
  deleting the old one; builtins are never renamed). `description` and `prompt_body` are required and
  non-empty; `description`, `model`, and each tool name must not contain a
  newline, carriage return, or other control character (they each render on
  a single frontmatter line). A template is rejected if its description or
  prompt body contains what looks like a complete Anthropic token (a
  high-confidence `sk-ant-...` match); the UI separately warns, without
  blocking, on looser patterns so legitimate text stays savable.
- **API surface.** All endpoints require authentication (session + CSRF);
  writes are scope-authorized per row (PRD #18), not blanket-admin: `GET
  /api/agent-templates` (list, viewer-scoped), `GET /api/agent-templates/:id`
  (one, viewer-scoped), `GET .../rendered`, `GET|PUT
  /api/agent-templates/allocations` (the per-template toggle view + replace-set
  write: an admin global-default half + the caller's overlay half), `POST
  /api/agent-templates` (create; `scope` global⇒admin or user⇒owner, blank⇒global
  for back-compat; a reserved lead name is rejected), `PUT .../:id` (update; name
  and scope are ignored, immutable), `DELETE .../:id` (409 on a builtin), `POST
  .../:id/reset` (400 on a non-builtin). Edits are last-write-wins — no
  optimistic-concurrency check in this release — but every row records
  `updated_by` and `updated_at` so concurrent edits are at least attributable.

## Agent skills

A skill is a named Markdown playbook (progressive disclosure: name +
description sit cheaply in an agent's context; the body loads on demand) that
attaches to agent templates rather than existing on its own. Full rationale
in `prds/done/16-agent-skills.md` (Solution Overview, Technical Design, Trust
model); this section is the map. User-facing usage is
[docs/skills.md](docs/skills.md).

- **Storage.** `skills` (`scope` ∈ `builtin`/`global`/`user`; `name` is
  kebab-case and immutable; `body` is the raw SKILL.md content below the
  frontmatter, which is synthesized at delivery, never stored) and
  `agent_skill_allocations` (`user_id NULL` for a shared/admin-managed
  allocation, non-`NULL` for a specific user's private overlay; unique on
  `(template_id, skill_id, COALESCE(user_id, sentinel))`, so a shared
  allocation and a user's overlay allocation of the same skill to the same
  template coexist as two distinct rows, no surrogate PK needed).
  `repos.repo_skills_enabled` is the opt-in flag for repo-borne skills,
  below. Builtins are seeded/repaired by the same reconciler pattern as agent
  templates (editable, resettable, never deletable), Go-embedded from
  `api/internal/skilltmpl/builtins/` (no `.claude/skills/` mirror, per the
  builtins convention above) — `prd-lifecycle`.
- **Default allocations** (PRD #72 M2). A builtin with no allocation row
  reaches *nobody* — not its scoped subagents and not the lead either, since
  the union `ListRunSkillAllocations` builds is what the lead receives. So
  each builtin carries a default target list (a Go-side map in `skilltmpl`
  keyed by skill name, deliberately not frontmatter: `skilltmpl.Parse`
  rejects every unknown key and that strictness is worth keeping), seeded in
  the **same transaction** as the insert and **only** on the boot that
  inserts the skill — `ReconcileBuiltinTemplates`' `n > 0` rule, for its
  reason: a default an admin later removes stays removed. The targets are
  agent-template *names*, so the template reconciler must run first (see
  [Startup](#startup-and-migrations)), and a zero-row seed warns rather than
  failing silently.
- **Read authz is deliberately not the agent-templates pattern.** Templates
  are all-shared; skills are not. Every read (`GET /api/skills*`) returns
  builtin ∪ global ∪ the caller's own user skills; admins additionally see
  other users' private skills (they can read the DB anyway). Allocation reads
  follow the same rule: a template's shared allocations plus only the
  caller's own overlay, never another user's.
- **Claim payload delta** (`api/internal/workersvc/claim.go`): `ClaimPayload.skills` is
  the run's deduplicated skill union (`{name, description, body}`), assembled
  server-side per the claiming user (shared allocations ∪ that user's
  overlay, across every template, since every template ships in every
  claim); `ClaimPayload.skills_dropped` carries assembly-time drops
  (`{name, reason}` — `shadowed` or `over_limit`). Each `ClaimAgent.skills`
  is that template's allocated skill names. `ClaimRepo.skills_enabled` mirrors
  `repos.repo_skills_enabled`. `ClaimConfig.skill_max_bytes` /
  `skills_max_per_run` carry the server's configured caps
  (`SKILL_MAX_BYTES`/`SKILLS_MAX_PER_RUN`) so the worker enforces the same
  limits the server assembled against, with no hardcoded drift. Name-collision
  precedence at assembly is user > global > builtin; the loser is dropped as
  `shadowed`. The server never writes `run_messages` for these drops — it
  hands the worker the list, and the worker (which owns the gapless per-run
  `seq`) logs each one as a run message.
- **Worker delivery** (`agent/src/skills-plugin.ts`,
  `agent/src/sdk-executor.ts`). Every claim (including a resume) rebuilds a
  local SDK plugin directory **outside the clone**, a sibling of the
  worktree: `.claude-plugin/plugin.json` plus `skills/<name>/SKILL.md` per
  surviving skill, with `name`/`description` frontmatter synthesized as
  quoted, fully-escaped YAML scalars (a frontmatter-injection guard covering
  every YAML metacharacter class, not just newlines). The SDK's top-level
  `skills` option is always sent as an explicit list — omitting it is not
  "skills off" — set to the run's full plugin-qualified union; the `lead`
  template runs on the main thread (not a subagent), so this union is also
  its only allocation surface. On an **own-template** run each subagent's
  `AgentDefinition.skills` scopes it to its own allocated skills, re-filtered
  to what actually survived materialization. On a **repo-source** run
  (`agent/src/agents.ts` `subagentsFromTemplates`, PRD #72) there are no
  template rows to allocate against, so every repo subagent receives the run's
  whole surviving set instead — the same all-templates rule repo skills follow.
- **Repo skills** (`agent/src/repo-skills.ts`), opt-in and default off. Only
  when `ClaimRepo.skills_enabled`, the worker enumerates
  `<clone>/.claude/skills/*/SKILL.md` after checkout, keeping only the `name`
  and `description` frontmatter keys (every other key, e.g. `allowed-tools`,
  is stripped — that is the security point) and re-synthesizing them through
  the same escaped-YAML materializer as delivered skills. Repo skills carry
  no allocation, so a surviving one attaches to **every** template in the
  run; they rank at the lowest precedence (a name collision with any
  delivered skill drops the repo skill) and are the first evicted if the
  combined set exceeds `skills_max_per_run`. This is the **only** clone-borne
  configuration the worker ever reads — no hooks, no settings, no commands,
  no `CLAUDE.md` — and it is why the toggle exists per repo: a repo's
  `.claude/` is exactly the config class `settingSources: []` (see Guardrail
  layers, below) is built to keep closed, so loading even this much requires
  the repo owner (or an admin) to vouch for that repo's review discipline.
- **Trust boundary.** `settingSources: []` stays `[]` in every configuration,
  with or without repo skills enabled — the plugin channel
  (`plugins: [{type: 'local', ...}]`) is a separate SDK option, independent
  of `settingSources`, so this delivery mechanism never has to loosen that
  isolation. A hostile repo skill still cannot push code (the worker alone
  holds the PAT), still hits the `PreToolUse` deny-hook, and still cannot
  load hooks, settings, or commands from the repo — only its own SKILL.md
  bodies, stripped to name + description, at the bottom of the precedence
  order.

## Secrets: per-user credentials at rest

`user_secrets` is a generic, kind-keyed table (`kind` currently only
`anthropic_token`, `CHECK`-constrained so a new kind is one migration, not a
new table) holding AES-256-GCM-sealed per-user secrets. A user may hold
**several** secrets of one kind, each under a label they chose, exactly one of
which is flagged `is_default` — the one every unbound consumer resolves
(PRD #104). It held exactly one per `(user, kind)` until migration `00077`
dropped the unique constraint that said so. The
`secretbox` package (`api/internal/secretbox/`) wraps `Seal`/`Open` around a
single 32-byte key that `config.Load` validates from `UZI_SECRET_KEY` at boot
(refusing to start if it is missing, malformed, or a low-entropy placeholder)
and then builds, already validated, into one `*secretbox.Box` shared by every
handler (see [docs/configuration.md](docs/configuration.md)). This is the
platform's one shared secret-at-rest mechanism: any feature that needs to
store a per-user credential (starting with the Anthropic token this PRD adds)
seals it with the same key before it reaches Postgres, so a DB dump alone
never yields a plaintext secret, and rotating the key invalidates every
stored secret across every feature at once, not just one.

The API around it is deliberately minimal, and **every read is metadata only**:
`GET /api/me/secrets` returns `id`, `kind`, `label`, `is_default` and
timestamps, never a value. There is no reveal endpoint and no admin path to
another user's secret value.

Since PRD #104 a user may hold **several named credentials of one kind**, so
the write surface is id-keyed: `POST /api/me/secrets/anthropic_token` creates
a named token, `PATCH …/{id}` renames, re-defaults or replaces one value, and
`DELETE …/{id}` removes one. The original kind-path routes survive as
deprecated compatibility aliases over the caller's *default* token (`PUT`
rotates-or-creates it; `DELETE` removes it, and 409s once more than one token
exists, because the path no longer names a single row). Exactly one token per
`(user, kind)` is the default — a partial unique index plus a per-user
advisory lock around every mutation, since no index can enforce "at least
one". Workers and the judge lane may each *bind* a token, resolved per claim
so a rebind lands on the next claim with no restart; the binding is a
composite FK on `(user_id, id)`, which makes cross-user binding
unspellable rather than merely rejected. Every write path stays cookie-only:
a Bearer-reachable mint would let a stolen CLI token replace a user's
credentials. See [docs/anthropic-token.md](docs/anthropic-token.md) for the
user-facing flow and `prds/done/104-named-anthropic-tokens.md` for the rationale.

## Claude rate-limit visibility (PRD #53)

A background engine (`api/internal/usagepoller`, cloned from the same
`Boot`+`Run`+ticker shape as the self-improvement engine) ticks on
`UZI_USAGE_POLL_INTERVAL` and, for each Anthropic token it can currently
open, asks Anthropic for that account's 5-hour/7-day
rate-limit windows — Anthropic's free usage endpoint first, falling back to
a 1-token header probe only when the endpoint refuses the credential — and
upserts one gauge row per **token** (`anthropic_rate_limits`, keyed on
`user_secret_id` since PRD #104, no history, D4). Per token rather than per
user because that is the unit Anthropic actually caps: two credentials are
two independent budgets, and one merged bar would describe neither.
Two read endpoints (`GET /api/me/rate-limits`, self-scoped; `GET
/api/admin/rate-limits`, every user) serve the same frozen DTO — an array of
per-token readings — and the SPA renders them as meters in three places: a
Settings card, a sidebar micro-meter, and an Admin → Rate limits table. See
[prds/done/53-rate-limits.md](prds/done/53-rate-limits.md) (especially its Design
Decisions) for the full rationale, including the vault-locked staleness
rule and the failure/backoff semantics; user-facing behavior is in
[docs/rate-limits.md](docs/rate-limits.md).

## Agent runtime: workers, runs, live view

uzi's third surface is the one that acts: a per-user worker container claims
queued work, drives a Claude Agent SDK session against a cloned repo, and opens
an MR — never touching `main`. This adds one optional service (`agent`, above)
and a third trust boundary: `api` now also accepts *inbound* calls from each
user's worker (the reverse of the forge boundary, where `api` calls *out*).
Full design rationale lives in the PRD (`prds/done/4-agent-runtime-workers.md`,
especially its Decision Log); this section is the map. See
[docs/run-cost.md](docs/run-cost.md) for why a run on this runtime costs less
than the same task worked by a local Claude Code agent-team session, and
whether the cheaper run is as good.

```
browser ──WS+REST──▶ web (nginx) ──▶ api (Go)  ◀──HTTP (compose) / HTTPS (k8s), poll/claim/report── agent (worker, per user)
                                       │  ▲                                        │
                                       ▼  │ decrypted per-run secrets              ▼
                                      db  └── forge (GitLab): issues, MRs     repo clone + worktrees
```

### Server/worker trust boundary

The worker never receives inbound connections; every call is worker → `api`
(`POST/GET /api/worker/*`), authenticated by a Bearer join token whose sha256
the server looks up (`mw.RequireWorker`) — never a session cookie, so there is
no CSRF step on this path. The token is shown once, at issuance (Settings →
Workers), and only its hash is ever stored.

**TLS on this hop is deferred on compose and MET on Kubernetes** (PRD #58 M4).
The join token gives *authentication* on both; transport encryption depends on
where the worker runs:

- **Compose**: still plain HTTP, and still fine for the reason it always was —
  the worker is another container on the same private network. It becomes a real
  gap the moment a worker runs off-network (a laptop, a remote VM) with
  `UZI_API_URL` pointed at a public `api` over plain HTTP. Flagged here so it is
  never silently assumed solved by "outbound-only".
- **Kubernetes**: closed. The `api` gains an **optional second listener in-process**
  (`API_TLS_CERT`/`API_TLS_KEY`, default `:8443`), and hosted workers plus the
  worker controller dial it over `https://`, verifying a cert-manager-issued CA
  that they pin *exclusively*. Note the mechanism, because an earlier version of
  this section named the wrong one: it is **not** a TLS-terminating ingress in front
  of `api`. Workers dial the `api` **directly**, so an ingress was never in their
  path and would not have encrypted this hop at all. The plain listener stays for
  `web`'s nginx and the kubelet probes, and the two are separate ports on purpose —
  a NetworkPolicy admits the worker namespace to the TLS one and nothing else.

Two properties of that listener that are easy to get backwards, both enforced in
code rather than intended: it serves **only** `/api/worker/*` + `/api/controller/*`
(not the full router), and it **strips `X-Forwarded-For`** — on a cluster
`TRUSTED_PROXIES` is the pod CIDR, so a worker pod is a trusted proxy by
construction and could otherwise forge its own rate-limit key. See
[docs/configuration.md](docs/configuration.md) §The optional TLS listener.

`api` remains the **sole holder of the encryption keys** (`UZI_SECRET_KEY`, see
above) throughout: it decrypts a user's Anthropic token and forge bot PAT and
hands both to the worker **only inside a run's claim response**
(`POST /api/worker/runs/claim`) — never persisted server-side in plaintext,
never logged (the same redaction discipline as the forge integration). *Which*
Anthropic credential is resolved is a **per-claim** decision across four lanes,
of which `workersvc.claimSecretID` decides two: a `self_improve` run follows
the owner's judge-lane binding, and any other run-lane claim follows the
claiming worker's **bind mode**. The other two never reach it — a judge run
forks to `assembleJudgeClaim` *before* `claimSecretID` and follows the owner's
judge binding, and the chat lane calls the opener directly with no override, so
it always resolves the owner's default. Because the token rides the claim
rather than the worker, re-pointing a worker is complete server-side — no
restart, no re-minted join token.

Since PRD #111 the bind mode is three-valued — `default`, `pinned` or `auto`
— and `auto` ranks the owner's opted-in tokens by rate-limit headroom
(`api/internal/autoselect`, fed by the same gauge the meters render). The
ranker is **pure**: the store query ends at `[]autoselect.Candidate` and
everything after it is arithmetic over already-fetched rows with an injected
clock, which is what keeps the ranking testable without a database. It sits
*behind* `claimSecretID` rather than beside it, so **the ranker runs in exactly
one place**: `assembleClaim` never ranks and `openAnthropic` never learns of it
at all — PRD #104's R4 is that three copies of credential resolution drift and
a wrong fallback spends the wrong account silently. `assembleClaim` *does* know
the choice came from the selector, because D14's retry lives where the open
happens: it re-resolves once on the non-auto binding when a selector-named
credential will not decrypt. Auto never fails a run: an empty, unmeasurable or
undecryptable pool falls back to the owner's default and records why.

Every claim now also **records the credential it spent** on the run
(`runs.anthropic_secret_id` + a `anthropic_secret_label` snapshot + the mode
that chose it), written after a successful open so the recorded id is
provably the opened one. The label is snapshotted rather than joined because
the composite FK nulls the id when a token is deleted — the run still names
the account it billed afterwards. That record is the attribution join
`run_usage` (PRD #40) could never make; see
`prds/done/111-auto-select-anthropic-token.md` for the ranking's rationale.

### Run lifecycle

One `runs` row is the unit of work; an issue can accumulate several over its
life (a DB partial unique index enforces at most one non-terminal run per
issue). A row enters `queued` from one of three origins: the manual **Start
run** button (`IssueView.tsx` → `POST /api/repos/{id}/runs`); **autopilot**
(PRD #19), event-driven off a forge label; and, since PRD #241, the
**scheduler** — time-driven. `api/internal/schedsvc`'s engine is a
single-instance background actor modeled on the selfimprove engine (a wake
ticker over the durable `run_schedules.next_fire_at` due-gate, `FOR UPDATE
SKIP LOCKED` claim, a `Boot()` immediate tick so a fire missed across a
restart happens promptly on the next wake rather than waiting a full
cadence — never a backfill of the cadences it missed). A due schedule fires
through the exact `workersvc.CreateRun`/`CreateAutopilotRun` seam autopilot
uses, so PRDLESS gating, the fresh-label forge fetch, active-run dedup, and
the usage-limit park all apply exactly as for a manual start; the exception
is its **ad-hoc prompt** target, which — like `ci_fix` — has no issue to
seam through, so it lands via a dedicated INSERT as a new `prompt` run kind
(`runs.kind`, beside `chat`/`ci_fix`/`self_improve`/`judge`/`issue`):
repo-ful, issue-less, `schedule_id`-keyed for dedup (no issue to key
`HasActiveRunForIssue` on), and MR-opening on the `ci_fix` shape. A **sweep**
target fans out over the oldest open issues matching its label; its
`max_issues` cap counts runs *started*, not candidates matched — a candidate
it can't start is flagged and the fire backfills from the next eligible issue
within a bounded scan window (`fireSweep`, issue #416), so a stale ineligible
issue at the head no longer under-fills every fire. See
[docs/scheduling.md](docs/scheduling.md) and
`prds/done/241-schedule-runs.md`.

`task` (PRD #400, `uzi handoff`/`uzi task`) is the **seventh** `runs.kind`,
alongside `issue`/`ci_fix`/`chat`/`judge`/`self_improve`/`prompt` above. Like
`prompt` it adds no new service — it rides the same worker/run machinery —
but it is CLI-only and MR-less by default: the CLI (not the worker) pushes
the user's own HEAD to a server-named `uzi/task/<run-id>` branch with the
user's own git credentials, then dispatches the run so the worker can clone
that branch, work the inline context, and push its commits back to it, with
no forge issue and no merge request unless `--mr` is passed. See
[prds/done/400-uzi-handoff.md](prds/done/400-uzi-handoff.md) for the full design and
[docs/handoff.md](docs/handoff.md) / [docs/cli.md](docs/cli.md#uzi-handoff-ephemeral-branch-scoped-task-runs)
for usage. Status is a linear state machine:

```
queued → claimed → running ⇄ awaiting_input (ask_user, PRD #88) → awaiting_approval ⟲ (revise, PRD #41) → running → completed
                                                                                                                   → failed
   ↳ (worker dies) → re-queued, up to RUN_MAX_REQUEUES → failed
   ↳ (Anthropic usage limit, opt-in) → limit_wait → queued, up to RUN_LIMIT_MAX_WAITS → failed
   ↳ cancel with no live poller → cancelled directly (server-side)
```

`running ⇄ awaiting_input` can fire twice over — once **pre-run**, ending the
planning turn with a question instead of a plan, and again **mid-run**, at any
point in the implement ⇄ review loop after approval — and the two resolve
differently, so the single edge above is a simplification. **Mid-run**
explicitly re-reports `running` (the next implement/review iteration does
so) before continuing the loop it paused. **Pre-run** does not: nothing is
reported between the answer and the planning turn's next move, and if that
move is a plan, the run goes straight to `awaiting_approval` — the literal
chain in the diagram above, with no intervening `running`.

- **running → limit_wait** (PRD #35, opt-in per run or per user) — a run that
  exhausts the owner's Anthropic usage limit **parks** instead of failing: the
  worker's slot is released, but its runner clone, skills plugin dir and per-run
  SDK home stay on disk, which is what lets the resume continue the same session
  rather than starting fresh. A sweeper pass promotes it back to `queued` once
  `retry_not_before` passes, and the resume skips the plan gate when the plan was
  already approved. Two independent guards keep the on-disk state alive: the
  runner's cleanup carve-out (teardown) and `home-reclaim`'s terminal-status check
  (the background sweep, hours later) — losing either loses the transcript. The
  park is server-timed and server-clamped, never worker-trusted; `retry_not_before`
  means *the earliest moment this user could spend anything*, computed at park time
  across the whole credential pool. See [adr/0035-run-limit-retry.md](adr/0035-run-limit-retry.md)
  for why that timing could not be deferred to the claim, and
  `prds/done/35-run-limit-retry.md` for the fourteen decisions.

- **queued → claimed** — `POST /api/worker/runs/claim` atomically claims the
  oldest queued run belonging to the caller's user (`FOR UPDATE SKIP LOCKED`),
  or the caller's own re-queued run if it is still inside its **affinity
  grace** (`WORKER_AFFINITY_GRACE`, default 2m) — giving a resume the best
  chance of landing back on the worker whose disk still holds the session and
  git worktree. After the grace window any of the user's workers may claim it —
  and when one does, the SDK session is **not** portable: a session is a local
  JSONL transcript at `$HOME/.claude/projects/<encoded-cwd>/<session-id>.jsonl`
  on the worker that wrote it, not server-side state. Being keyed by **both**
  HOME and cwd, it is lost to a different worker, to a replaced volume, and to
  a changed clone path on the very same machine. The worker therefore
  preflights the transcript before resuming (`agent/src/sdk-session.ts`, issue
  #105) and, when it is not resolvable here, drops the resume and says so on
  the feed rather than passing an id the SDK can only fail on — it resolves a
  resume locally, so an unresolvable id kills the run on its first turn instead
  of starting fresh. The run continues without its earlier context; if the
  branch already carries pushed work, the planning prompt says so, so an
  amnesiac lead reads that work instead of redoing it.
  Claim placement is also **fleet-aware** (PRD #216): affinity is checked
  first, as above, but past that, a worker already holding an active run
  defers a fresh queued run to a live, eligible peer that is strictly less
  loaded and has a free slot, rather than taking a second run while that peer
  sits idle. A queued run older than `WORKER_SPREAD_GRACE` (default 3× the
  poll interval) is exempt from this deferral so it can never be stranded
  waiting for a peer — see [adr/0216-fleet-aware-claim.md](adr/0216-fleet-aware-claim.md)
  for the eligibility seam and the placement/enforcement boundary against
  ADR-42.
- **claimed → running, before the plan turn** — once `provisionRunTools` has set up
  the run's tool env, the executor kicks off a lockfile-driven JS dependency
  install for the cloned repo, picked per discovered lockfile (monorepo
  workspaces resolving to one root install): a frozen, `--ignore-scripts`
  install per manager, **plus** per-manager hardening flags beyond that alone —
  `--ignore-scripts` does not close every repo-controlled install-time vector
  (see the PRD's Trust posture section). Exact commands live at
  `INSTALL_COMMANDS` in `agent/src/js-deps.ts`, kept there rather than
  duplicated here so this bullet can't drift from what ships. Runs under the
  same runner-uid + scrubbed-env sandbox as the checks below, concurrently
  with the plan turn — and, on human-gated runs, the `awaiting_approval` wait
  — and is joined before the first implement turn, so the agent's own
  dependency install never races it (PRD #121).
- **A run can also be born already past the gate** (PRD #209, `plan_source='seeded'`):
  `POST /api/repos/{id}/runs` accepts an externally-authored `plan_md` (+ an
  optional agent selection and a planned-against base commit) at create time,
  and such a run skips the planning turn and the `awaiting_approval` bullet
  below entirely — the worker implements the supplied plan directly, and the
  human checkpoint moves from the plan gate to the merge request. Reachable
  from the CLI only (`uzi run create --plan-file`, [docs/cli.md](docs/cli.md),
  [docs/seeded-plans.md](docs/seeded-plans.md)); the web board's start button
  is unchanged. Everything else about the run — status machine, sweeper
  coverage, guardrails below — is the same run this bullet describes.
- **running → awaiting_approval → running** — the lead agent produces a plan;
  the worker reports it (`POST /api/worker/runs/:id/state`) and the run parks
  at the gate until the user approves or rejects it in the run view, or
  `WORKER_PLAN_APPROVAL_TIMEOUT` (worker-side, default 24h) elapses. **The
  gate is a round-aware loop, not a single step (PRD #41).** Alongside
  approve/reject the user can **request changes**: the worker resumes the
  *same* SDK session with the feedback, the lead revises the plan, and the
  run re-parks at `awaiting_approval` with plan v2 — no new status, since the
  loop is entirely worker-internal (`plan → gate → (revise → resume → new
  plan → re-gate)* → approve/reject/cancel`, fail-closed on any other exit).
  Rounds are bounded by `PLAN_MAX_REVISIONS` (default 3, enforced both
  server- and worker-side; the server half is a counter on the run row, for
  the concurrency reason in [ADR-106](adr/0106-revise-cap-atomicity.md)), and
  the whole loop shares **one absolute `WORKER_PLAN_APPROVAL_TIMEOUT`
  deadline** computed at first gate entry, not
  a fresh one per round. A monotonic **gate epoch**, bumped at each
  `awaiting_approval` re-report, ties every verdict to the plan version the
  user actually saw — an approve or reject arriving mid-revision is
  discarded rather than silently applied to a plan no human reviewed. Once
  approved, the run resumes the same SDK session into the implement ⇄ review
  loop (`RUN_MAX_ITERATIONS`, default 5). See [PRD #41](prds/done/41-plan-revision-gate.md)
  for the epoch mechanism (Decisions 2/3) and
  [docs/run-activity.md](docs/run-activity.md#plan-approval-gate) for the
  user-facing actions. The worker also generates a short plain-English intent
  summary before planning and a plan summary + deltas at this gate, both
  advisory (skipped on any failure, never blocking the run) and spent on the
  run owner's own token — see [PRD #362](prds/done/362-run-summaries.md) and
  [docs/run-summaries.md](docs/run-summaries.md).
- **running ⇄ awaiting_input — the run's third human-in-the-loop channel**
  ([PRD #88](prds/done/88-ask-user-clarification.md)), beside the plan gate above
  and user-initiated steering below. The lead calls an in-process `ask_user`
  signal when it hits a fork it shouldn't resolve alone; the worker parks the
  run, posts a `question` run-message, and awaits an `answer` steering input
  the way `gatePlan` awaits a plan verdict. On answer it resumes the *same*
  SDK session (no transcript replay) and continues — a **pre-run** question
  (asked before `submit_plan`) resumes into a fresh planning turn that
  eventually reaches the plan gate; a **mid-run** question resumes the
  implement ⇄ review loop it interrupted. It is a distinct status rather than
  a sub-state of `awaiting_approval`, because `SetRunRunning`'s resume
  transition requires a *consumed* `approve_plan`, which an `answer` can
  never satisfy — the plan gate's status could not be reused for a question.
  It otherwise inherits `awaiting_approval`'s treatment everywhere that
  matters: every worker-death sweep, the `busy`/`active_runs` and
  rate-limit-window concurrency counts, and the board's move-pending
  bookkeeping all list `awaiting_input` alongside `awaiting_approval`, so a
  parked question is recoverable across a worker death, holds its worker
  slot, and never wedges a board card. The resume guard is keyed on the open
  **question's identity**, not on a timestamp or an arrival order: a
  worker-death requeue re-parks on the *same* question id (re-stamped from
  the claim), so an answer submitted just before the crash still resumes the
  run afterwards, while an answer to an already-superseded question is
  rejected. Bounded by an absolute answer deadline
  (`QUESTION_TIMEOUT_SECONDS`, default 24h) and a per-run question cap
  (`QUESTION_MAX`, default 5), both enforced worker-side and both
  **worker-in-memory** — a requeue resets both counters, so the honest
  worst case over a run's life is each value **× (RUN_MAX_REQUEUES + 1)**,
  not the configured value flat. **Only the deadline fails the run closed**
  ("clarification timed out"); there is no configurable default action.
  Exhausting the question cap does **not** fail the run — it emits a feed
  notice and the lead proceeds on its own best judgment instead. (The one
  cap-adjacent failure, a distinct message, is pre-run-only: looping on
  questions without ever reaching a plan.)
  **Autopilot never parks**: the same `claim.auto_approve` that
  short-circuits `gatePlan` short-circuits `ask_user`, auto-resolving with a
  frozen `"no human available — proceed on your best judgment, and note the
  assumption you made"` answer instead. The wording is quoted exactly because
  it is a frozen constant (`AUTOPILOT_SENTINEL_ANSWER`) that a test asserts
  byte-for-byte: an approximate quote here would read as the spec and send
  someone "fixing" the constant to match the doc. The question surfaces on every surface a
  user might be watching — the run view (an "Answer required" composer), the
  owner's opt-in Slack DM thread (free-text reply), and `uzi run answer` —
  and all three derive the open question from the run feed rather than a
  dedicated field, so no surface invents a question the others don't have. See
  the PRD's Decision Log for the full mechanism (including the accepted
  mixed-fleet-rollout residual: a run resumed onto a pre-#88 worker degrades
  to guessing rather than asking, mid-run, and a pending answer in transit is
  lost) and [docs/run-activity.md](docs/run-activity.md#answering-a-question),
  [docs/slack.md](docs/slack.md#using-it) and [docs/cli.md](docs/cli.md#commands)
  for the user-facing surfaces.
- **→ completed / failed** — the **worker**, not the agent, pushes the branch
  (`agent/issue-{iid}`) and opens the MR on completion (see Secrets, below);
  failure carries a `failure_reason`. The push and the whole MR-create call are
  wrapped in a bounded, fast transient-retry (`[1s,2s,4s,8s,16s]`,
  `agent/src/forge-retry.ts`): a dropped HTTP/2 stream, a 5xx, or a connection
  reset retries so a run whose work is already committed is not discarded, while
  a **permanent** rejection — auth failure, a protected-branch guardrail,
  non-fast-forward — fails fast and is never retried, per
  [ADR-284](adr/0284-forge-push-retry-classifier.md). A `failed` transition also
  lands an in-app inbox notification (`notifysvc.Notify`, inbox-only —
  Slack is not attempted, since the existing Slack ❌ DM already covers opted-in
  users) for the run's owner, gated on `stop_kind` so a deliberate cancel or
  plan-rejection stays silent and only genuine breakage notifies.
- **Milestone tracker reconciliation** (PRD #122 M2 + PRD #265 + PRD #390) — on a
  milestone-structured `issue` run the run view shows a *reported-complete*
  tracker (`runs.milestones_completed`, a monotone server-side union; never
  "verified"). Two sources feed it, both subset-validated against the frozen
  list server-side: mid-run `report_progress` calls (visible immediately,
  turn-non-ending), and — since PRD #265 — the lead's declaration on
  `signal_done` of the milestones it actually finished, unioned into the tracker
  on the `completed` transition. The declaration is what keeps a **single-turn
  run** honest: one that goes straight to `signal_done` never emits a mid-run
  report, so without it the tracker would freeze at "nothing reported" on a run
  that shipped its work. Completion is **declared, not inferred** — a milestone
  the lead leaves undeclared stays not-complete, so a deliberately-skipped
  milestone is not back-filled. `milestones_in_progress` (a snapshot, not a
  union) is cleared on every terminal transition a milestone-bearing run can
  reach, since "in progress" is meaningless on a done/failed/cancelled run. The web renders a **null** tracker
  as "not reported" (`M–/N`), distinct from a genuine `0/N`, so a completed run
  that simply never reported does not read as a failure. PRD #390 is the
  "make mid-run reporting truthful and enforced" step in that #122 → #265 → #390
  progression: mid-run reporting is now **enforced**, not merely offered — the
  per-turn prompt requires a `report_progress` declaration (`agent/src/prompt.ts`),
  and the implement/review loop (`agent/src/sdk-executor.ts`) escalates the next
  turn's prompt when a work turn leaves a milestone-bearing run with no milestone
  marked in progress, then surfaces a feed-only `status` signal after K=2
  consecutive misses so a silently-non-reporting lead is observable — while still
  never failing the run (D4) and keeping `checkpoint` a durability boundary, not a
  gate (D2): a cooperative checkpoint re-arms enforcement and clears the
  in-progress latch instead of nagging past a milestone boundary. Complementing
  that, an all-empty `report_progress` call (both sides empty after parsing) is
  now a **no-op, not a signal** (D3, `agent/src/signals.ts`) — it never persists a
  misleading `[]`, so the `milestones_completed` column stays `null` on a run that
  truly never reported and the neutral `M–/N` render is what that run actually
  shows. The CLI's `uzi run get` now renders that same neutral `–/N` numerator for
  a never-reported run (PRD #390 D5/M4, `api/cmd/uzi/run.go`), bringing it to
  display parity with the web badge and the TUI rail.
- **Sweeper** (a goroutine beside the forge poller) enforces what workers
  can't be trusted to self-report: a claimed-but-never-started run older than
  5 minutes is re-queued; a running run older than `RUN_TIMEOUT` (default 2h)
  is failed; a worker whose heartbeat is stale past `WORKER_HEARTBEAT_STALE`
  (default 45s) is marked offline and its non-terminal runs re-queued,
  incrementing `requeue_count` — past `RUN_MAX_REQUEUES` (default 1) a run is
  failed instead of re-queued again. An orphan sweep also runs once at API
  boot, so a run left dangling by a server restart is not stuck forever.
- **Cancel/reject with no live poller** (a `queued` run, or one whose worker
  has gone stale) is transitioned straight to `cancelled`/`failed`
  server-side rather than waiting on a `GET /api/worker/runs/:id/inputs` poll
  that would never arrive. **Accepted residual:** a live-run cancel (the
  worker is actively polling) still ends as `failed` with reason "run
  cancelled" — the worker protocol has no settable `cancelled` state, only
  the server-side no-poller path yields a true `cancelled`.
- The full message history (`run_messages`, gapless per-run `seq`) and any
  captured plan/session id survive every transition above, so a resumed run
  continues exactly where it left off — see Live message stream, below.
- **Bounded worker concurrency** (PRD #42, `adr/0042-worker-run-concurrency.md`): a
  worker may drive more than one of these state machines at once, capped by a
  worker-side slot semaphore (`WORKER_MAX_CONCURRENT_RUNS`, default 1 — the serial
  behavior above, unchanged) that the worker advertises at registration but the
  server never enforces. A run parked at `awaiting_approval` holds its slot the
  whole time, same as any other non-terminal run. Cap>1 is an informed opt-in with
  one accepted intra-user residual (Bash writes reaching outside a run's own
  worktree) — the sibling push-credential read is now closed by the PRD #51 uid
  split on the root-started compose path (a sibling run's agent is the `runner` uid;
  the push git child is the `worker` uid; a #58 single-uid start does not split) —
  documented at the knob in
  [docs/worker-setup.md](docs/worker-setup.md#concurrent-runs); the real fix for
  the remaining one — container-per-run — belongs to the future k8s-operator
  deployment (see Not yet in scope, below).

### Secrets: who holds what

Three tiers, deliberately narrower at each step, are the primary directive's
real enforcement (GitLab-side role protection and the SDK hook below are
defense-in-depth *on top of* this, not instead of it):

1. **`api`** decrypts the user's Anthropic OAuth token and forge bot PAT and
   sends both, once, in the claim response. It never sends them anywhere
   else and never logs them.
2. **The worker process** receives both, but only the PAT ever leaves the
   worker's own memory: the worker itself — not the agent — performs every
   authenticated git operation (clone, fetch, push) and the MR creation, via
   per-invocation env-scoped git config (`GIT_CONFIG_COUNT` plus a
   `GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` pair at the next free index,
   e.g. `GIT_CONFIG_KEY_0=http.<host-scope>.extraHeader`, host-scoped so the
   credential is only ever sent to the repo's own host, **not** `git -c`,
   whose values are readable on argv in the process table). The PAT is never
   written to on-disk git config and never enters the agent subprocess's
   environment.
3. **The agent subprocess** (the Claude Agent SDK's own process) gets *only*
   `CLAUDE_CODE_OAUTH_TOKEN`, `HOME`, `PATH` — none of the worker's own env
   (join token, PAT) is inherited; `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`
   are explicitly unset so they can't outrank the OAuth token. The agent only
   edits files and makes local commits; when it signals done, the worker
   performs the push and MR.

This means "worker disk/agent env contain no PAT" (a Success Criterion) holds
structurally, not by policy — the agent has no credential to push with in the
first place. On the compose stack the untrusted surfaces (the SDK, the
self-improve checks, the provision hooks, the runner clone's git) also run under
a distinct, cap-less `runner` uid from the credential-holding `worker` (PRD #51),
so a `runner` survivor cannot read the worker's join token or a live push-window
git child's `/proc/environ` — the same-uid residual this used to name is **closed
for the local path**. See [docs/proc-hardening.md](docs/proc-hardening.md) for the
built mechanism and its (deferred) k8s cross-container mapping.

### Guardrail layers (the primary directive)

Four independent layers, any one of which failing still leaves the others:

1. **Forge role + protected `main`.** The bot account sits at exactly the write
   role (GitLab **Developer** / Forgejo **`write`** / GitHub **Write**), never
   higher, and `main` is a protected branch the bot can neither push nor merge
   to — the outermost, platform-enforced backstop. On **GitLab** this is
   protection-by-default; on **Forgejo** it is **user-supplied**, because
   Forgejo creates repos with no protection and lets a `write` bot merge its own
   PR by default (D6c), so [docs/forgejo-bot-setup.md](docs/forgejo-bot-setup.md)
   leads with "protect `main` and enable the merge whitelist — uzi will not do
   it for you." On **GitHub** it is also user-supplied, and, on a
   classically-protected repo, **not fully detectable at write role either** — a
   write-role bot can read GitHub's newer *rulesets* but not classic branch
   protection, so that case surfaces as `BranchProtection.ProtectionUnverified`
   rather than a verified answer (see [ADR-238](adr/0238-github-driver.md)),
   which is why [docs/github-bot-setup.md](docs/github-bot-setup.md) recommends
   a ruleset over classic protection. uzi's privilege checker **detects** a bot
   that can push or merge to `main` on all three forges
   (`BranchProtection.BotCanPush`/`BotCanMerge`, or `ProtectionUnverified` where
   GitHub cannot determine it), and **[PRD #66](prds/done/66-guardrail-enforcement.md)
   enforces it**: uzi refuses to enable the repo, and refuses to start or claim a
   run against one that's already enabled, whenever that finding is present. The
   check runs live against the forge at all three points and fails closed,
   including on GitHub's `ProtectionUnverified` — "could not confirm" is treated
   the same as a confirmed violation, never read as safe. An instance admin may
   still allow one named repo through, per-repo and audited, but that override
   never waives an unreadable-protection finding either. Layers 2–4 below hold
   regardless of layer 1's configuration or its override. See
   [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md#protect-your-main-branch).
2. **Worker-owned network git** (above). The agent process has no push
   credential at all, so a protected-branch write is impossible regardless of
   what the model attempts — this is the layer the other three exist to
   reinforce, not replace.
3. **SDK `PreToolUse` deny-hook** on `Bash` (`agent/src/guardrails.ts`):
   denies `git push` to any branch unconditionally, ref/history-rewriting
   force operations (`git branch -D`/`-M`/`--force`, `--force`/`-f` on
   `checkout`/`switch`/`restore`), remote mutation (`git remote set-url`,
   `git config` writes to the `remote`/`url`/`http`/`credential`/`alias`
   namespaces), and credential-reading commands (`git config --get`, `env`,
   reads of `/proc` or the worker's token-secret path) — including through
   shell wrappers (`sh -c`, `git -c`, `eval`, `sudo`, `env X=y …`). Force
   flags on local, non-history file ops (`git clean -f`, `git add -f`) are
   deliberately **allowed**; only the ref/history-rewriting subcommands above
   are denied. A deny from this hook blocks the tool call even though the
   permission mode below is allow-by-default.
4. **`settingSources: []`.** Nothing from the cloned repository's own
   `.claude/settings.json`, hooks, commands, or `.claude/agents` is loaded *by
   the SDK* — a prompt-injection-via-repo defense none of the inspiration
   projects has. The one deliberate, user-gated exception is per-run agent
   selection (PRD #37): the *worker* parses `.claude/agents/*.md` itself and
   feeds them through the same programmatic `agents` map, only when the user
   picks the repo source at the plan gate. `settingSources` stays `[]`; repo
   hooks/settings/commands never load; and repo subagents are still bound by
   the `disallowedTools` list and the deny-hook above. See the Repo agents docs
   page and `specs/ai.md` "PRD #37".

Permission mode is explicitly `bypassPermissions` (allow-by-default) **plus**
the deny-hook and a `disallowedTools` list — not `default` (which hangs
headless waiting for an approval that will never come in an unattended
worker) and not a deny-by-default allowlist (too narrow for the coder
subagent's legitimate file/bash needs). Read-only roles (reviewer, tester)
are constrained per-subagent via their `tools` allowlist instead, since
subagents inherit the parent's permission mode and cannot loosen it.

### Live message stream

Every agent/tool event is **persisted first, broadcast second**:
`run_messages` rows carry a per-run, gapless `seq` (`UNIQUE(run_id, seq)`),
written by the worker's batched (500ms) `POST /api/worker/runs/:id/messages`
call before `internal/hub` fans it out over `/api/ws`. A browser reconnecting
mid-run replays via REST from its last-seen `seq` (`?after=<seq>`) and only
then continues on the live socket — so a dropped WebSocket connection loses
nothing, unlike a design where the socket itself is the only record. The
socket is a live cache and reconnect-convenience layer, **never** the source
of truth.

The `/api/ws` endpoint sits behind `RequireUser`, the same dual guard the run
read routes use: a browser session cookie **or** a user-scoped CLI token
(`uzc_`/`uza_`), so the headless `uzi` client can follow a run without a
browser (PRD #112 M1; the worker's own `uzw_` join token is a different
credential class and never reaches here). It enforces two rules that are
load-bearing, not incidental: **Origin validation** on the upgrade (the
standard CSWSH defense) and a **per-run authorization check on subscribe** —
the same owner-or-admin rule REST enforces, checked again here since a WS
subscription is a second entry point into the same data.

Admitting a Bearer token did **not** require relaxing the origin rule, and
that is the whole reason the change was a route move rather than a new
`Accept` option. A browser-less client sends no `Origin` at all, which
`coder/websocket` passes by design; a cross-site browser page cannot attach an
`Authorization` header, so it can only present the ambient cookie, sends its
own foreign `Origin`, and is still rejected. The cookie path is byte-identical
to what it was before.

The run view's activity pane (crew roster, collapsed-by-default logs, the
steer queue) is a pure client derivation over this stream plus `run.health`
(PRD #47) and a data-less `input` frame that mirrors `health`'s
re-read-on-signal shape — no new server-pushed state. See
[docs/run-activity.md](docs/run-activity.md) and
[docs/run-health.md](docs/run-health.md).

### Worker message-path contract (PRD #108)

A worker is untrusted input on `/api/worker/runs/:id/messages` and `/state`, exactly as `sanitizeSelfReported` already assumes for the self-reported register fields above. Before this PRD, three byte patterns a worker can legitimately emit were ones Postgres refuses to store, and a rejection wedged the run: the store error fell through to a generic 500, and the worker's batcher treated any throw as retryable, re-posting the identical (and growing) batch forever — 27 minutes and 239 lost messages in the incident that motivated the fix. Full rationale and Decision Log: [prds/done/108-worker-retry-loop-autostop.md](prds/done/108-worker-retry-loop-autostop.md). This section is the contract, not a changelog.

**What is sanitized, and where.** `workersvc.AppendMessages` (`api/internal/workersvc/service.go`) is the authoritative choke point. It strips `\u0000`, unpaired UTF-16 surrogates (U+D800–U+DFFF), and raw invalid UTF-8 from the message `payload` with a JSON-aware, in-string-state scanner (`sanitizePayloadJSON`, `workersvc/sanitize.go`) — deliberately not a decode/walk/re-encode, which would silently corrupt large integers (`jsonb` stores numbers exactly; a Go float64 round-trip does not). It NUL-strips (only) the four sibling worker-controlled TEXT columns — `kind`, `agent`, `agent_instance`, `agent_label` — since `encoding/json` has already folded surrogates and bad UTF-8 to U+FFFD by the time those exist as Go strings. Each is also rune-capped (`kind` 64, `agent` = `agenttmpl.MaxNameLen`, `agent_instance` 128, `agent_label` 80 — hardcoded constants, not env, since they bound a per-frame-repeated attribution field rather than tune an operator knob) so an unbounded value cannot become an unbounded column or an unbounded log line. Every strip is counted and logged, never silent, so a NUL-emitting tool stays visible rather than laundered. Deliberately **not** stripped: `\n`, `\t`, ANSI escapes — legal in `jsonb`, load-bearing in tool output, and rendered by a React component that escapes them (a different rule than PRD #90's `sanitizeMemoryField`, whose sink is a terminal table printer). The worker (`agent/src/sanitize.ts`, wired into `batcher.ts`'s `emit()`) applies the identical treatment before anything leaves the box — defense in depth, explicitly not the mechanism, since only a worker already running the patched image benefits from it.

`/state`'s worker-reported text (`failure_reason`, plus `session_id`/`plan_md`/`branch`/`mr_web_url`) gets the same NUL-strip on the same authoritative choke point (`sanitizeFailureReason`/`stripNULParam`, `workersvc/service.go`) — but **silently**, unlike `/messages`' count-and-log: these fields are worker/forge/git-minted rather than free tool output, so a NUL there is already anomalous rather than expected untrusted content, and `failure_reason` additionally carries the breaker's own trip report (below), so a poisoned run must still be able to record that it failed.

**What returns 400 vs 413 vs 500 — the retry contract.** The status code is what the client is obliged to believe, not a detail. `workersvc.ErrUnstorableMessage` maps to **400** for an enumerated, measured set of permanent Postgres SQLSTATEs — `22P05` (unsupported Unicode escape), `22P02` (invalid JSON text representation), `22021` (invalid byte sequence), and `22003` (numeric overflow: `{"n":1e1000000}` is legal JSON that sanitation cannot touch and `jsonb` cannot store). Classification is deliberately statement-level — only `InsertRunMessage`'s own error is eligible — so a `22P02` raised by the unrelated `foldRunUsage` fold is never misreported as "your batch is poisoned," which would drop messages that were never the problem. Everything else — connection loss, pool exhaustion, serialization failures, lock timeouts, statement cancellation, any non-`PgError` (notably a disconnecting client's `context.Canceled`), and `23503` (the run was deleted mid-batch: permanent, but not a payload fault) — stays on **500**, because a 500 must mean "try again"; misclassifying a transient failure as permanent is the mirror-image bug. **413** is separate and route-specific: `DecodeJSONLimited` (`api/internal/httpx/respond.go`) swaps the silently-truncating `io.LimitReader` for `http.MaxBytesReader` on this one route, so an oversize body is reported as oversize instead of folding into the same generic 400 a malformed batch gets — otherwise the worker has no way to learn "too large" from the server at all. 413 is a **split-and-retry** signal, never a poison verdict; the worker's own conservative byte cap keeps this a backstop it should rarely reach, and "no 413" is never proof of being under the cap (`MaxBytesReader` fires on the read, not the decoded value).

**A second poison-pill class, closed the same way.** `run_usage`'s primary key `(run_id, session_id, model)` (`00062_run_usage.sql`) has both `session_id` and `model` worker-controlled and untouched by the payload sanitizer above (no escapes, no invalid bytes — just length). `foldRunUsage` caps both at 200 runes before the upsert, because an over-long value overflows a btree index entry (2704 bytes) and raises `54000`, which is outside the enumerated permanent set and would otherwise 500 into the same forever-retry wedge one sink over. Truncating (never rejecting) is safe because both are a grouping key, not content, and a collision between two over-long values is absorbed by the same idempotent `GREATEST` merge that makes at-least-once delivery converge.

**How a batch is bounded, split, and bisected.** `agent/src/batcher.ts` caps a flush at `MAX_BATCH_BYTES` (512 KiB — a soft grouping target: the longest head prefix that fits) and tombstones any single message over `MAX_MESSAGE_BYTES` (900 KiB) at `emit()` time, before it ever reaches the flush state machine. A 413 splits the batch in half and **resets** the failure streak — a legitimate split is progress, not a repeat failure. A 400 with more than one message left triggers **bisection**: a one-sided search — post the left half; a failure narrows the search to it, a 2xx confirms the right half clean — isolates the single poisoned message in `ceil(log2 n)` posts (8 for the incident's n=239), backstopped at `MAX_BISECT_POSTS` (24). The isolated message is **tombstoned, not dropped**: it is re-posted under its original `seq` as a worker-minted `status` marker (`{"event":"message_dropped",...}`), because `web/src/lib/runStream.ts` requires seq contiguity and a genuine drop would freeze the live run view at the gap permanently — trading a client-side wedge for the server-side one this PRD removes. Only a *rejected* tombstone is a true, unrecoverable drop; it is idempotent (re-tombstoning a marker is a no-op) and carries the full attribution triple (`agent`/`agent_instance`/`agent_label`) so the loss renders in the subagent lane it happened in, not the top-level stream.

The breaker trips immediately on a fatal verdict (401/403/404 — every message would fail, so bisecting only burns budget proving it), on a rejected tombstone, or on an exhausted bisect budget. On an ordinary **transient** run it trips only after `TRANSIENT_TRIP_MS` (~10 minutes) of unbroken failure — a duration, deliberately not "N consecutive same-batch failures": once the 400/413 reclassification above is in place, a 5xx on this route means a genuine transient, so a tight count-based trip would fail healthy runs through an ordinary API restart. A trip's explanation is never emitted through the batcher itself — `concat` is order-preserving, so it would queue behind the very poison it's explaining, and a tripped batcher never flushes again — it is routed through each runner's `reportState` call instead, which carries its own bounded retries and 4xx-fatal semantics.

**The server-side half: detect the loop, then stop it (PRD #108 Phase 2).** Everything above protects a worker running the patched image. A fleet on an older image has none of it, so the authoritative detection and stop are server-side. **A detector that infers health from the message stream cannot detect a broken message stream** — PRD #47's `looping` reads its evidence from persisted `run_messages` (`toolWindow` → `ListRunToolWindow`), and this wedge *is* a failure to persist them, so that arm is blind by construction rather than by threshold. The signal that can see it is the API's own count of `AppendMessages` failures per run, produced on the failing path itself and needing no cooperation from the worker: `AppendMessages` and `detectRunHealth` are methods on the same `*Service` in the same process, so the failing writer writes `workersvc/persistfail.go`'s tracker directly and the sweeper reads it directly. The wedge cannot suppress it, because the wedge *is* the event being counted.

- **The counter's boundary is the ownership gate, and that is the whole of its safety.** Every recorded failure is reached only after `runOwnedByWorker` has succeeded, at exactly three non-test sites: `AppendMessages`' recorder (which requires a resolved run), `NoteOversizeBatch` (which re-checks ownership itself, because the 413 is answered *before* any ownership check runs), and nothing else. That gate — not the entry cap, which is defense in depth — is what bounds the map's key space to runs really claimed by the caller's own workers, and what stops a worker driving a streak against somebody else's run. A fourth recording hook added without it would be a cross-tenant kill primitive.
- **The flag is early warning and rides the health toggle; the stop does not.** At a low threshold (5 failures over 10s) the run reads `looping` with a reason naming the mechanism — *"the agent's updates can't be saved, so it keeps resending them"* — instead of falling through to `slow` at 45 minutes, which is what the incident actually reported. That arm is checked **first, above all three existing ones** — the resulting priority is persist-looping > tool-looping > stalled > slow. Above the tool-window arm because both map to the same enum and the persistence cause is the more specific truth on a run doing both; above `stalled` as a consequence, and that consequence is **user-visible**: a wedged run whose last persisted message is a *completed* `tool_result` used to read `stalled` and now reads `looping`, with a different Slack head and a different reason. It is not a purely additive arm, and describing it as one was wrong — an arm that returns first necessarily pre-empts the ones below it. The **stop** is a separate sweep step with its own operator switch (`UZI_AUTOSTOP_ENABLED`, env, default on), deliberately not `health_enabled` — an admin disabling health must not silently disable loop protection — and deliberately not a settings key, since an automatic destructive behaviour should not depend for its off switch on the database it might be misbehaving against.
- **The conjunction is the design, and it is deliberately not summarised as a count** (a tally cannot detect its own referent disappearing — see `00082`). Every member lives in `autoStopWedgedRuns`/`evaluateAutoStop`: the operator kill switch, ≥20 consecutive failures, sustained ≥60s, the run's `runs.last_seq` not advancing, the same failure class throughout, that class being one **a correct pre-0.10.1 worker could have hit through no fault of its own**, and **other runs succeeding on this same instance** inside the window. The last is the outage-vs-poison discriminator and the entire safety argument: if the API is broken, *every* run's writes fail and killing them turns an outage into data loss. When the comparison set is empty the rule is **flag and do not kill, permanently** — no fallback, no timeout into killing. The class guard is the one that is easy to get wrong: `unstorable` and `oversize` may kill (a browser's NUL bytes and a batch grown past 1 MiB are both things the world does to a correct old worker), while `invalid` and `store` may not — a malformed batch means the worker *build* is broken, and a 500 means retry, which is the contract the 400/413 split above exists to make honest. The no-progress and same-class guards are implemented as streak *resets* rather than as predicates at decision time, so reaching the threshold **is** the proof that neither changed.
- **A streak is evidence about one running attempt.** It is evicted when the run is observed terminal, when the run leaves `running`, and — load-bearing — whenever the sweeper hands the run back to the queue. Without that last one a requeued run returns under a fresh worker still carrying the dead attempt's streak and is stopped before the new worker persists a byte, which is likely rather than exotic for this population: a pre-0.10.1 worker wedged at 2 Hz with a growing batch is a prime OOM candidate, and OOM is what puts it there. The sweeper gives that window 15 seconds against the 5 minutes `defaultClaimGrace` budgets for claimed→started, and a fresh container from a *new image* takes `ensureClone`'s cold `cloneBare` path, so the gap spans an entire clone.
- **Two stop halves, one wire format.** With a live poller the server enqueues a synthetic verdict through `CreateStopVerdictInput` — and it rides `kind='cancel'`, not a new kind, because `SteeringChannel.route`'s `default:` arm **logs and drops** an unrecognised kind while `/inputs` is consume-on-read, making the drop permanent and unacknowledgeable. A new kind would therefore be a silent no-op on exactly the older fleet this exists to protect. Version skew is impossible by construction: the only thing on the wire is a cancel every worker version has always understood. With no live poller (or if a live worker ignores the cancel for 60s) the server takes the terminal transition itself via `FailRunAutoStop`, status-scoped so a run that finished in between is a no-op, and firing the same `publishSwept` + judge-enqueue side effects every other server-side `failed` path fires.
- **`stop_kind = 'auto_stopped'` is the contract; `failure_reason` is decoration and must never be parsed.** On the live half the worker reports its own terminal state, and `SetRunFailed` overwrites `failure_reason` unconditionally with `"run cancelled"` — so the two halves genuinely carry different strings and only `stop_kind` survives both. Migration `00082` widens the `runs_stop_kind_check` domain (constraint name measured, not assumed). Consumers must not fold the third value in with the two human ones: `web/src/lib/runBadge.ts`'s `isStoppedRun` enumerates the human kinds rather than null-testing, because a deliberate stop is not breakage and an auto-stop is, and `uzi run get` prints a `STOP_KIND` row for the same reason.
- **This is the fourth reason `api` is a hard singleton, and the one that fails silently.** The streak counters and the "other runs are succeeding" comparison set are in-process. Split across replicas, neither pod's streak reaches the threshold and each pod's comparison set is a fraction of reality, so auto-stop simply stops firing rather than misfiring — a guard that quietly disarms looks exactly like a healthy fleet. `deploy/chart/values.yaml`'s `api.replicaCount` comment says so at the place someone would change it.
- **Honest scope, which review must not oversell: this would not have fired on the incident that motivated it.** There was one active run, so there was no comparison set, so the rule degrades to flag-and-notify permanently — the correct behaviour on insufficient evidence, and tested as such rather than as a limitation. On a single-active-run deployment the *flag* is the value and the kill is insurance for multi-run instances and for pre-0.10.1 workers. There is also no metrics surface in `api` (no `promhttp`, no `/metrics`, no dependency), so the structured log lines named in [docs/run-auto-stopped.md](docs/run-auto-stopped.md) are the operator's interface, not a dashboard — and because every terminal path clears run health per PRD #47's exit contract, an auto-stopped run carries no flag afterwards, making those lines and that page the only durable record of why.

### Incidental findings (PRD #333)

A worker mid-run can flag a bug **outside** its current task without ending
its turn: this adds **no new service and no new run kind** — it's a plain
MCP tool on the existing run lane (issue/ci_fix/prompt/self_improve), sitting
beside `signals.ts` as a second, distinct in-process MCP server so the finding
tool can never be mistaken for (or promoted into) a turn-ending signal. Full
design rationale is in the PRD (`prds/done/333-incidental-findings.md`, especially
its Decision Log); the durable seams are in
[adr/0333-incidental-findings.md](adr/0333-incidental-findings.md); user-facing
usage is [docs/findings.md](docs/findings.md).

- **Capture is worker→api, never worker→forge.** The tool posts to a
  `RequireWorker` endpoint the same way a chat proposal does; the worker holds
  no forge credential at any point, so a finding cannot reach the forge by
  itself — only a subsequent, human-clicked file action can.
- **A coordinate-keyed two-table store, reusing the judge backlog's *shape*,
  not its producer.** `findings` is per-run evidence (one row per report,
  mirroring `review_recommendations`); `finding_dispositions` is the
  cross-run lifecycle, keyed on a stable `(user_id, repo_id, location)`
  coordinate rather than a churning row id — the same judge-backlog pattern
  of separating "what was observed" from "what's been done about it," applied
  to a different, independent producer (findings are about the user's code;
  the judge grades the agent's own performance). A `content_hash` on the
  disposition lets a materially different finding re-open an otherwise
  resolved coordinate, so a dismissed bug stays dismissed unless the report
  itself changes.
- **Filing is human-gated on the user's own forge connection**, the same
  claim-first write pattern `ConfirmProposalForUser` uses for chat: a
  disposition is claimed (`open` → `filing`) before the forge call, so a
  double-click can't file twice, and the filed text is resolved from the
  stored, sanitised row — never from whatever a client happens to send.

## Slack integration (outbound-only)

uzi's fourth surface is a Slack bot, owned entirely by `api` (`api/internal/slacksvc`, PRD #25): per-user run DMs, plan-approval buttons, reply-from-Slack steering, and — since PRD #191 — a conversational chat surface (a top-level DM to the bot opens a `kind='chat'` run streamed back into a thread, with `start_run`/`cancel_run`/`steer_run` tools and issue-proposal cards, the run-control trio added by PRD #322). It adds no new service and no new inbound port — the trust posture below is why. Full design rationale lives in the PRD (`prds/done/25-slack-integration.md`, especially its Security posture and Decision Log; the conversational surface in `prds/done/191-slack-chat-surface.md`); user-facing setup is [docs/slack.md](docs/slack.md).

- **Outbound-only, no inbound surface.** The manager (`slacksvc.Manager`) opens a Socket Mode WebSocket *out* to Slack and polls it live; there is no public URL, no signing-secret HTTP endpoint, and no new port on `api`. This holds the same "only `web` publishes a port" boundary above unchanged — Slack is a second *outbound* relationship, the same shape as the forge integration, not a new inbound one. The honest caveat: enabling Slack does export run *status metadata* off-box to Slack's cloud — and, since PRD #41, gated plan bodies too, and since PRD #191 chat message content for `kind='chat'` runs (see Content minimization, below).
- **`api` is the sole custodian of both Slack tokens.** The bot (`xoxb-`) and app-level (`xapp-`) tokens are settings values, sealed with the same `secretbox` key as every other secret at rest, and structurally excluded from every value-producing settings read (`settings.SecretKeys`, kept out of `Defaults` so `All()`/`Effective()` cannot emit them by construction — the same "cannot forget to redact" pattern used for the Anthropic token above). They are readable only through slacksvc's own decrypt accessors. A dedicated `slacksvc.Redact` additionally scrubs `xoxb-`/`xapp-` patterns *and* the Socket Mode connection URL's `?ticket=` query — a live-session credential the token-shape redaction alone would miss — from every log line. Neither token, nor the ticket URL, is ever sent to a worker or agent.
- **Identity mapping is the authz primitive for every inbound action.** `users.slack_resolved_id` (the manual override, or a cached `users.lookupByEmail` match) has a partial unique index (`users_slack_resolved_id_key`, `WHERE slack_resolved_id IS NOT NULL`), so at most one uzi user can ever resolve from a given Slack id. Every inbound handler (the Gatekeeper's Approve/Reject, the Replier's thread replies) re-resolves the Slack-authenticated envelope actor through `GetConfirmedUserBySlackID`, which additionally requires `slack_link_confirmed_at IS NOT NULL` and `is_active = true` — an unconfirmed link or a deactivated account resolves to no row and the action is refused with an ephemeral notice, never guessed at. Content flows only after the user completes a link-confirmation DM round-trip; since uzi emails are unverified at registration, that confirmation click — not the email match itself — is what makes the mapping trustworthy against account-squatting. Approve/Reject, follow-up and clarification-answer submissions then ride `workersvc.SubmitInput`'s own ownership check (`GetRunByIDForUser`) as a second, independent gate. A Slack answer additionally carries no id of its own, which makes *which question it answers* a derivation rather than a fact (PRD #88 M3). Web and CLI echo back the id they were shown; a free-text reply cannot, and the tempting derivation — "whichever question is open when it arrives" — is an **arrival-time key wearing identity's clothes**: a reply written against Q1 that lands after Q2 opened would be stamped with Q2's id *by the server*, and would then satisfy every downstream equality check precisely because the server supplied it, re-opening the race identity keying exists to close. So the reply is instead bound to the question it **follows**, by ordering its `ts` against the `ts` of the question message the notifier posted (`slack_run_messages.question_id` + `question_ts`); a reply predating the current card is refused as answering a superseded question, and an unorderable pair fails closed. That survives a requeue for free, because a re-park re-uses the question id, the notifier's identity dedupe therefore does not re-post, and the recorded `ts` still points at the original card. The body produced is the same `{question_id, answers}` shape (`workersvc.AnswerBody`, marshalled by the `gateSubmitter` adapter in `cmd/server`, so the wire shape is declared exactly once), and the server re-checks the id against `runs.open_question_id` — keeping the two facts independent: Slack says what the user answered, `workersvc` says whether that is still open.
- **Content minimization — with four deliberate exceptions: the plan gate, the clarification question, milestone titles, and (PRD #191) chat message content.** Slack messages otherwise carry status, repository path, issue number and title, MR link, and failure reason only — diff content never leaves `api`. Every dynamic field that could carry forge- or worker-controlled text (issue title, repo path, failure reason, a linked account's label) is mrkdwn-escaped (`EscapeMrkdwn`) before interpolation, so it can't smuggle a clickable link or an `@mention` into a message that also carries trusted deep-link markup, and a separate outbound scrub (`ScrubSecrets`) strips `sk-ant-`/`glpat-`/`xoxb-`/`xapp-` patterns from every outbound string as defense in depth. **The plan body is the one exception** (user-approved 2026-07-10, reversing this minimization for `plan_md` only — [PRD #41](prds/done/41-plan-revision-gate.md) Decision 10): every gated plan, and each revision a Request-changes round produces, is posted into the run's Slack thread — `ScrubSecrets`, then whole-blob CommonMark→mrkdwn rendering (`SlackMrkdwn`, not the per-field `EscapeMrkdwn` rule above, since the blob carries no trusted markup of its own to preserve — PRD #292, see below), then a rune-safe truncate to Slack's 3000-char block limit, with the deep link appended as trusted markup outside the truncated region. This genuinely widens what leaves the box: plan bodies quote source/issue content, and no layer here catches a secret a model happens to quote verbatim into a plan — only the four known token *patterns* above are stripped. Gate posts still land only in the owner's own 1:1 DM, so the added exposure is Slack's cloud (retention, admin export, e-discovery) plus that workspace's own admin boundary, not other members; the deep link (`UZI_PUBLIC_BASE_URL`/`public_base_url`) stays the canonical, untruncated rendering. **The clarification question is the second exception, on the same terms** ([PRD #88](prds/done/88-ask-user-clarification.md) Decision 9): when a run parks at `awaiting_input`, the question body — headers, question text, and any suggested option labels and descriptions — is posted into the same 1:1 DM thread through the *same* pipeline (`ScrubSecrets` → whole-blob `SlackMrkdwn` rendering (PRD #292) → rune-safe truncate → deep link as a separate block, `slacksvc.questionThreadBlocks`). It widens exposure the same way and for the same reason, and the same limit applies: question text is model-authored from repo and issue content, and only the four known token *patterns* are stripped, so a secret quoted verbatim into a question is not caught by any layer. Two things are specific to it. First, the text is **attacker-influenceable in a directed way** — an injected repo file can steer what the lead asks — so the question is treated as untrusted at every sink, and the lead's prompt forbids asking for a credential, token or password (D-G). Second, the widening runs in **both directions**: a question invites a human to type an answer back, and that answer is `ScrubSecrets`-ed and length-bounded in `workersvc.SubmitInput` so every surface inherits the scrub, not just the Slack one that always had it. **Milestone titles are the third**, a narrower one ([PRD #122](prds/done/122-milestone-structured-runs.md) M4): a `✓ N/M · working <title>` progress line carries a plan-authored milestone title, `EscapeMrkdwn`-escaped like the status fields but genuinely model-authored, so it shades from status metadata into content — which is part of why the count is four, not two. **Chat message content is the fourth, default-on wherever Slack is on** ([PRD #191](prds/done/191-slack-chat-surface.md) Decision 10): a `kind='chat'` run opened from a Slack DM streams its turns back into that DM's thread, each turn's `text` frames coalesced into one edited message through the *same* pipeline (`ScrubSecrets` → whole-blob `SlackMrkdwn` rendering (PRD #292) → rune-safe truncate → deep link outside the truncated region, `slacksvc/chatpost.go`); it widens exposure the same way and for the same reason, with the same limit (only the four known token *patterns* are stripped, so a secret the model quotes verbatim into an answer is not caught). Two things are specific to it. First — the **second-order exposure** — the exposure is broader than the run-state DM lanes it rides beside: the chat agent's read tools (`list_runs`/`get_run`/`get_run_messages`) are user-scoped but **not** kind-scoped, so a Slack chat can be asked to quote an **`issue` run's** message content — plan bodies, diffs, tool output — into Slack. The run-state notification lanes for the five non-chat kinds (`issue`, `ci_fix`, `judge`, `self_improve`, `prompt`) stay byte-identical to before; what changed is that run *content* now has a route to Slack, through chat, that it did not have. Second, the write direction is human-gated the same way the web Chat page is: the `start_run` tool and `propose_issue` emit **cards** whose Create/Start button — not the tool call — performs the forge write / run start, through the presser's own connection, so a repo that says "start a run on #42" produces at most a card. The same gate covers run control (PRD #322): `cancel_run` and `steer_run` likewise only emit cards — a danger Cancel button (confirm-gated) and a Steer button that, on press, arms a one-shot pending steer and asks the presser to reply in the thread with their instruction — and it is that button press (cancel) or in-thread reply (steer), not the tool call, that reaches `SubmitInput`.
- **Untrusted bodies render, they no longer just escape** ([PRD #292](prds/done/292-slack-mrkdwn-rendering.md)). The four sites above that post whole-blob model-authored text — the chat answer, the plan/gate body, the clarification question, and (new here) the judge review / self-improvement / schedule-paused notification body — now run that text through `SlackMrkdwn` (`api/internal/slacksvc/mrkdwn.go`), a goldmark-AST CommonMark→Slack-mrkdwn renderer, in place of the old whole-blob `EscapeMrkdwn`. It is at least as safe as the escape it replaces: every text/code/raw-HTML node's content still goes through the same `slackutilsx.EscapeMessage` `EscapeMrkdwn` wraps, so an injected `<@U123>` mention or a `<https://evil|Open>` payload still parses as inert text, not live markup; a clickable `<url|label>` is only ever emitted from an `https://` link node, with both the URL and label sanitized so neither can re-open Slack's link grammar; and anything the renderer doesn't understand (tables, images, raw HTML) degrades to escaped plain text rather than a parse error. The trusted per-field chrome around these bodies — repo path, titles, agent names, verdict/category chips, deep links — is untouched by this change and stays on the per-field `EscapeMrkdwn` used everywhere else in this section, since that text carries intentional uzi-authored markup rather than being an untrusted blob.
- **Inbound rate limits, at two layers.** The Socket Mode receive loop ACKs every envelope before processing (Slack retries an un-ACKed one in ~3s), and a per-Slack-user flood window bounds thread-reply volume. Separately, the two `/me/slack` endpoints that trigger an outbound DM to a caller-supplied Slack id (`PUT .../override`, `POST .../test-dm`) sit behind a dedicated, tighter per-user `mw.Limiter` (`SLACK_DM_RATE_LIMIT_MAX`/`_WINDOW`, distinct from the forge limiter above) plus a 30-second per-target DM cooldown in `slacksvc.Linker` — together bounding both an arbitrary-member DM-spam primitive and a member-id enumeration oracle.
- **The primary directive is unaffected.** Slack can only approve, reject, request changes to (feeding text back into the same planning session), or otherwise thread-steer a plan gate — a latency/authorization control, not a `main`-write capability. A wrongful approval can at worst produce a branch + MR, same as an approval from the web UI, and every one of the [four guardrail layers](#guardrail-layers-the-primary-directive) is untouched by this integration: Slack never holds a forge credential, never talks to a worker, and never reaches the agent's own context.
- **Fails safe when unconfigured.** With Slack disabled (the default) or either token absent, the manager idles and every other surface behaves exactly as before — nothing here is a hard dependency of the run lifecycle.

## Chat with uzi (the fifth surface)

uzi's fifth surface is a conversational one: a **Chat** page — and, since PRD #191, a **Slack DM** to the bot (see the Slack section above) — where a user talks to an agent that knows uzi's own source, can investigate the user's runs, and can draft GitLab issues or start runs the user confirms. It adds **no new service and no new trust boundary** — chat rides the existing worker/run machinery as a third run **kind** (`runs.kind = 'chat'`), the same way `ci_fix` did as a second kind. Full design rationale is in the PRD (`prds/done/39-chat-agent.md`, especially its Decision Log and the review-corrected decisions); user-facing usage is [docs/chat.md](docs/chat.md). The short map:

- **Chat is a run kind, not a new service.** A conversation is one `chat` run: `repo_id`/`issue_iid`/`branch` all NULL (`runs.repo_id` was made nullable, with `runs_kind_shape` enforcing the all-NULL shape for chat and the existing NOT-NULL shape for every other kind). The first user message and every follow-up are ordinary `run_user_inputs` (`kind='follow_up'`) rows — the initial message is atomically seeded into `run_user_inputs` by `CreateChatRun` so a chat can never exist without its first message — and the whole conversation streams through the same persisted-first `run_messages` → `/api/ws` pipeline (with REST replay) as any run. Chat runs are excluded from the board, the runs list, and the admin runs list (they have no repo/issue), but an individual chat is still openable owner-or-admin via the run view (`GET /api/runs/:id`).
- **A dedicated claim lane, narrower credentials.** The worker polls a second, independent claim lane (`POST /api/worker/runs/claim?lane=chat`) alongside its run slot, so a chat is served concurrently with an executing run (up to `WORKER_CHAT_SESSIONS`, default 1) without queueing behind a long run. Chat is cheap to hold — no clone, no worktree, no provisioning. The chat claim payload is a **separate `ChatClaimPayload` that structurally cannot carry a forge PAT** — the `ForgePAT` field does not exist on it, one tier narrower than a run claim (which carries the PAT for the worker's git operations). A chat needs no repo and no forge connection at all: a user with only an Anthropic token can still chat.
- **`ChatRunner`, not `RunRunner`.** Chat has its own slim runner (claim → session loop → complete) with no git/clone/push/MR spine. The `ChatExecutor` runs the SDK session with `cwd` at the baked source and a **deny-by-default tool surface**: the SDK `tools` option is restricted to `Read`/`Grep`/`Glob` plus the uzi MCP tools (not `allowedTools`, which does not confine under `bypassPermissions`), with `disallowedTools`, `settingSources: []`, the Bash deny-hook, and the **path-guard hook rooted at `/opt/uzi-src`** (with the worker join-token path in `extraSecretPaths` as defense-in-depth; the load-bearing protection is the outside-root deny, since the token file lives outside the baked root, plus the `/proc` deny that closes the one non-Bash egress for `CLAUDE_CODE_OAUTH_TOKEN`). No Bash, no Write/Edit, no WebFetch/WebSearch, no subagents. Lifecycle is idle-bounded + per-turn wall-clocked + turn-capped (server-enforced from persisted inputs, not worker-trusted); an idle sweep replaces the 2h `RUN_TIMEOUT` for chat. An idle-ended conversation is resumable via **Continue** (a new chat run carrying `resume_of_run_id`, resuming the persisted SDK session when the worker's disk still holds it).
- **Baked source, never a clone.** uzi's own source is copied into the worker image at build time to `/opt/uzi-src` (read-only, root-owned) with a `BUILD_INFO` stamp, so the agent's answers match the *deployed* version exactly and need no PAT and no network — the same bundle-at-build discipline the docs section uses. This required moving the agent image's build context to the repo root with a per-Dockerfile ignore that hard-excludes `.env*`, `.git`, `inspiration/`, and `node_modules` (committed history is never baked).
- **uzi tools are an in-worker MCP server** (`agent/src/uzi-tools.ts`, the `signals.ts` precedent): `list_runs` / `get_run` / `get_run_messages` read the worker's **own user's** runs (new user-scoped worker endpoints, `GET /api/worker/chat/runs*`, that filter by the authenticated worker's `user_id` — never a bare run id, so a foreign id is a 404), and `propose_issue` drafts an issue on the **current** chat run only (closure-scoped, no injectable run id) and **never writes the forge**. All forge- and model-derived text returned by the read tools (titles, failure reasons, messages, plans) is wrapped in an **untrusted-evidence fence** with a per-call CSPRNG nonce in the closing sentinel, so attacker-authored evidence cannot forge a close tag and break out to become an instruction — the same posture `ci_fix` uses for job logs. `start_run`, and — since PRD #322 — `cancel_run` and `steer_run`, are the run-control analogue of the next point: each emits a card only (`run_request` / `cancel_request` / `steer_request`) and makes **no mutating call** from the tool handler, so the run write happens on the human's Create/Start/Cancel click or Steer thread-reply, through the presser's own connection, resolved against the existing owner-scoped `SubmitInput` (`cancel`/`follow_up`) — no new endpoint, no new run kind, no migration.
- **Issue creation is structurally human-gated.** `propose_issue` persists a `pending` `issue_proposals` row and emits a `proposal`-kind run_message rendered as a card by the browser (and, PRD #191, as a Block Kit card in Slack); only a human Create click — the browser's session+CSRF `POST /api/chats/:id/proposals/:pid/confirm`, or the Slack card's button routed through the same lifted `ConfirmProposalForUser` — executes `Forge.CreateIssue` via the user's own connection (forge-first, PAT-redacted, per-user forge-rate-limited). Confirm is **claim-first** (atomic `pending → confirming` before the forge call, revert on failure) so a double-confirm creates exactly one issue; a stuck-`confirming` row is swept back to `pending`, and a boot-time clamp keeps the sweep timeout safely above the forge HTTP timeout so a slow-but-alive confirm is never reaped into a duplicate. The proposal write path simply does not exist without the human click.
- **The primary directive is unaffected.** Chat holds no forge credential and has no git access; the worst it can do is draft an issue the user must click to file. Every guardrail layer is intact — `settingSources` stays `[]`, the deny-hooks still fire, and no PAT is ever in reach of the chat agent. Named residual (unchanged from the run surface): `CLAUDE_CODE_OAUTH_TOKEN` is in the agent's env, but chat's no-Bash/no-network/no-`/proc` surface is strictly stronger than a run's, so a prompt-injected chat has no egress channel for it.
- **Slack is a second entry point to the same chat** (PRD #191). A top-level DM to the bot opens a `kind='chat'` run through the identical service verbs the web uses; nothing kind-scoped forks (no new run kind, no new claim lane, no new credential — the chat claim still carries no forge PAT). The composite operations the web did in its *handlers* — proposal confirm and run start — were lifted into `workersvc` (`ConfirmProposalForUser`, `StartRunForUser`) so both surfaces share one claim-first, ownership-scoped, PRD-gated path; the `start_run` tool lands in web chat in the same change. Turns stream back into the DM thread (content-widened per Decision 10, above); issue proposals and start-run requests render as Block Kit cards in a third `slack_chat_*` action namespace (not the plan gatekeeper's), whose Create/Start click performs the write through the presser's own connection. The per-user chat spend limiter is shared across web and Slack (one budget bounds the person, not the surface), and the run-affecting verbs keep riding `workersvc`'s ownership checks, so a forged card value can only ever act on the confirmed presser's own runs.

## The uzi CLI: a second API consumer

Until PRD #64, `web` (browser + session cookie) was the API's only caller.
The `uzi` CLI (`api/cmd/uzi/`, same module as `api`) is a second one, driven
identically by humans and agents — it talks to the same JSON API `web` does,
over a different credential. It adds **no new service and no new inbound
port**: the CLI is an outbound-only client of the existing `api`, the same
way a browser is. User-facing usage is [docs/cli.md](docs/cli.md); full
design rationale — including the security audit that found and closed the
scope-ceiling gap below — is in the PRD (`prds/done/64-uzi-cli.md`, especially its
Decision Log).

`uzi tui` (PRD #112, `api/cmd/uzi/tui_*.go`) is a full-screen view over that same
client — a live runs board, a run-detail split with a per-agent lane rail and
transcript, and run-level steering. It adds **no endpoint and no service**: every
read and write is a call the plain CLI already makes. Its one new capability is a
Bearer-authenticated subscription to the existing `/api/ws` hub, which required
moving that route from the cookie-only group into `RequireUser` — a route move with
`websocket.Accept`'s options untouched (see the ws section above for why the
same-origin rule still covers both credential paths). It lives in `package main`
rather than a `tui/` subpackage because Go forbids importing a main package, and the
sanitize and render helpers it must reuse live there; that trade is recorded in
[prds/done/112-uzi-tui.md](prds/done/112-uzi-tui.md). User-facing usage is
[docs/cli.md](docs/cli.md); the plain `--json` verbs remain the agent-facing surface
and are unchanged.

The CLI can also hold several credentials at once as **named contexts**
(`uzi context …`, PRD #427) and switch which one a command uses via a flag, an
env var, or a sticky default. This is purely client-side credential selection —
which already-stored `{URL, token}` pair a command sends — with no server, API,
or scope change; authority is still the token's server-enforced scope. See
[Config and credentials](docs/cli.md#config-and-credentials) for the mechanics.

- **One new credential, one new middleware.** A `cli_tokens` row (`uzc_`
  user-scoped / `uza_` admin-scoped, sha256 at rest, mirroring the worker
  join-token posture) is presented as `Authorization: Bearer …`.
  `mw.RequireUser` dispatches on whether that header **parses** as
  `Bearer <non-empty>` — never on bare presence, and never "try cookie, fall
  back to Bearer" — so a request takes exactly one path: a parsing Bearer
  header goes CLI-token-only (no CSRF), anything else goes through the
  existing `RequireAuth` cookie path, CSRF-enforced, byte-identical to
  before. Both populate the same `userKey` every handler already reads, so
  no handler needed to change to gain a CLI-reachable route.
- **The route swap is enumerated per route, not per group.** Only the routes
  a v1 CLI verb needs were moved onto `RequireUser` (`GET /api/auth/me`,
  the `/api/runs` read+input routes including `/review`, `GET /api/repos` +
  `POST /api/repos/{id}/runs`, `GET/DELETE /api/workers`, the 9 admin GETs).
  Everything else — `POST /api/workers` (mints a plaintext join token that
  can read decrypted secrets), `/api/me/cli-tokens` itself, `/api/vault/*`,
  `/api/forge/*`, `/api/me/secrets/*`, and every admin write — stayed
  cookie-only. Swapping whole route groups would have hit endpoints no v1
  command needs and materially widened what a stolen CLI token could do; see
  the PRD's route-disposition table for the full per-route reasoning.
- **`scope` is a ceiling, enforced in the middleware, not just at
  `/api/admin/*`.** `RequireAdminRO` is a plain `user.IsAdmin` check — the
  real control is upstream, in `RequireUser`: a CLI token whose `scope !=
  'admin_ro'` gets a **copy** of the user row with `IsAdmin=false` installed
  in the request context. Every owner-or-admin handler outside
  `/api/admin/*` that reads admin-ness live (`GetRunForViewer`,
  `ListRunMessagesForViewer`, `GetReviewForTarget` — three run-visibility
  checks an admin's default `uzc_` would otherwise widen to "any user's
  run") degrades to owner-only **for free**, with no handler change. This is
  the fix a security audit added mid-design: an earlier draft checked scope
  only under `/api/admin/*` and would have shipped an admin's everyday
  token able to read every user's run transcripts.
- **`apitypes` (`api/internal/apitypes/`) is a stdlib-only leaf.** The CLI
  imports handler DTOs through it rather than `internal/handler` directly,
  so the binary never drags in `chi`/`pgx`. Enforced mechanically, not by
  convention: a test runs `go list -deps ./cmd/uzi` and fails if `pgx` or
  `chi` appears in the dependency graph.
- **Browser login is poll-based, not a loopback listener.** `uzi login`
  generates a PKCE verifier locally, sends only its challenge to
  `POST /api/auth/cli/start`, prints a `user_code` plus a `/cli-auth?request=`
  URL, and polls `POST /api/auth/cli/poll` until a human — in an
  already-authenticated browser tab, via whatever this instance's login
  method is (password or OIDC) — types the code and approves. No token is
  minted at approve; the poll mints it, claim-first (`UPDATE … WHERE
  status='approved' … RETURNING`) so two racing polls can't both mint from
  one approval. This is deliberately unlike `multica`'s loopback-listener
  design (a session JWT in a URL, SSH-hostile, LAN-exposed binding
  heuristics) — see the PRD's inspiration-check table.

## Docs

The `/docs` section (`web/src/pages/Docs.tsx`, `web/src/pages/DocPage.tsx`)
renders the repo's own `docs/*.md` in-app, bundled at build time via a Vite
glob (`web/src/lib/docs.ts`) rather than served from an API or duplicated
into `web/`, so the in-app copy can never drift from the repo copy. See
[docs/README.md](docs/README.md) for the frontmatter contract and how to
add a page, and the PRD (`prds/done/7-docs-section-webui.md`) for the design
rationale.

- **Audience-gated visibility.** Every doc carries a leading-fence
  frontmatter block (`title`, `order`, `audience`); `audience` decides where a
  page appears (see the role-aware index below), ordered by `order`.
  `design`/`contributor` pages (and anything with missing or malformed
  frontmatter) stay repo-only, so adding a page is self-describing and never
  touches `web/` code.
- **No new trust boundary.** `/docs` is public (no auth) — the content is
  non-secret and already world-readable in the repo, and onboarding docs are
  needed before a user can do anything else. `react-markdown` renders
  without `rehype-raw`, so raw HTML in a doc stays inert rather than needing
  a sanitizer; content is repo-reviewed, not user- or model-supplied.
- **Role-aware index (issue #75).** `audience: user` pages are listed, routed
  and searched for everyone; `audience: operator` pages additionally for admins
  (`me.is_admin`), in an "Admin / operator" section. Presentation-only, not a
  trust boundary: every `docs/*.md` is already eager-bundled to every browser, so
  the `is_admin` gate filters the index/routing/search, not what is downloaded.
- **Link rewriting.** A relative link to another in-app-routable page becomes an
  in-app route (`/docs/:slug`) — `user` pages for everyone, `operator` pages for
  admins (`rewriteHref(href, isAdmin)`); a link to a repo-only file (`../plan.md`,
  a `design`/`contributor` doc) rewrites to the pinned GitLab blob URL instead.
  `#anchor` fragments are preserved either way.
- **Build-time validation gate.** `web/scripts/check-docs.mjs` runs ahead of
  `npm run build` and fails on missing/invalid frontmatter, a missing or
  duplicate `order` among `user` or `operator` pages (each its own namespace), a
  broken relative link (doc→doc or doc→img), reference-style links, or an
  oversized `docs/img/*` file; it warns (without failing) on a `user` page over
  the 60-line house-style budget.
  It runs both locally (ahead of `npm run build`) and in CI (`validate:web`
  runs `npm run check-docs` too, PRD #52), so a broken doc fails the pipeline as
  well as the local build.

## Deployment: compose (laptop) and k8s

uzi has two deploy topologies for the **same** services and trust boundaries. The
compose stack above is the laptop MVP; PRD #52 adds a k8s deployment to a
**dev cluster** via **ArgoCD** GitOps, the way the rest of the
platform is deployed. The release/deploy runbook is [deploy/README.md](deploy/README.md);
the design rationale (Decision Log, the compose→chart adaptations) is
`prds/done/52-cicd-argocd-deploy.md` — this section is the map, not a duplicate of it.

- **compose** (`docker-compose.yml`) — `web` + `api` + `db` (`postgres:17`) +
  the opt-in `agent`, secrets from `./.env`, published to `127.0.0.1:8080` only.
  The path this whole document otherwise describes.
- **k8s** (`deploy/chart/`, an umbrella chart) — `web` (Deployment/Service/
  Ingress) + `api` (Deployment/Service/NetworkPolicy) + a **CloudNativePG
  `Cluster`** (the upstream `cluster` chart as the `postgres` subchart) in place
  of the `db` container + `InfisicalSecret`s for runtime secrets and the Harbor
  pull secret. Per-cluster values (in a private GitOps repo, `apps/uzi/values/<cluster>.yaml`) carry the
  public host, `FRONTEND_ORIGIN`, `TRUSTED_PROXIES`, and the CNPG image/storage,
  layered over the cluster-agnostic `deploy/chart/values.yaml`. ArgoCD deploys a
  **multi-source** app (the private GitOps repo's `apps/uzi/`): the released chart
  from an OCI registry + these values from that GitOps repo. Public URL
  `https://uzi.example.com` behind ingress-nginx (a wildcard-TLS domain). Images + chart are versioned Model B (chart `version` ==
  `appVersion` == the release git tag; see the runbook). **Optionally** (PRD
  #58, off by default — `workers.enabled: false`) a `uzi-controller`
  Deployment and a dedicated `uzi-workers` namespace it renders hosted worker
  pods into; see [Worker controller](#worker-controller-k8s-only) below.

**Trust boundaries are unchanged by k8s.** The same three boundaries hold: `web`
is still the sole entry point (now the ingress → web Service, still same-origin,
no CORS); `api` is still the sole holder of secrets/keys and makes the same
outbound forge calls; the worker is still outbound-only over the Bearer join
token. The k8s deployment adds **no** new secret-bearing surface beyond
Infisical-sourced env, and every [guardrail layer](#guardrail-layers-the-primary-directive)
is untouched — `main` protected, worker holds the PAT, agent never gets network
git.

Two adaptations are load-bearing and worth stating here (full detail in the PRD's
"K8s-specific adaptations"):

- **X-Forwarded-For: append in k8s, overwrite in compose.** In compose the web
  nginx sets `X-Forwarded-For: $remote_addr` (overwrite) because its immediate
  client *is* the browser (see [Trust boundaries](#trust-boundaries) above). In
  k8s the immediate client is the ingress-nginx controller, so the chart's web
  nginx **appends** (`$proxy_add_x_forwarded_for`) with ingress-nginx as the
  outermost overwriting hop — overwriting there would collapse every user into
  one per-IP auth rate-limit bucket and lose client IPs in audit logs.
  `TRUSTED_PROXIES` is the pod CIDR (e.g. `10.244.0.0/16`) so the api
  reads the real client IP from the appended chain. The compose behavior stays
  overwrite; the chart toggles this per-deployment.
- **api NetworkPolicy (default-deny ingress, web pods only).** The cluster is a
  **shared** one — other tenants run arbitrary dev code on it —
  so a per-IP rate limit that trusts XFF from the pod CIDR is only safe if a rogue
  pod cannot reach the api directly to forge `X-Forwarded-For`. The chart's api
  NetworkPolicy allows ingress **only from the web pods** on `:8080` (an L3
  connectivity control, distinct from the XFF-trust value above), closing the
  direct-to-api spoofing path. A sibling app on a non-shared mgmt cluster has no
  such policy; uzi needs it. Its one operational wrinkle —
  default-deny also drops kubelet probes, so `api.networkPolicy.probeCIDRs` must
  be set to the node CIDR on Antrea — is documented in the runbook and values.

**The api is a hard singleton** in both topologies (compose runs one container;
the chart pins `replicas: 1` + `strategy: Recreate` + a generous `startupProbe`).
Three independent reasons it cannot run >1 replica today: the forge poller and
run sweeper hold single-goroutine in-memory state (two replicas double-poll and
double-sweep), the Slack Socket Mode manager would double-handle events, and
`store.Migrate` runs goose at boot with **no advisory lock** (two pods booting
concurrently race the migrations). `Recreate` (not `RollingUpdate`) ensures the
surge never briefly runs a second pod. The api is documented as
non-horizontally-scalable this release; `web` is stateless and runs 2 replicas.

**Laptop workers need no published image change.** The per-user worker stays
available as a local, opt-in `docker compose --profile agent` process on user
machines, and a laptop worker still points at `https://uzi.example.com`
and joins through the ingress unchanged (the remote-worker posture the [worker
trust boundary](#serverworker-trust-boundary) and
[docs/proc-hardening.md](docs/proc-hardening.md) anticipate). What changed
under PRD #58: there is now a published worker image too
(`.../uzi/agent-{base,jvm}:<tag>`, built on every `v*` tag), and an optional
in-cluster alternative — see the next section.

### Worker controller (k8s only)

**PRD #58** adds one new, deliberately small component: `uzi-controller`, the
only thing in uzi that ever holds a kube-API credential — `api` holds none and
must never hold one (the spec constraint this component exists to satisfy).
Shipped in the chart but **off by default** (`workers.enabled: false`); see
[docs/hosted-workers.md](docs/hosted-workers.md) for the user-facing feature
and [deploy/README.md](deploy/README.md#hosted-workers-prd-58) for turning it
on.

- **Scoped to two empty namespaces, one of them privileged.** Its
  ServiceAccount carries a `Role` bound to the dedicated `uzi-workers`
  (**restricted**-tier) namespace containing nothing but hosted worker
  objects — no CNPG, no other app's Secrets, no privileged ServiceAccounts —
  plus, when the docker tier is enabled, a second `Role`+`RoleBinding` into a
  dedicated `uzi-workers-docker` namespace running
  `pod-security.kubernetes.io/enforce: privileged` (PRD #83 M3). The
  privileged tier is a recorded deviation from the PRD's original
  `baseline`-namespace intent: the cluster's k8s and containerd versions
  are below what pod user namespaces need, so a flagless-rootless DinD
  sidecar is infeasible there, and the userns remap inside the
  `docker:dind-rootless` image is the security property instead. Isolating
  that privileged tier in its own namespace is what still bounds a
  compromised controller — a `Role` that can create pods in a namespace can
  mount any Secret and impersonate any ServiceAccount already there, so the
  emptiness (not the RBAC verbs alone) is the real fence, exactly as the
  restricted namespace already relied on — plus its own default-deny
  `NetworkPolicy` (in-cluster lateral egress denied; external egress is the
  named [PRD #50](prds/50-llm-egress-proxy.md) residual, not closed here).
  **The two tiers' external egress is OPPOSITE, and a probe that does not name
  the tier is uninterpretable:** the **restricted** tier enforces the FQDN
  allowlist (`worker-fqdn-egress.yaml` over the `worker-networkpolicy.yaml`
  default-deny floor — only `cache.nixos.org`, the forge, `*.anthropic.com`,
  and, as of #285, the CNPG chart's OCI pair (`ghcr.io` +
  `pkg-containers.githubusercontent.com`, replacing four GitHub-ish hosts —
  `github.com` is **no longer allowlisted** on this tier); `api.github.com`
  **TIMEOUT**, measured #123 §1),
  while the **docker** tier reaches arbitrary internet hosts by design
  (`0.0.0.0/0`-except-in-cluster by CIDR, `worker-docker-networkpolicy.yaml`:
  `api.github.com` **200**, `search.devbox.sh` **404**) — [PRD #50](prds/50-llm-egress-proxy.md)'s
  residual, not a broken control. A docker-tier reading looks exactly like a
  broken standard-tier allowlist; two false alarms have resulted (an operator
  during #123, and a closed #283 on 2026-08-09), so **check `uzi admin workers`
  → `docker:` (true = docker tier = broad egress) before concluding anything
  about egress enforcement, and re-measure on the restricted tier.**
  Both namespaces' Roles carry the same pinned-minimal verbs: Deployments/PVCs
  create/list/patch(Deployments only)/delete; Secrets **create/delete
  only** — no `get`/`list`, so the controller writes each worker's join
  token once and can never read any token back, including one it didn't
  create. No pod access at all, in either namespace.
- **Outbound-only, like a worker.** The controller authenticates to `api`
  with its own bearer credential and polls a controller-facing endpoint for
  desired state; there is no inbound port on the controller and `api` never
  dials it — the same direction of trust as the worker protocol above, just a
  second, differently-scoped credential.
- **Stateless.** Desired state lives in Postgres, observed state in the
  cluster; every poll reconciles the two from scratch, so a controller or api
  restart loses nothing.
- **The join token still never rests server-side in plaintext** (Decision 3):
  `api` seals it as a delivery buffer, the controller writes it into the
  worker's Secret as a **file** mount (never `secretKeyRef` — the same
  `/proc/<pid>/environ` leak class [docs/proc-hardening.md](docs/proc-hardening.md)
  closes elsewhere), and `api` destroys its own sealed copy only once the
  worker actually authenticates with it — proof of delivery, not a
  controller-asserted claim. Residuals (plaintext in etcd for the worker's
  lifetime; the pending buffer's own TTL) are documented in
  [docs/vault-threat-model.md](docs/vault-threat-model.md#hosted-worker-join-tokens-prd-58).
- **TLS on this hop, not an ingress.** The controller and every hosted worker
  dial `api`'s TLS listener directly (see
  [Server/worker trust boundary](#serverworker-trust-boundary) above) — there
  is no ingress in front of `api` for this traffic.
- **Provisioning is user-triggered, not autoscaled.** A user clicks
  "Provision" in Settings → Workers; the controller then renders that one
  worker's Deployment/Secret/PVCs and keeps it running until deleted.
  Spawn-on-queued-work and scale-to-zero remain deferred — see
  [Not yet in scope](#not-yet-in-scope) below.

Full design rationale — why a dedicated controller rather than the api, the
RBAC verb-by-verb reasoning, the namespace/NetworkPolicy split, and the sizing
presets — is `prds/done/58-hosted-k8s-workers.md` (its Decision Log especially). The
docker tier's own privileged-namespace ruling (Q-B) and Decision 3's
separate-mount-namespace invariant are recorded in
`prds/done/83-docker-capable-worker.md`.

### Worker version and upgrade health

A worker's `version` is written **only at register** (`workersvc.Register`), so a worker
that is offline mid-roll still reports the release it was running before — heartbeats
carry a version the api discards, and the `X-Client-Version` header is read nowhere. That
one fact is why upgrade health needs two independent detectors rather than one: version
comparison answers "is this behind?", and it structurally cannot answer "did this try to
upgrade and fail?", because the worker that failed is the worker that cannot report.

The second detector is the controller, which observes worker **pods** (`pods: list`, both
worker namespaces) and POSTs a **display-only** report to `POST /api/controller/status`.
Display-only is structural rather than promised: the report lands in its own table whose
only foreign key is `worker_id`, the upsert is confined to existing `kind='hosted'` rows so
it can neither create workers nor touch an external one, liveness stays heartbeat-owned,
and the endpoint answers 204 with no body — so there is nothing for the controller to read
state back out of. PRD #58's "the controller asserts nothing" holds for everything else;
the poll is still a pure read.

The api folds both into one derived `upgrade_status` on the worker DTO, computed at read
time and never stored, so it cannot go stale against the row it describes. Three separate
timestamps make that safe, and collapsing any pair reopens a specific hole: the
controller's own clock is display-only, the api stamps its own receipt time to drive
signal freshness, and a third column anchors a ceiling on how long a controller may be
believed about one roll — cleared only by the worker's own authenticated re-registration
moving its version. Roll-health rows are reachable only by joining through `workers`,
which is what makes the per-user scoping unavoidable.

Rationale, the decision log, and the alternatives that were rejected are in
`prds/done/113-worker-upgrade-status.md`. The user-facing behaviour is
[docs/worker-upgrades.md](docs/worker-upgrades.md).

## Not yet in scope

Auto-starting a run from a GitLab label, a CI-status watching/fixing agent,
WS wakeup for idle workers (a 3s poll is the MVP), **PRD #84's
capability-aware pre-run readiness check** (the lead can already `ask_user`
before planning — see [Run lifecycle](#run-lifecycle) — but nothing yet
decides automatically that an issue is missing capability/tool/size and
calls it; #88 ships the mechanism, #84 owns that trigger and remains
Draft), **autoscaled/spawn-on-demand hosted workers** (PRD #58 delivered the
static-provisioning subset of the item decided 2026-07-10, specs/ai.md §168 —
a user-triggered click provisions a persistent worker via the dedicated
controller described [above](#worker-controller-k8s-only); the controller
never spawns one on its own when queued work appears, and there is no
scale-to-zero — a hosted worker runs until deleted, same as one you start by
hand), per-user
skills-management UI, encrypting secrets with the user's
own password instead of a shared server key, PAT least-privilege
verification, and a second (e.g. OpenAI) execution provider are all
deliberately deferred — see [plan.md](plan.md), the PRDs' Risks sections, and
`prds/done/4-agent-runtime-workers.md`'s "Out of scope". This document will grow
new sections (additional services, data flows, trust boundaries) as those
land.
