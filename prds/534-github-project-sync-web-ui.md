# PRD #534 — GitHub Projects sync: admin kill-switch + per-repo web UI

- **Issue**: [#534](https://github.com/vtmocanu/uzi/issues/534)
- **Priority**: Medium
- **Status**: Planned
- **Forge scope**: GitHub only. GitLab and Forgejo repos never show the control.
- **Handoff**: queued for the `Planned` uzi sweep (offline worker). Every fact below is resolved from this repo's own code and baked in. **Implementation is fully offline** (no live network, no open web). **Verification is offline for M2/M3 (vitest) and for M1's build/lint/deadcode**, with ONE carve-out: **M1's owner/non-owner/admin authorization assertions are live-DB tests** (`*_livedb_test.go`), because the `GetRepoForUser` preflight runs against a real Postgres — `Handler.q` is a concrete `*store.Queries` with no fakeable seam, exactly like the `DeleteRepo` foreign-404 precedent (`repo_remove_livedb_test.go`). A no-DB `task gate:api` runs with `UZI_TEST_DATABASE_URL` unset and **silently skips** them (the `PASS=0` trap in `.claude/rules/go.md`), so they are run via `./e2e/run-store-it.sh` (CI `test:api-store-it`). The worker writes them to that pattern and proves every offline slot; their live-DB execution is a CI / local-Postgres step (analogous to #364's own live-acceptance carve-out, one tier down). This is a pure follow-up to PRD #364, whose backend already shipped — see "What already exists".
- **Workflow-scope**: this PRD touches **no** `.github/workflows/**` file in either implementation or validation, so it is safe for the worker PAT (per `.claude/rules/prds.md`).

---

## Problem

PRD #364 shipped GitHub Projects v2 Status sync end to end — including autonomous provisioning (M4) — but decision **D10** deliberately deferred the web UI, delivering the feature as **admin-API-only**. Today, to use it a user must:

- flip the instance setting `github_project_sync_enabled` with a raw `PUT /api/admin/settings`, and
- provision/adopt/disable a board with raw `POST`/`DELETE` calls to `/api/admin/repos/{id}/github-project-sync*`.

There is no in-app surface for any of it — not even the instance toggle appears in Admin → Instance settings. So a working, shipped feature is effectively unreachable for anyone who will not open devtools.

A second gap follows from D10's admin-only choice: the sync writes to a **user's own** GitHub project with that user's **own** PAT, yet every route sits behind `RequireAdmin`. In a multi-user instance a non-admin member who owns a repo cannot turn sync on for it, even though they can already enable the board itself.

## Solution overview

Build the deferred UI, and split authorization to match who the action actually belongs to:

- **Instance kill-switch → stays admin.** A new toggle for `github_project_sync_enabled` in Admin → Instance settings, mirroring the existing bool-toggle cards (capability-aware scheduling, health, judge). It is the instance-wide on/off and a rate-limit/cost lever; off by default (unchanged).
- **Per-repo provision / adopt / disable / status → the connection owner (or an admin).** Relocate the four per-repo routes from the `/admin` group to the per-repo `/repos/{id}/…` group and authorize them owner-or-admin, using uzi's established per-repo pattern (fork on `user.IsAdmin`; a non-owner gets a 404 existence-hiding 404). The instance flag still gates them, so a member can act only after an admin has enabled the feature instance-wide. This is exactly "admin kill-switch + each user enables per repo".
- **Boards page is where per-repo linking lives.** A new "Project sync" cell in the repo table (badge + Manage), expanding one shared panel below the table — the same shape as the existing Trusted-repo / Tools cells. GitHub repos only; other forges show `—`.
- **Web only. The CLI stays out**, and this is not a punt: the `uzi` CLI authenticates with a Bearer token (`Authorization: Bearer uzc_…`, no cookie jar), while these writes require a cookie session + CSRF (`RequireAuth`). Worse, `RequireUser`'s CLI ceiling (`api/internal/middleware/cli_auth.go:85-87`) injects `IsAdmin=false` for a non-`admin_ro` token, so a CLI token could not drive the admin instance toggle either. Closing that is a cross-cutting auth change, out of scope here (this is the same reason D10 kept the CLI out).

## What already exists (PRD #364 — do NOT rebuild)

