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
- **`api`** (`api/Dockerfile`) — a statically linked Go binary (`chi` router) on `gcr.io/distroless/static-debian12`, running as `nonroot`. Serves `/api/auth/*`, `/api/admin/*`, `/api/agent-templates/*`, `/api/me/secrets/*`, `/api/worker/*` (the worker protocol, PRD #4), `/api/ws`, `/api/health`, among others. Runs its own DB migrations and builtin-template reconciliation at startup before it starts accepting traffic (see below).
- **`db`** — stock `postgres:17`, digest-pinned in `docker-compose.yml`. Data lives in the named volume `pgdata`; it is not a bind mount, so it survives container recreation but is bound to the Docker volume store on this host.
- **`agent`** (`agent/Dockerfile`, opt-in via `docker compose --profile agent up`) — one worker per user: a Node 22 + git + bash container (not distroless — the Claude Agent SDK's Bash tool needs a real shell) that connects *outbound only* to `api` and executes runs. See [Agent runtime](#agent-runtime-workers-runs-live-view) below and [docs/worker-setup.md](docs/worker-setup.md) for the operator procedure.

All images are pulled/built by digest or pinned tag in `docker-compose.yml`, not floating `latest`.

## Trust boundaries

- **Only `web` publishes a port**, and only to loopback: `127.0.0.1:8080` in `docker-compose.yml`. `api` and `db` have no `ports:` mapping — they are reachable exclusively over the private compose network, by each other, and are invisible from the host's other network interfaces or from any other machine.
- **nginx is the sole entry point and the sole source of truth for the client's identity.** `web/nginx.conf` sets `X-Forwarded-For: $remote_addr` (not `$proxy_add_x_forwarded_for`), which *overwrites* any `X-Forwarded-For` a client sent rather than appending to it. `api` then only trusts that header when the direct connection comes from a `TRUSTED_PROXIES` CIDR (the compose network's private ranges, by default) — see [docs/auth-design.md](docs/auth-design.md#rate-limiting-and-the-x-forwarded-for-trust-model). Net effect: a client cannot spoof its apparent IP by sending its own `X-Forwarded-For`.
- **The session cookie is the only credential**, and it is `HttpOnly` — never readable by any JavaScript running in the SPA, including an XSS payload. See [docs/auth-design.md](docs/auth-design.md) for the full cookie/CSRF/revocation design.

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
4. Opens the `pgx` connection pool, then reconciles the builtin agent templates through it (`store.ReconcileBuiltinTemplates`, see below): idempotent, so this is safe to run on every boot.
5. Builds the `secretbox.Box` from the already-validated key.
6. Only then starts the HTTP server.

`web` depends on `api`'s healthcheck (`GET /api/health`, which itself pings the DB pool), so `docker compose up` brings the stack up in the correct order: `db` healthy → `api` migrated and healthy → `web` serving.

## Forge integration

uzi's second surface connects each user to a git forge (GitLab now, via a forge-generic interface that keeps a later Forgejo driver from touching callers, schema, or UI) so the board has real work to show. It adds no new service — everything below lives inside `api` — but it does add a second trust boundary: `api` now makes authenticated *outbound* calls to a third party (the forge) on top of the inbound boundary at the nginx edge described above. See [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md) for the operator/user procedure and the PRD (`prds/2-forge-integration-kanban.md`) for the full design rationale.

### Forge abstraction

`api/internal/forge` defines the `Forge` interface (`VerifyToken`, `ListProjects`, `ListLabels`, `EnsureLabels`, `ListIssues`, `UpdateIssueLabels`) and a neutral domain vocabulary (`BotIdentity`, `Project`, `Label`, `Issue`); `forge.New` selects a driver by `forge.Type` (`gitlab.go` today) so no other package ever imports a driver directly. Every driver call goes through an `*http.Client` bounded by `FORGE_HTTP_TIMEOUT` (`timeoutClient` in `forge.go`) — closing the untimeouted-`http.DefaultClient` wart the `multica` inspiration carries — and every returned error is passed through a `redactor` (`redact.go`) that scrubs the PAT and any `Authorization`/`PRIVATE-TOKEN` header value before the error can reach a log line or an HTTP response body.

### Bot PATs, encrypted at rest

`api/internal/secretbox` is a generic AES-256-GCM box (12-byte random nonce prepended to the ciphertext) — deliberately not PAT-specific, since the same utility is slated for per-user Anthropic OAuth tokens in the next PRD. `config.Load` calls `secretbox.LoadKey("UZI_SECRET_KEY")` at boot and refuses to start if it is missing, not valid base64, not exactly 32 bytes, or a low-entropy placeholder (e.g. all-zero) — the same refuse-to-start stance as `JWT_SECRET`. The resulting `*secretbox.Box` is constructed once in `main.go` and handed to `forgesvc.Service`, which seals a PAT into `forge_connections.token_ciphertext` on connect and opens it on every forge call thereafter. **Rotating `UZI_SECRET_KEY` invalidates every stored PAT** — there is no re-encrypt path in this MVP, so a rotation means every user reconnects.

### SSRF guard

The server making authenticated outbound calls to a `base_url` supplied at connection time is a classic SSRF surface (cloud metadata endpoints, internal services, loopback). `config.Config.ForgeAllowedBaseURLs` (from `FORGE_ALLOWED_BASE_URLS`, default `https://gitlab.example.com`) is the only set of base URLs a connection may target; `config.NormalizeForgeBaseURL` requires `https` and canonicalizes to `scheme://host[:port]` before every allowlist comparison, and boot fails if the parsed list is empty or any entry is malformed or non-`https`. The Settings → Forge UI offers only this set (`GET /api/forge/config`), so a user cannot even attempt a free-text URL.

### Data model: forge as source of truth

Migration `00002_forge.sql` adds four tables, all scoped down to `forge_connections.user_id` by FK cascade:

- **`forge_connections`** — one row per (user, forge_type, base_url); carries the encrypted PAT and the verified bot identity.
- **`repos`** — projects discovered via the bot's membership list, keyed by the forge's stable numeric project id (not the path, which can be renamed); upserted with `enabled=false` on every listing call so enable/disable always has a row to target.
- **`board_columns`** — ordered label names per repo; the implicit Open (no column label) and Closed (issue `state`) columns are never stored.
- **`issues`** — a *cache*, never authoritative. uzi's own board state is limited to column configuration; every other field is overwritten from the forge on each sync. `has_prd_link` is computed at fetch time from the issue description (regex match on a `prds/*.md` reference) and stored as a bool — the description itself is never persisted.

### Sync engine

`api/internal/forgesvc.Service` is shared by the HTTP handlers and the background poller: it builds a `Forge` driver from a stored (encrypted) connection, runs the PRD-link check, and implements `IncrementalSync`/`FullSync` against an `IssueStore` interface (narrowed for unit testing against a fake store and a mocked `Forge`, without a live database).

`api/internal/poller.Engine` (`main.go`) is started as a single background goroutine alongside the HTTP server. Each tick, for every enabled repo, it either runs an incremental pull (`ListIssues(labels=["PRD"], state=all, updated_after=HWM)`, high-water-mark = the max `updated_at` the forge itself returned, never the client clock) or, every `FORGE_RECONCILE_EVERY`-th tick (and always on a repo's first poll after being enabled), a full reconcile: fetch the complete `PRD`-labeled set with no lower bound, upsert everything, and evict cache rows the forge no longer returns. Eviction is the only way to observe de-labeling, closing-with-label-removed, or deletion, which an `updated_after`-filtered query structurally cannot report. Per-repo state (`hwm`, poll count) lives only in the `Engine`'s in-memory map — a disabled repo simply drops out and a re-enabled one starts over with a fresh full reconcile.

Writes are forge-first: a board move (`POST /api/repos/:id/issues/:iid/move`) calls `UpdateIssueLabels` before touching the cache, and only updates the cache on success — a failed forge write leaves the card where it was (snap-back), never an optimistically-moved card the forge disagrees with.

Shutdown ordering matters here specifically because of the poller: `main.go`'s `run()` calls `srv.Shutdown` to drain in-flight HTTP requests, cancels the root context (stopping the poller's next tick), then `pollerWG.Wait()`s for any in-flight sync to finish — all *before* the deferred `pool.Close()` runs, so a mid-tick database query never races the pool shutting down underneath it.

### CI status + fix agent (PRD #6)

The same poller tick that syncs issues also syncs **pipeline status** (`forgesvc.SyncPipelines`, after the issue sync — no second loop, no new interval). For each enabled repo it resolves the **watched refs** — the repo's `default_branch` plus the branches of its recent agent runs (non-terminal, or terminal-with-an-MR within `CI_WATCH_RUN_WINDOW`, capped at `CI_WATCH_MAX_REFS`) — and caches the latest pipeline per ref in `pipeline_statuses` (a latest-per-`(repo, ref)` cache, upserted every tick, reconcile-evicted like the issues cache). A run branch is read via `LatestMRPipeline(mr_iid)` (which catches detached and merged-results pipelines the branch-ref query misses); the default branch via `LatestPipeline`. The forge stays the source of truth; a ref with no CI honestly caches nothing. `CI_WATCH_MAX_REFS=0` disables the whole feature. The four pipeline read methods (`LatestPipeline`/`LatestMRPipeline`/`ListPipelineJobs`/`JobLogTail`) live behind the same `Forge` interface + PAT-redaction discipline as every other forge call.

The board, board header, and repos list render the cached status as badges (`web/src/lib/pipelineBadge.ts` collapses GitLab's status set to five tones). A failed watched pipeline offers a **Fix CI** button that `POST`s to `/api/repos/:id/ci-fix-runs`: the server re-checks the cache shows the ref failed, freezes a **failure snapshot** (up to `CI_FIX_MAX_JOBS` failed jobs, each with a `CI_FIX_LOG_TAIL_BYTES` log tail, PAT- + `glpat-*`-redacted) onto a new run, and queues it.

A `ci_fix` run rides PRD #4's run machinery as a second run **kind** (`runs.kind`, with a nullable `issue_iid` + a `runs_kind_shape` CHECK; the failed pipeline is snapshotted onto the run so it stays self-contained). The worker (`agent/`) diagnoses from the snapshot — job logs are framed as quoted **untrusted evidence**, and every PRD #4 guardrail layer holds verbatim — behind the same plan gate as an issue run. The plan is either a fix (implemented on `ci-fix/pipeline-{id}` for a default-branch failure, or the existing agent branch for a run-branch failure, then pushed + MR'd by the worker) or an explicit **`not_code`** verdict (infra/flaky/secret/runner problem: the run completes with the diagnosis and no MR).

**Verification** ("uzi verifies its work"): the pipeline sync stamps a `ci_fix` run's `fix_verdict` — `verified` when its post-fix pipeline passes, `fix_failed` when it fails — keyed on `runs.branch` (the fix branch, not the failed ref, which differ for a default-branch fix) with an `observed pipeline id > snapshot pipeline id` guard so the original failing pipeline never false-stamps. Manual trigger + the plan gate keep a human in the loop; auto-spawn on failure is deferred (it would burn tokens unattended — `multica`'s ungated autopilot is the audited weakness, not the pattern to copy). See [docs/configuration.md](docs/configuration.md#ci-status-integration-prd-6) for the env knobs and the documented residual risks (poll-based staleness, third-party secrets in logs, merge-result false-positives).

### Per-user rate limiting on forge-proxying endpoints

The endpoints that call out to the forge on a user's behalf (`.../verify`, `.../projects`, `.../issues/:iid/move`, `.../sync`) run behind a second instance of the same fixed-window `mw.Limiter` PRD #1 uses for auth endpoints, but keyed **per user** (`PerUserMiddleware`) rather than per client IP, and budgeted separately (`FORGE_RATE_LIMIT_MAX`/`FORGE_RATE_LIMIT_WINDOW`, default 30/minute) from `RATE_LIMIT_MAX`/`RATE_LIMIT_WINDOW`. This bounds how hard one uzi user's actions can hit the upstream forge, independent of the local-network IP-sharing caveat already documented for the auth limiter in [auth-design.md](docs/auth-design.md#accepted-limitations).

## Agent templates

An agent template (`agent_templates` table) is the definition of one role an
agent can play: name, description, an optional model override, an optional
tools allowlist, and a prompt body. It is not itself an agent; it is the
recipe a later release renders into a running one.

- **`builtins/` is the single source of truth.** Eight builtin roles — the
  `lead` orchestrator (`model: opus`) plus seven subagents (`coder`,
  `reviewer`, `auditor`, `tester`, `documenter`, `fact-checker`,
  `spec-keeper`) — are Go-embedded from `api/internal/agenttmpl/builtins/*.md`,
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
- **Read/write split.** Any authenticated user can list, view, and preview
  templates (`GET /api/agent-templates*`); only an admin can create, edit,
  delete, or reset one (`RequireAdmin` in `api/internal/handler/handler.go`).
  Templates are shared across all users, so this closes the hole where any
  user could rewrite the prompts everyone else's agents run with.
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
- **Validation.** `name` is kebab-case (`^[a-z0-9]+(-[a-z0-9]+)*$`), unique,
  and immutable after creation (it is the subagent's filename and identity;
  renaming means creating a new template and deleting the old one; builtins
  are never renamed). `description` and `prompt_body` are required and
  non-empty; `description`, `model`, and each tool name must not contain a
  newline, carriage return, or other control character (they each render on
  a single frontmatter line). A template is rejected if its description or
  prompt body contains what looks like a complete Anthropic token (a
  high-confidence `sk-ant-...` match); the UI separately warns, without
  blocking, on looser patterns so legitimate text stays savable.
- **API surface.** All endpoints require authentication (session + CSRF); the
  writes also require admin: `GET /api/agent-templates` (list), `GET
  /api/agent-templates/:id` (one), `GET .../rendered`, `POST
  /api/agent-templates` (create), `PUT .../:id` (update; name is ignored,
  immutable), `DELETE .../:id` (409 on a builtin), `POST .../:id/reset` (400
  on a non-builtin). Edits are last-write-wins — no optimistic-concurrency
  check in this release — but every row records `updated_by` and `updated_at`
  so concurrent edits are at least attributable after the fact.

## Agent skills

A skill is a named Markdown playbook (progressive disclosure: name +
description sit cheaply in an agent's context; the body loads on demand) that
attaches to agent templates rather than existing on its own. Full rationale
in `prds/16-agent-skills.md` (Solution Overview, Technical Design, Trust
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
  builtins convention above) — starting with `ci-cd-norms`.
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
  its only allocation surface. Each subagent's `AgentDefinition.skills` scopes
  it to its own allocated skills, re-filtered to what actually survived
  materialization.
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
new table) holding one AES-256-GCM-sealed secret per `(user, kind)`. The
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

The API around it is deliberately minimal: `PUT /api/me/secrets/anthropic_token`
writes or rotates the caller's own secret, `DELETE` on the same path removes
it, and `GET /api/me/secrets` (and every other read) returns **metadata
only** (`kind`, `created_at`, `updated_at`); there is no reveal endpoint, and
no admin path to another user's secret value. See
[docs/anthropic-token.md](docs/anthropic-token.md) for the user-facing flow.

## Agent runtime: workers, runs, live view

uzi's third surface is the one that acts: a per-user worker container claims
queued work, drives a Claude Agent SDK session against a cloned repo, and opens
an MR — never touching `main`. This adds one optional service (`agent`, above)
and a third trust boundary: `api` now also accepts *inbound* calls from each
user's worker (the reverse of the forge boundary, where `api` calls *out*).
Full design rationale lives in the PRD (`prds/4-agent-runtime-workers.md`,
especially its Decision Log); this section is the map.

```
browser ──WS+REST──▶ web (nginx) ──▶ api (Go)  ◀──HTTP (TLS deferred), poll/claim/report── agent (worker, per user)
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

**TLS on this hop is accepted as deferred, not met, by the MVP.** The `api`
service listens plain HTTP on the compose network; the join token gives
*authentication* now, but not *transport encryption*. This is fine while the
worker is another container on the same private compose network; it becomes a
real gap the moment a worker runs off-network (a laptop, a remote VM) with
`UZI_API_URL` pointed at a public `api` endpoint over plain HTTP. Closing it —
a TLS-terminated ingress in front of `api` — is scoped to the same later phase
that moves the worker off compose onto its own host/pod (see
[docs/proc-hardening.md](docs/proc-hardening.md)'s remote-worker design); it is
flagged here so it is never silently assumed solved by "outbound-only".

`api` remains the **sole holder of the encryption keys** (`UZI_SECRET_KEY`, see
above) throughout: it decrypts a user's Anthropic token and forge bot PAT and
hands both to the worker **only inside a run's claim response**
(`POST /api/worker/runs/claim`) — never persisted server-side in plaintext,
never logged (the same redaction discipline as the forge integration).

### Run lifecycle

One `runs` row is the unit of work; an issue can accumulate several over its
life (a DB partial unique index enforces at most one non-terminal run per
issue). Status is a linear state machine:

```
queued → claimed → running → awaiting_approval → running → completed
                                                           → failed
   ↳ (worker dies) → re-queued, up to RUN_MAX_REQUEUES → failed
   ↳ cancel with no live poller → cancelled directly (server-side)
```

- **queued → claimed** — `POST /api/worker/runs/claim` atomically claims the
  oldest queued run belonging to the caller's user (`FOR UPDATE SKIP LOCKED`),
  or the caller's own re-queued run if it is still inside its **affinity
  grace** (`WORKER_AFFINITY_GRACE`, default 2m) — giving a resume the best
  chance of landing back on the worker whose disk still holds the session and
  git worktree. After the grace window any of the user's workers may claim it.
- **running → awaiting_approval → running** — the lead agent produces a plan;
  the worker reports it (`POST /api/worker/runs/:id/state`) and the run parks
  until the user approves or rejects it in the run view, or
  `WORKER_PLAN_APPROVAL_TIMEOUT` (worker-side, default 24h) elapses. Approval
  resumes the same SDK session into the implement ⇄ review loop
  (`RUN_MAX_ITERATIONS`, default 5).
- **→ completed / failed** — the **worker**, not the agent, pushes the branch
  (`agent/issue-{iid}`) and opens the MR on completion (see Secrets, below);
  failure carries a `failure_reason`.
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
first place. See [docs/proc-hardening.md](docs/proc-hardening.md) for the
one honestly-open residual: a same-uid `/proc` read of a live git child's
environment during the short push window, and the k8s uid-split design that
closes it fully.

### Guardrail layers (the primary directive)

Four independent layers, any one of which failing still leaves the others:

1. **GitLab role.** The bot account is Developer, never Maintainer/Owner, and
   `main` is a protected branch — see
   [docs/gitlab-bot-setup.md](docs/gitlab-bot-setup.md#protected-main-branch).
   A GitLab-side rejection is the outermost, platform-enforced backstop.
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
   `.claude/settings.json`, hooks, or `.claude/agents` is loaded — a
   prompt-injection-via-repo defense none of the inspiration projects has.

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

The `/api/ws` endpoint authenticates with the same session cookie as the rest
of the browser API (not the worker's Bearer token) and enforces two rules
that are load-bearing, not incidental: **Origin validation** on the upgrade
(the standard CSWSH defense for cookie-authenticated WebSockets) and a
**per-run authorization check on subscribe** — the same owner-or-admin rule
REST enforces, checked again here since a WS subscription is a second entry
point into the same data.

## Slack integration (outbound-only)

uzi's fourth surface is a Slack bot, owned entirely by `api` (`api/internal/slacksvc`, PRD #25): per-user run DMs, plan-approval buttons, and reply-from-Slack steering. It adds no new service and no new inbound port — the trust posture below is why. Full design rationale lives in the PRD (`prds/25-slack-integration.md`, especially its Security posture and Decision Log); user-facing setup is [docs/slack.md](docs/slack.md).

- **Outbound-only, no inbound surface.** The manager (`slacksvc.Manager`) opens a Socket Mode WebSocket *out* to Slack and polls it live; there is no public URL, no signing-secret HTTP endpoint, and no new port on `api`. This holds the same "only `web` publishes a port" boundary above unchanged — Slack is a second *outbound* relationship, the same shape as the forge integration, not a new inbound one. The honest caveat: enabling Slack does export run *status metadata* off-box to Slack's cloud (see Content minimization, below).
- **`api` is the sole custodian of both Slack tokens.** The bot (`xoxb-`) and app-level (`xapp-`) tokens are settings values, sealed with the same `secretbox` key as every other secret at rest, and structurally excluded from every value-producing settings read (`settings.SecretKeys`, kept out of `Defaults` so `All()`/`Effective()` cannot emit them by construction — the same "cannot forget to redact" pattern used for the Anthropic token above). They are readable only through slacksvc's own decrypt accessors. A dedicated `slacksvc.Redact` additionally scrubs `xoxb-`/`xapp-` patterns *and* the Socket Mode connection URL's `?ticket=` query — a live-session credential the token-shape redaction alone would miss — from every log line. Neither token, nor the ticket URL, is ever sent to a worker or agent.
- **Identity mapping is the authz primitive for every inbound action.** `users.slack_resolved_id` (the manual override, or a cached `users.lookupByEmail` match) has a partial unique index (`users_slack_resolved_id_key`, `WHERE slack_resolved_id IS NOT NULL`), so at most one uzi user can ever resolve from a given Slack id. Every inbound handler (the Gatekeeper's Approve/Reject, the Replier's thread replies) re-resolves the Slack-authenticated envelope actor through `GetConfirmedUserBySlackID`, which additionally requires `slack_link_confirmed_at IS NOT NULL` and `is_active = true` — an unconfirmed link or a deactivated account resolves to no row and the action is refused with an ephemeral notice, never guessed at. Content flows only after the user completes a link-confirmation DM round-trip; since uzi emails are unverified at registration, that confirmation click — not the email match itself — is what makes the mapping trustworthy against account-squatting. Approve/Reject and follow-up submissions then ride `workersvc.SubmitInput`'s own ownership check (`GetRunByIDForUser`) as a second, independent gate.
- **Content minimization.** Slack messages carry status, repository path, issue number and title, MR link, and failure reason only — plan and diff content never leaves `api`; the plan is one click away behind the deep link (`UZI_PUBLIC_BASE_URL`/`public_base_url`). Every dynamic field that could carry forge- or worker-controlled text (issue title, repo path, failure reason, a linked account's label) is mrkdwn-escaped (`EscapeMrkdwn`) before interpolation, so it can't smuggle a clickable link or an `@mention` into a message that also carries trusted deep-link markup. A separate outbound scrub (`ScrubSecrets`) strips `sk-ant-`/`glpat-`/`xoxb-`/`xapp-` patterns from every outbound string as defense in depth, on top of the mrkdwn escaping.
- **Inbound rate limits, at two layers.** The Socket Mode receive loop ACKs every envelope before processing (Slack retries an un-ACKed one in ~3s), and a per-Slack-user flood window bounds thread-reply volume. Separately, the two `/me/slack` endpoints that trigger an outbound DM to a caller-supplied Slack id (`PUT .../override`, `POST .../test-dm`) sit behind a dedicated, tighter per-user `mw.Limiter` (`SLACK_DM_RATE_LIMIT_MAX`/`_WINDOW`, distinct from the forge limiter above) plus a 30-second per-target DM cooldown in `slacksvc.Linker` — together bounding both an arbitrary-member DM-spam primitive and a member-id enumeration oracle.
- **The primary directive is unaffected.** Slack can only approve, reject, or steer a plan gate — a latency/authorization control, not a `main`-write capability. A wrongful approval can at worst produce a branch + MR, same as an approval from the web UI, and every one of the [four guardrail layers](#guardrail-layers-the-primary-directive) is untouched by this integration: Slack never holds a forge credential, never talks to a worker, and never reaches the agent's own context.
- **Fails safe when unconfigured.** With Slack disabled (the default) or either token absent, the manager idles and every other surface behaves exactly as before — nothing here is a hard dependency of the run lifecycle.

## Docs

The `/docs` section (`web/src/pages/Docs.tsx`, `web/src/pages/DocPage.tsx`)
renders the repo's own `docs/*.md` in-app, bundled at build time via a Vite
glob (`web/src/lib/docs.ts`) rather than served from an API or duplicated
into `web/`, so the in-app copy can never drift from the repo copy. See
[docs/README.md](docs/README.md) for the frontmatter contract and how to
add a page, and the PRD (`prds/7-docs-section-webui.md`) for the design
rationale.

- **Audience-gated visibility.** Every doc carries a leading-fence
  frontmatter block (`title`, `order`, `audience`); only `audience: user`
  pages are listed and routable at `/docs/:slug`, ordered by `order`.
  `operator`/`design`/`contributor` pages (and anything with missing or
  malformed frontmatter) stay repo-only, so adding a page is self-describing
  and never touches `web/` code.
- **No new trust boundary.** `/docs` is public (no auth) — the content is
  non-secret and already world-readable in the repo, and onboarding docs are
  needed before a user can do anything else. `react-markdown` renders
  without `rehype-raw`, so raw HTML in a doc stays inert rather than needing
  a sanitizer; content is repo-reviewed, not user- or model-supplied.
- **Link rewriting.** A relative link to another bundled `user` page becomes
  an in-app route (`/docs/:slug`); a link to a repo-only file (`../plan.md`,
  a `design`/`operator` doc) rewrites to the pinned GitLab blob URL instead.
  `#anchor` fragments are preserved either way.
- **Build-time validation gate.** `web/scripts/check-docs.mjs` runs ahead of
  `npm run build` and fails on missing/invalid frontmatter, a duplicate
  `order` among `user` pages, a broken relative link (doc→doc or doc→img),
  reference-style links, or an oversized `docs/img/*` file; it warns
  (without failing) on a `user` page over the 60-line house-style budget.
  There is no CI yet (`plan.md`: later), so this build step is the only gate
  keeping in-app docs from rotting.

## Not yet in scope

Auto-starting a run from a GitLab label, a CI-status watching/fixing agent,
`AskUserQuestion` mid-run steering, WS wakeup for idle workers (a 3s poll is
the MVP), API-spawned worker containers (pods/VMs the server provisions
itself), per-user skills-management UI, encrypting secrets with the user's
own password instead of a shared server key, PAT least-privilege
verification, and a second (e.g. OpenAI) execution provider are all
deliberately deferred — see [plan.md](plan.md), the PRDs' Risks sections, and
`prds/4-agent-runtime-workers.md`'s "Out of scope". This document will grow
new sections (additional services, data flows, trust boundaries) as those
land.
