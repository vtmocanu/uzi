# PRD #2: Forge Integration — Repos, PRD Issues, Label-Synced Kanban

**GitLab Issue**: [vtmocanu/uzi#2](https://gitlab.example.com/vtmocanu/uzi/-/issues/2)
**Status**: Draft
**Priority**: High
**Created**: 2026-07-03
**Depends on**: PRD #1 (users, sessions, admin — the auth shell)

## Problem

uzi has an authenticated web shell (PRD #1) but no connection to the git forge where the work lives. Users cannot see their repos, their PRD-labeled issues, or a kanban view of the pipeline; the AI factory has nothing to act on. plan.md requires: see GitLab issues where our bot has rights, pick repos in the UI, a per-repo kanban board based on GitLab labels kept in sync both ways, and PRD-labeled issues sanity-checked for a PRD link.

## Solution Overview

A **forge-generic integration layer** (interface first; GitLab driver now, Forgejo later) using **per-user bot tokens**: each user creates their own GitLab bot account, adds it to the projects they want uzi to work on, and connects its PAT in uzi. uzi then lists the bot-visible projects, imports PRD-labeled issues, and renders a per-repo kanban board whose columns are forge labels (GitLab-board semantics, example-app board as the reference: `In Progress` / `Upcoming` / `Later`; [kan.bn](https://kan.bn/kan/roadmap) as UI style reference). Drag-and-drop writes label changes back to the forge; a polling sync engine pulls forge changes in.

Neither inspiration implements this — both were audited and the audit fact-checked (2026-07-03, see decision log). dot-agent-deck was also checked: it is a TUI for monitoring agent sessions with no forge/label/kanban layer — N/A here.

| Concern | multica does | bottega does | uzi will do |
|---|---|---|---|
| Forge credentials | GitHub App/installation, env-configured, per-workspace binding; no per-user forge token | None — agents use the server's ambient `gh`/git identity (single shared identity, weakness) | **Per-user bot PAT**, encrypted at rest (AES-256-GCM, multica's `secretbox` util pattern — GCM despite the NaCl-ish name), scoped to that user's connection |
| Forge abstraction | None — hand-rolled `net/http` GitHub-only code | None — GitHub-only `gh` CLI shell-outs | **`Forge` interface from day one**; GitLab driver first, Forgejo later without touching callers |
| Repo registry | Denormalized JSONB array on workspace (no FK, no forge id — weakness) | Local filesystem paths typed by user, no forge discovery | **Normalized `repos` table keyed by stable forge project id**, discovered via membership API, user enables per repo |
| Issue source | Native tracker; never imports forge issues | Local SQLite tasks; never fetches issues | **Forge is the source of truth**; uzi caches PRD-labeled issues, never forks them |
| Kanban columns | Fixed local status enum (7 values) | Local 4-value status enum (board renders 3 columns) | **Columns = forge labels** (GitLab board lists semantics), configurable per repo, synced two-way |
| Issue↔work mapping | Regex on `PREFIX-NUMBER` in PR text (brittle, weakness) | Regex on branch name `task/{id}-` (brittle, weakness) | **Explicit FK rows** `(repo_id, forge_issue_iid)` — no text-convention parsing |
| Sync | Webhook-only, no reconciliation (missed webhook = silent drift, weakness) | Webhook for PR re-trigger only; board never syncs | **Polling with `updated_after` + periodic full reconcile + on-demand refresh** (laptop MVP can't receive webhooks); webhook support deferred, designed-for |
| Rate limits/caching | None (barely calls the forge) | None | Honor `RateLimit-*`/429+`Retry-After`, per-connection poll budget, persisted issue cache |

Patterns copied: multica's secretbox encryption-at-rest and persisted-truth conflict handling; bottega's per-user credential isolation, membership authz, and refuse-to-start secret guard (already in PRD #1).

## Technical Design

### Forge abstraction

Go interface in `api/internal/forge/` — drivers implement, callers never import a driver:

```go
type Forge interface {
    VerifyToken(ctx) (BotIdentity, error)          // GET /user
    ListProjects(ctx) ([]Project, error)           // membership=true, min_access_level=Developer
    ListLabels(ctx, projectID) ([]Label, error)
    EnsureLabels(ctx, projectID, labels []Label) error
    ListIssues(ctx, projectID, opts) ([]Issue, error)  // labels, state (always queried state=all), updated_after; Issue carries iid, title, state, labels, description, author, web_url, updated_at
    UpdateIssueLabels(ctx, projectID, issueIID, add, remove []string) error
}
```

- **GitLab driver**: official Go client, import path `gitlab.com/gitlab-org/api/client-go/v2` (successor of `xanzy/go-gitlab`). Label moves use the atomic `add_labels`/`remove_labels` issue update. Base URL per connection — self-hosted-first.
- **Forgejo driver**: out of scope; the interface, schema (`forge_type` column), and UI copy stay forge-neutral. Note: Forgejo has no atomic add+remove label call (separate POST/DELETE requests), so `UpdateIssueLabels` may be non-atomic there; single-column enforcement is best-effort on non-GitLab drivers (Forgejo's native scoped/exclusive labels can close that gap later).
- All calls go through one wrapper handling 429/`Retry-After`, `RateLimit-*` headers, timeouts (multica's untimeouted `http.DefaultClient` is a known wart to avoid), pagination, and **secret redaction: the PAT and `Authorization`/`PRIVATE-TOKEN` headers must never reach logs or error strings** (redaction unit test required, M5).

### SSRF guard (base_url)

The server makes authenticated outbound HTTP to `base_url`, so it must not be free-text: allowed forge base URLs come from an **admin-controlled allowlist** env `FORGE_ALLOWED_BASE_URLS` (comma-separated, default `https://gitlab.example.com`). Connect UI offers only allowlisted URLs. `https` scheme required. This closes SSRF (cloud metadata endpoints, internal services, loopback) without per-request IP filtering; if free-text URLs are ever allowed, private/loopback/link-local IP ranges must be resolved and rejected at that point.

### Bot account model (user-managed, per plan.md)

- Each user creates their **own GitLab bot account** (e.g. `uzi-bot-vmocanu`) and a PAT with scope `api` (label writes need it; `read_api` is read-only), then adds the bot as **Developer** to each project uzi should see. Documented procedure + `glab`-based helper script ships in M6.
- uzi-side: connect the PAT in Settings → Forge. uzi verifies it (`VerifyToken`), displays the bot identity, and stores the token **encrypted at rest**: AES-256-GCM (multica secretbox pattern), master key from env `UZI_SECRET_KEY` (base64 32 bytes), **refuse-to-start guard** like `JWT_SECRET`. Build the encryption helper as a **generic secretbox utility**, not PAT-specific code — plan.md already earmarks it for per-user Anthropic OAuth tokens in the upcoming agent PRD. Never returned to the client after save; re-connect to rotate a PAT. **Known limitation (document in M6): rotating `UZI_SECRET_KEY` itself invalidates all stored tokens — every user must reconnect; there is no re-encrypt path in MVP.**
- Auto-creating bot accounts and role enforcement are deferred (plan.md "later stuff").

### Schema (goose migrations, extends PRD #1 DB)

```sql
forge_connections (
  id uuid PK default gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  forge_type text NOT NULL CHECK (forge_type IN ('gitlab')),  -- 'forgejo' later
  base_url text NOT NULL,                 -- must be in FORGE_ALLOWED_BASE_URLS
  bot_username text NOT NULL,
  bot_forge_user_id bigint NOT NULL,
  token_ciphertext bytea NOT NULL,        -- AES-256-GCM(PAT)
  created_at timestamptz NOT NULL DEFAULT now(),
  last_verified_at timestamptz,
  UNIQUE (user_id, forge_type, base_url)
)

repos (
  id uuid PK default gen_random_uuid(),
  connection_id uuid NOT NULL REFERENCES forge_connections ON DELETE CASCADE,
  forge_project_id bigint NOT NULL,       -- stable forge id, not path
  path_with_namespace text NOT NULL,
  web_url text NOT NULL,
  default_branch text,
  enabled boolean NOT NULL DEFAULT false,
  UNIQUE (connection_id, forge_project_id)
)
-- rows are upserted (enabled=false) whenever the membership list is fetched,
-- so enable/disable always has an id to target

board_columns (
  id uuid PK default gen_random_uuid(),
  repo_id uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
  label_name text NOT NULL,
  position int NOT NULL,
  UNIQUE (repo_id, label_name)
)

issues (                                   -- cache of forge truth, never authoritative
  id uuid PK default gen_random_uuid(),
  repo_id uuid NOT NULL REFERENCES repos ON DELETE CASCADE,
  forge_issue_iid bigint NOT NULL,
  title text NOT NULL,
  state text NOT NULL,                     -- opened|closed
  labels jsonb NOT NULL DEFAULT '[]',
  web_url text NOT NULL,
  author text,                             -- shown on the card
  has_prd_link boolean NOT NULL DEFAULT false,  -- computed at fetch time from the description (description itself not stored)
  forge_updated_at timestamptz NOT NULL,
  synced_at timestamptz NOT NULL,
  UNIQUE (repo_id, forge_issue_iid)
)
```

### Kanban board semantics (GitLab-board compatible)

- Columns per repo: implicit **Open** (no column label) + ordered label columns (default seeded: `In Progress`, `Upcoming`, `Later`) + implicit **Closed** (requires syncing `state=all`). Configurable in board settings (pick any repo label as a column). Default labels are created on the forge on first board open if missing — a deliberate, documented side effect.
- Card is in a column iff the issue carries that label (exactly GitLab's board-list semantics; GitLab allows an issue in multiple lists). **uzi enforces one column label per issue**: moving a card issues one `UpdateIssueLabels(add=[target], remove=[other column labels])` call. Moving to **Open** removes all column labels; moving to **Closed** is unsupported in MVP (closing/reopening stays on the forge; card links to the issue). Issues arriving from the forge with multiple column labels are shown in the highest-positioned column with a conflict badge; the next uzi-side move normalizes them.
- Board shows **only PRD-labeled issues** (the factory works PRDs, plan.md). Each card gets a **PRD-link sanity check** computed at fetch time from the issue description: it must contain either a relative path or an absolute URL to a PRD file — regex `(?i)(?:https?://\S+/-/blob/[^\s)]+/)?prds/[\w.-]+\.md(?:[#?][^\s)]*)?`. Failing cards render a warning badge and are excluded from future agent pickup.

### Sync engine

- **Forge is the source of truth.** uzi's `issues` table is a cache; no uzi-only board state beyond column config.
- **Incremental pull**: per enabled repo, poll `ListIssues(labels=PRD, state=all, updated_after=HWM)` on `FORGE_POLL_INTERVAL` (default `60s`). High-water-mark = max `updated_at` **returned by the forge** (never client clock — skew would drop updates); GitLab's `updated_after` is inclusive at second granularity, so boundary rows are re-fetched and deduped by upsert. Assumption to verify in M1: a label change bumps GitLab's issue `updated_at`; if not, incremental polling degrades and the reconcile interval becomes the real freshness floor.
- **Full reconcile** (every `FORGE_RECONCILE_EVERY` polls, default 10): fetch the complete PRD-labeled set (`state=all`, no `updated_after`), diff against cached IIDs, upsert everything, **evict cache rows absent from the fresh set** — this is the only way to observe de-labeling and deletion, which the incremental filter structurally cannot return. Manual Refresh triggers the same full sync.
- **Push**: card moves apply the label change via the API **first**; on success, update the cache; on failure, the card snaps back with the API error. No optimistic divergence.
- **Conflicts**: last-writer-wins at the forge (its native semantics). If a poll shows the forge changed an issue uzi displays, forge state replaces cache (persisted-truth-over-event-claim, multica's lesson).
- **Freshness contract**: content edits and column-label changes appear within one poll interval; de-labeling, close/reopen visibility gaps, and deletions are caught within one reconcile interval.
- **Repo disable** stops its poller and hides the board; cache rows are retained (purged only when the connection is deleted, via FK cascade).
- **Webhooks** (deferred): the laptop compose stack is not reachable from gitlab.example.com. Design keeps a seam: the sync engine consumes a `ChangeSource` (poller now; webhook receiver later authenticated via GitLab's HMAC-SHA256 signing token — preferred — or legacy `X-Gitlab-Token` compared in constant time).

### API surface (all authenticated, PRD #1 session/CSRF)

- `POST/GET/DELETE /api/forge/connections` (+ `POST .../verify`)
- `GET /api/forge/connections/:id/projects` — live membership list; upserts `repos` rows (`enabled=false`) so they are addressable
- `PUT /api/repos/:id` (enable/disable) · `GET /api/repos` (enabled repos for current user)
- `GET /api/repos/:id/board` (columns + cards) · `PUT /api/repos/:id/board/columns` (configure)
- `POST /api/repos/:id/issues/:iid/move {to_column}` · `POST /api/repos/:id/sync` (manual full sync — delivered with the board in M4)

All repo/board endpoints authorize through the owning connection's `user_id` (bottega's membership-authz shape) — users only ever see their own bot's world.

### Configuration (env, extends PRD #1 table)

| Var | Default (compose) | Notes |
|---|---|---|
| `UZI_SECRET_KEY` | — (required, boot guard) | base64 32B, `openssl rand -base64 32`; encrypts bot PATs; rotation invalidates stored tokens |
| `FORGE_ALLOWED_BASE_URLS` | `https://gitlab.example.com` | comma-separated allowlist, https only (SSRF guard) |
| `FORGE_POLL_INTERVAL` | `60s` | per enabled repo |
| `FORGE_RECONCILE_EVERY` | `10` | full reconcile every Nth poll |
| `FORGE_HTTP_TIMEOUT` | `15s` | every forge call |

### UI (extends PRD #1 shell)

1. Settings → Forge — connect/verify/rotate/delete bot PAT (base URL picked from the allowlist); shows bot identity + last verified.
2. Repos — membership list with enable toggles; enabled repos appear in the sidebar picker.
3. Board (per repo) — kanban: Open | label columns | Closed; drag-drop moves; PRD-link warning badges; conflict badges; Refresh button; column settings.

## User Journey

1. User creates `uzi-bot-<name>` on gitlab.example.com per docs, PAT scope `api`, adds it as Developer to a project.
2. In uzi: Settings → Forge → paste PAT → uzi verifies and shows the bot identity.
3. Repos page lists the bot's projects → user enables one.
4. Board shows PRD-labeled issues in Open; default columns seeded on the forge.
5. User drags a card to `In Progress` → label appears on the GitLab issue (visible in GitLab's own board too).
6. Someone moves the issue in GitLab's board → uzi reflects it within one poll interval (de-labeling/deletion: within one reconcile interval).
7. An issue whose description lacks a PRD link shows a warning badge and is excluded from agent pickup.

## Milestones

- [ ] **M1 — Forge abstraction + connection management**: `Forge` interface + GitLab driver (official client v2, wrapper with timeouts/429/pagination/redaction); AES-256-GCM token encryption + `UZI_SECRET_KEY` boot guard; `FORGE_ALLOWED_BASE_URLS` SSRF guard; connections API + Settings UI (connect/verify/rotate/delete). Verify the label-change-bumps-`updated_at` assumption against gitlab.example.com and record the result in this PRD.

  > **M1 verification finding (2026-07-03, coder):** Not verified against a live forge — no bot PAT was available, and a label change requires write access to bump `updated_at`. From the official [Issues API docs](https://docs.gitlab.com/api/issues/): `updated_after` is documented as "Return issues updated **on or after** the given time" (ISO 8601), which confirms the inclusive-boundary assumption the incremental poller relies on (boundary rows are re-fetched and deduped by upsert). The docs do **not** explicitly state that adding/removing a label bumps `updated_at`. Label writes go through the issue-update endpoint (`add_labels`/`remove_labels` on `PUT /issues/:iid`), which returns a fresh `updated_at`, so the assumption is very likely to hold for API-driven moves (uzi's own path); it is less certain for board/quick-action edits made in the GitLab UI. Either way the design does not depend on it: the periodic **full reconcile with eviction** (`FORGE_RECONCILE_EVERY`) is the guaranteed freshness floor and observes de-labeling/deletion that the incremental filter structurally cannot. Recommend the live check (uzi move → poll interval) be run in M5 once a bot PAT is supplied.
- [ ] **M2 — Repo discovery + selection**: membership listing with `repos` upsert, enable/disable API + Repos page + sidebar picker; per-user authz on every repo path.
- [ ] **M3 — PRD issue import**: issue cache schema, PRD-label fetch (`state=all`), PRD-link sanity check, list view with badges.
- [ ] **M4 — Kanban board**: column config (default set seeded on forge), board API + drag-drop UI, single-column-label enforcement (incl. move-to-Open), move-writes-through-to-forge with snap-back on failure, manual `POST …/sync` + Refresh button (reuses M3 import as a full sync).
- [ ] **M5 — Sync engine + hardening**: poller with server-clock high-water-mark, full reconcile with eviction, rate-limit handling; E2E: move in uzi → verify via `glab`; move in GitLab → appears in uzi ≤ poll interval; de-label in GitLab → evicted ≤ reconcile interval; concurrent-move conflict test; auditor pass on token handling incl. log-redaction test.
- [ ] **M6 — Bot procedure + docs**: `docs/gitlab-bot-setup.md` + `glab` helper script (create PAT guidance, add bot to project); README/ARCHITECTURE updates; config reference incl. `UZI_SECRET_KEY` rotation warning and default-label seeding note.

## Success Criteria

- Fresh stack + a real bot PAT → enabled repo → PRD issues on the board in under 5 minutes following the docs.
- Card drag in uzi is visible as a label change in GitLab (and its native board) immediately; GitLab-side label moves/edits appear in uzi within one poll interval; de-labeling/deletions within one reconcile interval; no drift after 24h of mixed edits (reconcile pass proves it).
- Bot PATs never stored or logged in plaintext (redaction test enforced); DB dump alone cannot recover them; server refuses to start without `UZI_SECRET_KEY`; forge URLs restricted to the allowlist.
- Adding a Forgejo driver requires no changes to callers, schema shape, or UI flows (design review checkpoint, not implementation); the label-swap atomicity guarantee is explicitly GitLab-only.
- Demonstrably ≥ inspirations: real forge-issue kanban with two-way label sync (neither has one), per-user revocable bot identity (bottega has shared ambient identity), reconciliation-by-design (multica is webhook-only).

## Risks

- **Two-way sync correctness** (races between drag and forge edits) — mitigation: forge-first writes, no optimistic divergence, periodic full reconcile with eviction, dedicated concurrent-move E2E in M5.
- **Label semantics drift** (humans add multiple column labels forge-side) — mitigation: deterministic display rule + conflict badge + normalize-on-next-move.
- **PAT security** (write-scoped tokens at rest) — mitigation: AES-256-GCM encryption, boot guard, never echoed to client, redaction test, auditor pass in M5.
- **Rate limits on gitlab.example.com** (poll × repos; N users watching the same busy project multiply pollers, since budgets are per-connection) — mitigation: `updated_after` high-water-mark, per-connection budget, backoff on 429; poll interval env-tunable (raise it as user count grows).
- **`updated_after` blind spots** (label changes possibly not bumping `updated_at`; de-label/delete invisible to the incremental filter) — mitigation: M1 verification task + reconcile-with-eviction as the guaranteed freshness floor.
- **Scope creep into agent execution** (clients acting on issues) — mitigation: this PRD ends at the synced board; agent/client architecture is the next PRD.

## Decision Log

- 2026-07-03 (user): forge-generic design, GitLab first, Forgejo later; review inspirations' GitHub implementations first.
- 2026-07-03 (research): bottega has no forge API layer (local kanban, shared ambient `gh` identity, webhook-retrigger only); multica is a native tracker with a one-way GitHub-App PR mirror (no issue import, no label sync, webhook-only, JSONB repo registry). dot-agent-deck: no forge layer, N/A. Core of this PRD is greenfield; patterns adopted: multica secretbox + persisted-truth conflict handling, bottega per-user credential isolation + membership authz.
- 2026-07-03 (AI): per-user bot PAT over GitLab-App/OAuth (matches plan.md bot-account model; self-hosted-friendly; Forgejo-portable); polling over webhooks for laptop MVP (compose stack unreachable from the forge); columns-are-labels with single-column-label enforcement (GitLab-board compatible, avoids multi-list ambiguity); explicit FK issue mapping over text-convention regex (both inspirations' weakness).
- 2026-07-03 (AI, post-review): SSRF closed via `FORGE_ALLOWED_BASE_URLS` allowlist; reconcile fully specified (state=all, no HWM, diff + evict) with `FORGE_RECONCILE_EVERY` and a two-tier freshness contract (poll vs reconcile interval); HWM from server-returned `updated_at` with inclusive-boundary dedupe; `state=all` mandated so the Closed column works; repos upserted `enabled=false` on listing (fixes enable-by-id chicken-and-egg); manual sync moved to M4; Forgejo atomicity caveat on `UpdateIssueLabels`; `UZI_SECRET_KEY` rotation limitation documented; wrapper redaction requirement + test; webhook seam reworded to GitLab signing token (HMAC) preferred over legacy `X-Gitlab-Token`; PRD-link regex concretized, `has_prd_link` computed at fetch time. Fact-check (all claims confirmed): client import path corrected to `…/client-go/v2`; multica's secretbox is AES-256-GCM despite the NaCl name; bottega wording tightened to 4-value enum / 3 rendered columns.