The entire backend is shipped and gated green. This PRD wires UI to it and moves four routes; it writes **no** new forge/GraphQL code and needs **no** new migration.

- **Service**: `api/internal/forgesvc/projectsync.go` — `ProjectSyncService` with `Adopt`, `Provision` (→ `provisionAndSeed`), `Disable`, `ProjectSyncStatus`. Provision creates a project titled `uzi: <repo-name>` (default when the caller passes no title; `projectsync.go:301`) and a single-select field named `uzi Status` (`projectsync.go:38`), seeding one option per board column.
- **Driver**: `api/internal/forge/projectsync.go` — the `ProjectBoardSyncer` capability interface (github-only) with `CreateProjectV2`, `CreateProjectV2Field`, `LinkProjectV2ToRepository`, etc.
- **Handlers** (today, admin-only): `api/internal/handler/admin_github_project_sync.go` — `AdoptGithubProjectSync`, `ProvisionGithubProjectSync`, `DisableGithubProjectSync`, `GetGithubProjectSyncStatus`, plus `writeProjectSyncError` (maps the forgesvc sentinels to 404/409/422).
- **Routes** (today): under `r.Route("/admin", …)` in `api/internal/handler/handler.go` — GET status behind `RequireUser + RequireAdminRO`; POST adopt / POST provision / DELETE behind `RequireAuth + RequireAdmin`.
- **Setting**: `settings.KeyGithubProjectSyncEnabled = "github_project_sync_enabled"`, default `"false"`, `validateBool` (`api/internal/settings/settings.go:84,182,307`). Already served by `GET /api/admin/settings`.
- **Sentinels**: `ErrProjectSyncDisabled` (instance kill-switch off → 409), `ErrProjectSyncAlreadyLinked` (409), `ErrProjectSyncNotGitHub` / `…Unsupported` / `…MissingScope` (422).
- **Docs**: `docs/github-project-sync.md` (audience: user) and the ARCHITECTURE.md pointer already exist.

## The ownership model (resolved — this is the guard's whole basis)

uzi is multi-user (first registrant is admin, everyone after is a non-admin member; `api/internal/handler/auth.go:201-211`). Ownership chains `repos.connection_id → forge_connections.id → forge_connections.user_id` (`api/internal/store/migrations/00002_forge.sql:8,25`). The **established per-repo authorization pattern** (do not invent a new one):

- **Owner-or-admin, forked on `user.IsAdmin`** — `PatchRepo` is the canonical example (`api/internal/handler/forge.go:929-957`): admin path calls the unscoped `SetRepoTrustFlags`; non-admin path calls `SetRepoTrustFlagsForUser` with `UserID: user.ID`, whose SQL carries `AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $N)`; zero rows → **404** (existence-hiding, never 403).
- **The reusable preflight primitive** is `GetRepoForUser(ctx, {ID: id, UserID: user.ID})` (`api/internal/store/queries/forge.sql:80-92`), already used this way by `SetRepoEnabled` (`forge.go:737`) and `DeleteRepo` (`forge.go:815`) — fetch owner-scoped, 404 on `pgx.ErrNoRows`.

Because `ProjectSyncService`'s methods take a `repoID` and resolve everything they need internally (the repo row for adopt/provision, the `github_project_links` row for disable/status), the guard needs **no new SQL**: in the handler, when `!user.IsAdmin`, call `GetRepoForUser` first and 404 on no rows; an admin skips the preflight and may target any repo. This exactly mirrors the owner-or-admin fork already in `PatchRepo`. The preflight authorizes the call regardless of what the service later fetches, so it applies uniformly to all four routes.

## Route change (server)

Move the four per-repo routes out of `/admin` and authorize them owner-or-admin. The instance setting route (`PUT /api/admin/settings`) is unchanged (stays admin).

| Today (admin-only) | New (owner-or-admin) | Group |
|---|---|---|
| `GET /admin/repos/{id}/github-project-sync` | `GET /repos/{id}/github-project-sync` | `/repos` `RequireAuth` |
| `POST /admin/repos/{id}/github-project-sync` (adopt) | `POST /repos/{id}/github-project-sync` | `/repos` `RequireAuth` |
| `POST /admin/repos/{id}/github-project-sync/provision` | `POST /repos/{id}/github-project-sync/provision` | `/repos` `RequireAuth` |
| `DELETE /admin/repos/{id}/github-project-sync` | `DELETE /repos/{id}/github-project-sync` | `/repos` `RequireAuth` |

They join the existing `r.Route("/repos", …)` `RequireAuth` sub-group (`handler.go:1078-1126`) that already holds `PUT /{id}` (SetRepoEnabled) and `PATCH /{id}` (PatchRepo) — cookie-only + CSRF, which is correct because these writes carry an admin-capable branch (the PatchRepo precedent, `handler.go:1046-1052`). GET status joins the same group. There are **no other callers** (no UI shipped; the old admin paths were reachable only via ad-hoc console/`curl`), so the admin paths are removed rather than aliased; the existing `admin_github_project_sync_test.go` cases move with them. Rename the handlers off the `admin`-implying names as a tidy-up (optional, non-blocking).

## Milestones

Structured for the repo's parallel-milestone convention. **Shared file to watch: `web/src/lib/api.ts`** is touched by both M2 (add the settings key to `AppSettings`) and M3 (add the client methods) — land M2 before M3, or expect a trivial merge. M1 (server) is independent of both web milestones.

| Phase | Milestone | Depends on | Files (disjoint except api.ts) |
|---|---|---|---|
| 1 | **M1** Route relocation + owner-or-admin guard (server) | — | `handler/handler.go`, `handler/*github_project_sync*.go`, `handler/*github_project_sync*_test.go` |
| 1 | **M2** Admin instance toggle (web) | — | `web/src/pages/AdminSettings.tsx`, `web/src/lib/api.ts` (AppSettings) |
| 2 | **M3** Per-repo Boards UI + client methods (web) | M1, M2 | `web/src/pages/Repos.tsx`, `web/src/lib/api.ts` (methods) |
| 3 | **M4** Docs refresh + full gate | M1, M2, M3 | `docs/github-project-sync.md` |

- **M1 — Route relocation + owner-or-admin guard (server).** Move the four per-repo routes to `/repos/{id}/github-project-sync*` in the `RequireAuth` sub-group; drop them from `/admin`. Add the owner-or-admin preflight: on the non-admin path, `GetRepoForUser(id, user.ID)` → 404 on `pgx.ErrNoRows` before calling the `ProjectSyncService` method; admin skips it. Keep the instance-flag gate (`ErrProjectSyncDisabled` → 409) and all existing error mapping. Instance setting route unchanged. **Tests — read the tier carefully, it is the one place this PRD is not pure-offline**: the owner/non-owner/admin-scoping assertions are **live-DB** tests (`api/internal/handler/*_livedb_test.go`, modeled on `TestDeleteRepoForeignIs404LiveDB` in `repo_remove_livedb_test.go`, using `cliLiveDB(t)`), because the `GetRepoForUser` preflight hits a real DB — `Handler.q` is a concrete `*store.Queries`, no interface to fake. Run them via `./e2e/run-store-it.sh` (CI `test:api-store-it`), **not** bare `task gate:api` (it runs with `UZI_TEST_DATABASE_URL` unset and silently SKIPS them — a green there proves nothing about these). Assertions: owner can GET/adopt/provision/disable their own repo; a non-owner member gets **404** (not 403, not 500 — existence-hiding); an admin can target any repo; the instance flag off still 409; a non-GitHub repo still 422. The existing admin-only cases in `admin_github_project_sync_test.go` move with the routes and stay pure httptest (they stub the syncer and never reach `GetRepoForUser`). No new migration; `go.mod` unchanged.
- **M2 — Admin instance toggle (web).** Add `github_project_sync_enabled: string` to the `AppSettings` type (`api.ts`, beside `capability_aware_scheduling`). Add a `GithubProjectSyncCard` to `AdminSettings.tsx` modeled exactly on `CapabilitySchedulingCard` (default-off bool toggle; sends only `github_project_sync_enabled` on save; honors the `env`-sourced disable/greying; a doc link to the sync guide). Mount it in the admin card list next to Capability-aware scheduling. Note the one divergence from the `CapabilitySchedulingCard` model: that card defaults ON, this one defaults OFF — initialize the toggle from the served value (`useState(settings.github_project_sync_enabled === "true")`, false by default), never a hard-coded `true`. **Tests** (vitest, `AdminSettings.test.tsx` pattern): the toggle renders, reflects the served value, saves via `updateSettings`, and greys when env-sourced.
- **M3 — Per-repo Boards UI + client methods (web).** Add `api.ts` methods targeting the relocated routes — `getProjectSyncStatus(id)`, `provisionProjectSync(id, {owner_kind, title})`, `adoptProjectSync(id, {project_number, owner_kind})`, `disableProjectSync(id)` — with request/response types matching the handler DTOs. Add a "Project sync" cell to the `Repos.tsx` table (badge + Manage), rendered only for GitHub repos and only for enabled repos (matching the Trusted/Tools `!r.enabled` guards; non-GitHub → `—`). **Forge detection: the `Repo` DTO has no forge field** — derive GitHub-ness from the *selected connection's* `forge_type === "github"`, reusing the exact pattern already at `Repos.tsx:362` (`connections.find((c) => c.id === connectionId)?.forge_type`). Do NOT parse `web_url` (breaks on GitHub Enterprise) and do NOT add a server-side per-repo forge field (that would be scope creep into M1). Manage expands one shared panel below the table (the Trusted-repo panel pattern, outside the horizontal-scroll container) with the states: **not linked** → Provision (owner_kind default `user`, optional title) or Adopt (owner_kind + project number); **linked** → the status readout (project number, owned-by-uzi vs adopted, item count, last-synced, last-error) + Disable. Surface the 409/422 errors as inline messages — notably `ErrProjectSyncDisabled` as "Projects sync is turned off for this instance — ask an admin to enable it" (v1 does not pre-read the instance flag for members; see D3). **Tests** (vitest, `Repos.test.tsx` pattern): the cell shows for a GitHub repo and is absent (`—`) for a GitLab repo; each action calls the right endpoint; the three panel states render; a disabled-instance 409 shows the ask-an-admin message.
- **M4 — Docs refresh + full gate.** Update `docs/github-project-sync.md`: replace the raw-API "how to enable" with the shipped UI flow (admin enables in Instance settings; the repo owner provisions/adopts on the Boards page), and state the two-tier model (admin kill-switch + per-owner per-repo) and the GitHub-only scope. Record the admin-only → owner-or-admin authorization change in `specs/ai.md` (the repo convention for design decisions; auto-applied, no user confirmation needed). Confirm `web/scripts/check-docs.mjs` still passes (frontmatter/order/links). Run the full gate: `task gate:api` (incl. `-race`, deadcode, lint ratchet, `-count=1`) and `task gate:web` green, **plus** `./e2e/run-store-it.sh` for M1's live-DB authorization tests (a plain `task gate:api` does not execute them — see M1).

## Success criteria

All verifiable without the open web or a live GitHub. Tiers are explicit: SC-1/SC-5(web) are vitest (offline); SC-2/SC-3/SC-4's **server** halves are **live-DB** (a throwaway Postgres via `./e2e/run-store-it.sh`, no network), the reason called out in M1 and the Handoff line.

1. **SC-1 — Instance toggle.** Admin → Instance settings shows a GitHub Projects sync toggle bound to `github_project_sync_enabled`; toggling and saving round-trips through `PUT /api/admin/settings`; an env-sourced value greys it. Proven offline by an `AdminSettings.test.tsx` case.
2. **SC-2 — Owner can drive their repo.** A non-admin member who owns a GitHub repo can provision/adopt/disable and read status for it through the Boards UI; the calls hit the relocated `/repos/{id}/…` routes. Proven by a **live-DB** handler test (owner path) + a `Repos.test.tsx` case (the UI calls the right client method).
3. **SC-3 — Non-owner is refused, existence-hiding.** A member acting on a repo they do not own receives **404** (never 403/500), matching `PatchRepo`. Proven by a **live-DB** handler test with a second user's repo id (skips silently under a no-DB gate — do not read a skipped run as passing).
4. **SC-4 — Admin retains reach.** An admin can drive any repo's per-repo sync (preflight skipped), and the instance toggle stays admin-only. The per-repo reach is proven by a **live-DB** handler test; the admin-only instance toggle stays pure httptest.
5. **SC-5 — Forge + gate isolation.** The per-repo cell never renders when the selected connection's `forge_type !== "github"`; no new Go dependency (`go.mod` diff clean); `task gate:api`, `task gate:web`, and `./e2e/run-store-it.sh` (M1's live-DB leg) green. Proven by a `Repos.test.tsx` case that mounts a GitLab connection (cell absent → `—`) vs a GitHub one (cell present) + the gates.

## Risks and mitigations

- **Relocating routes silently orphans the old admin paths.** No shipped UI or test consumes them except `admin_github_project_sync_test.go`, which moves in M1; the ad-hoc console recipe some operator may have used is a stopgap the UI replaces. Mitigation: remove cleanly and move the tests in the same milestone; grep for `/admin/repos/{id}/github-project-sync` across the tree before finalizing (should be zero after M1).
- **Broadening from admin-only to owner could over-expose.** The guard reuses the exact `GetRepoForUser` / `…ForUser` existence-hiding pattern already trusted for enable/disable/trust-flags; there is no weaker path. The instance flag remains an admin-held global gate on top.
- **M1's guard is NOT no-DB verifiable — do not report a skipped gate as green.** `Handler.q` is a concrete `*store.Queries` with no fakeable seam, so the owner/non-owner assertions must be `*_livedb_test.go` run under `./e2e/run-store-it.sh` (CI `test:api-store-it`). A no-DB `task gate:api` silently SKIPS them (`.claude/rules/go.md`'s `PASS=0` trap). The worker writes them to the live-DB pattern, runs every offline slot, and explicitly reports the live-DB leg as CI-executed rather than claiming a green it could not produce. CI runs it before merge; a human merges.
- **Member cannot read the instance flag to pre-gate the UI.** `GET /api/admin/settings` is admin-only, so a member's Boards UI cannot know if the feature is on. v1 shows the control and surfaces the 409 (D3) rather than adding a new member-readable settings surface; a nicer pre-gate is a follow-up.
- **api.ts touched by M2 and M3.** Sequencing M2 → M3 (or a trivial merge) avoids the only file collision; every other file is disjoint.
- **CLI drift.** Normally new functionality checks `api/cmd/uzi/` (repo convention). Here the CLI is deliberately out (bearer vs cookie+CSRF; the RequireUser admin-ceiling). Documented in the Decision log so a future reader does not "fix" the omission.

## Out of scope

- Any CLI verb (see Solution overview; a cross-cutting auth change).
- Ongoing board-column-change propagation into an existing project (PRD #364 D5 — bootstrap-once stays; still a manual step).
- A member-readable instance-flag read to pre-gate the Boards cell (D3 follow-up).
- Reverse-sync/poller, provisioning, or any backend behavior — all shipped in #364 and untouched here.

## Open questions / Decision log

- **D1 — Per-repo placement: Boards table (RESOLVED).** Put the per-repo control on the Boards repo table beside Trusted/Tools, not on a dedicated Admin sub-page. Rationale: it is a per-repo, owner-scoped action, and the table already carries the sibling owner-scoped controls (Trusted, Tools) with an established expand-panel pattern. A dedicated admin page would re-centralize a decision we are intentionally decentralizing.
- **D2 — Authorization: owner-or-admin (RESOLVED).** Per-repo routes become owner-or-admin (fork on `user.IsAdmin`, `GetRepoForUser` preflight on the member path), mirroring `PatchRepo`, not owner-only. Rationale: an admin should still be able to help provision for any user; the member path is the new capability. Instance toggle stays admin-only.
- **D3 — Instance-flag visibility to members (RESOLVED for v1: show-and-409).** The Boards cell renders for any GitHub repo the member owns; if the instance flag is off, Provision/Adopt returns 409 `ErrProjectSyncDisabled` and the UI shows "ask an admin to enable it". Rationale: avoids adding a member-readable settings surface for v1; the admin (who can read settings) is the one who enables anyway. A member-safe feature-flag read is a noted follow-up.
- **D4 — Route relocation vs. dual-mount (RESOLVED: relocate).** Move the routes to `/repos/{id}/…` and delete the `/admin` variants rather than keeping both. Rationale: no external consumer exists, and dual routes duplicate authorization surface. The admin retains reach through the owner-or-admin fork on the relocated routes.
- **D5 — CLI: out of scope (RESOLVED).** Recorded so the `api/cmd/uzi/` convention check is answered explicitly: the CLI cannot reach these routes (bearer-only auth vs cookie+CSRF; `RequireUser` forces `IsAdmin=false` for CLI tokens), so CLI support is a separate auth change, not part of this UI work.
