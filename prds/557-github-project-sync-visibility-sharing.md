# PRD #557 — GitHub Projects sync: board visibility + collaborator sharing from the UI

- **Issue**: [#557](https://github.com/vtmocanu/uzi/issues/557)
- **Priority**: Medium
- **Status**: Planned
- **Forge scope**: GitHub only. GitLab and Forgejo repos never show the control (the capability interface is github-only; `forge/projectsync_test.go:502` asserts the negative — the gitlab/forgejo drivers do not implement it).
- **Depends on**: PRD #534 (merged, `44ee36030`). This is a pure follow-up: it extends #534's per-repo Project-sync panel and adds routes to the `/repos/{id}/github-project-sync*` group #534 relocated. Every fact below is resolved from the merged code.
- **Workflow-scope**: touches **no** `.github/workflows/**` file in either implementation or validation, so it is safe for a uzi worker PAT (per `.claude/rules/prds.md`).
- **Internet-independence**: all external facts (the GitHub GraphQL surface) are resolved into this document as stated facts, so an offline worker needs no open-web access. See "GitHub API facts (resolved)" and the **no-live-forge-test caveat** in Risks.

---

## Problem

PRD #534 shipped the per-repo Project-sync UI — Provision, Adopt, Disable, and a status readout, owner-or-admin scoped, on the Boards page. But it left a visibility gap that bites the common self-hosting shape:

- A user connects a forge with a **separate bot account's PAT** (e.g. `vtmocanu-uzi`) and keeps their personal account (`vtmocanu`) out of uzi.
- **Provision** creates the board with that PAT, so the board is **owned by the bot account** and — per GitHub's default — **private**. GitHub gates a private project on project-level read access; **repo access alone is not enough** ("only users granted at least read access can see the project"). So the human's own account cannot see the board uzi just made.
- There is no in-app way to change that. The user must leave uzi, open GitHub's project settings, and flip visibility or add themselves as a collaborator by hand — the exact "open devtools / raw API" friction #534 set out to remove, one layer over.

## Solution overview

Add a **Board access** section to #534's linked-state Project-sync panel, with two controls, both github-only and both reusing #534's owner-or-admin authorization:

- **Visibility toggle (full round-trip).** Read the board's current `public` flag and let the owner flip it. `ProjectV2.public` is both readable and writable, so the toggle reflects true state and writes it back.
- **Write-only sharing.** A "Share with a GitHub user (Reader)" field: type a username, uzi grants that user **Reader** access; a revoke control sets the role back to none. This is **write-only by necessity** — GitHub exposes no readable collaborator list (see below) — so the panel grants/revokes but does not render an authoritative current-collaborator list.

The instance kill-switch (`github_project_sync_enabled`) and the owner-or-admin guard from #534 both apply unchanged; a member can act only after an admin has enabled the feature and only on a repo they own (admins skip the preflight).

## GitHub API facts (resolved)

Verified against the live GitHub GraphQL schema during PRD authoring (introspection); baked here so the implementer needs no open web. **These are the design's load-bearing external facts and NO automated gate re-verifies them** — every Go test uses fakes (see the Risks caveat). Re-introspect before M1 if in doubt; the highest-leverage one is "no collaborators read field", which the whole write-only UX rests on.

- **`ProjectV2.public` is readable and writable.** The type exposes `public`, `title`, `url`. Visibility is a true round-trip.
- **`ProjectV2` has NO `collaborators` field.** There is no way to read the current collaborator list back from a ProjectV2. Consequence: sharing is **write-only** — uzi can grant/revoke but cannot enumerate current collaborators. This is the decision driver for D2.
- **Set visibility**: `mutation updateProjectV2(input: {projectId, public})`. `UpdateProjectV2Input` accepts `projectId`, `title`, `shortDescription`, `readme`, `closed`, `public` — we use `projectId` + `public` only.
- **Grant/revoke collaborator**: `mutation updateProjectV2Collaborators(input: {projectId, collaborators: [{userId, role}]})`. Role enum `ProjectV2Roles` = `NONE | READER | WRITER | ADMIN`. Grant Reader = `READER`; revoke = `NONE`. The mutation **sets** the collaborator's role (upsert semantics), so a duplicate Reader grant is expected to be a **no-op success** rather than an error — see the Risks caveat (this idempotency is not gate-verified).
- **Resolve a username to its node id**: `user(login: $login){ id }`. For a **non-existent** login GitHub returns `data.user = null` AND an `errors` entry of `type: "NOT_FOUND"`. This is why bad-username detection needs a driver change, not a plain reuse — see "New surface" M1.
- **Reading visibility by node id**: `node(id: $projectId) { ... on ProjectV2 { public } }` — the link row already stores the project's node id (`github_project_links.project_node_id`, read via `GetGithubProjectLinkByRepo`), so no re-resolution is needed.
- **Scope**: both mutations require the PAT `project` scope, the same scope the existing Projects v2 mutations need. `ensureProjectScope` (`forgesvc/projectsync.go:1205`) already preflight-checks it, so no new scope handling.

## What already exists (PRD #534 / #364 — do NOT rebuild)

- **Routes** (per-repo `RequireAuth` group, cookie + CSRF, owner-or-admin), `api/internal/handler/handler.go:1068-1071`:
  - `GET /repos/{id}/github-project-sync` (status), `POST …` (adopt), `POST …/provision`, `DELETE …` (disable).
- **Handlers**: `api/internal/handler/admin_github_project_sync.go` (name kept post-relocation). Owner-or-admin preflight pattern, verbatim to reuse: `if !user.IsAdmin { GetRepoForUser({ID: id, UserID: user.ID}) → 404 on pgx.ErrNoRows; slog.Error + 500 on any other error }`. Error mapping: `writeProjectSyncError` (`admin_github_project_sync.go:274-289`) maps the forgesvc sentinels — Disabled/AlreadyLinked → 409; NotGitHub/Unsupported/MissingScope → 422; `pgx.ErrNoRows` → 404; **default → 500**.
- **Service**: `api/internal/forgesvc/projectsync.go` — `ProjectSyncService` with `projectSyncPreamble` (instance-flag gate + GitHub check + build forge + `ProjectBoardSyncer` assertion + `ensureProjectScope`), `GetGithubProjectLinkByRepo` (returns the link row incl. `ProjectNodeID`).
- **Driver**: `api/internal/forge/projectsync.go` — the `ProjectBoardSyncer` capability interface (github-only, NOT part of the neutral `Forge` interface) **and** the `(g *github)` implementations of it (e.g. `CreateProjectV2Field` at `projectsync.go:547`, `LinkProjectV2ToRepository` at `projectsync.go:594`). The shared low-level GraphQL helper `graphqlDo` lives in `github.go:902`; the compile-time assertion `var _ ProjectBoardSyncer = (*github)(nil)` is at `projectsync.go:633`. `ResolveProjectV2OwnerID` (`github.go:454`) runs `user(login){id}` for the `OwnerUser` kind.
- **Web**: `web/src/lib/api.ts` — `ProjectSyncStatus` (line 413), `ProjectSyncOwnerKind` (422), client methods `getProjectSyncStatus`/`provisionProjectSync`/`adoptProjectSync`/`disableProjectSync` (2838-2858). `web/src/pages/Repos.tsx` — the Project-sync panel (1120-1266): lazy-fetches status on open, renders the linked readout (`<dl>`, 1153-1178) + Disable, or the not-linked Provision/Adopt forms; GitHub-ness via `connections.find((c) => c.id === connectionId)?.forge_type === "github"` (line 496).
- **Instance flag**: `github_project_sync_enabled` (admin toggle in `AdminSettings.tsx`, gate reused as `ErrProjectSyncDisabled` → 409). Unchanged here.
- **Live-DB test harness**: `api/internal/handler/github_project_sync_livedb_test.go` builds the Handler via `cliLiveDB(t)` with **`projectSync == nil`** — so it discriminates **authorization only**: a request past the preflight hits the `h.projectSync == nil` guard and returns **500**; a request blocked by the preflight returns **404** *before* that guard. It therefore proves the 404-vs-500 boundary (owner/foreign/admin) and **cannot** prove any service behavior. This is the exact tier constraint the new tests must respect (see M3/SC).
- **Docs**: `docs/github-project-sync.md` (audience: user) exists and is the doc to extend.

## New surface this PRD adds

### Driver (`api/internal/forge`, github-only) — M1

Add to the `ProjectBoardSyncer` interface + implement the `(g *github)` methods in **`forge/projectsync.go`** (alongside `CreateProjectV2Field`, using `graphqlDo` from `github.go`). Because `ProjectBoardSyncer` is an **optional capability interface, not the neutral `Forge` interface**, this touches **neither** `gitlab.go`/`forgejo.go` **nor** the six `Forge` fakes. It touches the two test **syncer** fakes that implement `ProjectBoardSyncer` — `fakeProjectSyncer` (`forgesvc/projectsync_test.go`, reused by `projectsync_reverse_test.go`) and `statefulProject` (`forgesvc/projectsync_convergence_test.go:67`) — which each need the new methods stubbed to keep the `forgesvc` test package compiling. (`forge/projectsync_test.go` defines no fake and needs no change.)

- `GetProjectV2Visibility(ctx, projectID string) (bool, error)` — `node(id){ ... on ProjectV2 { public } }`.
- `SetProjectV2Visibility(ctx, projectID string, public bool) error` — `updateProjectV2`.
- `SetProjectV2Collaborator(ctx, projectID, userID string, role ProjectV2CollaboratorRole) error` — `updateProjectV2Collaborators`. A small `ProjectV2CollaboratorRole` type with `RoleReader`/`RoleNone` mapping to `READER`/`NONE` keeps the call sites readable and the enum closed.
- `ResolveUserNodeID(ctx, login string) (string, error)` — `user(login){ id }`, **returning a typed `forge.ErrGitHubUserNotFound`** when the login does not resolve. Plain reuse of `ResolveProjectV2OwnerID` is NOT sufficient: `graphqlDo` collapses every GraphQL `errors[]` entry into one redacted string (`github.go:931-936`) with no `errors.Is`-able type, and returns it *before* the caller can inspect `id == ""`. So M1 also makes the not-found case distinguishable: capture the GraphQL error `type` on the response envelope (`graphqlResponse.Errors[].Type`) and have `graphqlDo` return a typed error the caller can `errors.As`/`errors.Is`, so `ResolveUserNodeID` maps GitHub's `NOT_FOUND` to `ErrGitHubUserNotFound` while every other error propagates unchanged (so transient/permission failures stay 500, not mis-reported as "bad username"). This is a small, low-risk change to `graphqlDo` (existing callers still just return the error up).

### Service (`api/internal/forgesvc/projectsync.go`) — M2

Each method resolves the link row (`GetGithubProjectLinkByRepo` → `ProjectNodeID`) and builds the forge through the existing `projectSyncPreamble` gates (instance flag, GitHub-only, scope). Sharing resolves the username via the new `ResolveUserNodeID`.

- `GetVisibility(ctx, repoID) (bool, error)`
- `SetVisibility(ctx, repoID, public bool) error`
- `ShareWithUser(ctx, repoID, username string) error` — resolve login, set `READER`.
- `Unshare(ctx, repoID, username string) error` — resolve login, set `NONE`.

New sentinel: `ErrProjectSyncUserNotFound` — returned when `ResolveUserNodeID` yields `ErrGitHubUserNotFound`; mapped to **422** by `writeProjectSyncError`. A repo with no link row surfaces the existing `pgx.ErrNoRows` → **404** ("project sync not enabled for this repo"), same as the status route.

### Handlers + routes (`admin_github_project_sync.go`, `handler.go`) — M3

Four routes joining #534's per-repo `RequireAuth` group, each with the verbatim owner-or-admin preflight + the instance-flag gate (inherited via the service) + `writeProjectSyncError` mapping (extended for `ErrProjectSyncUserNotFound` → 422):

| Method | Path | Body | Success |
|---|---|---|---|
| GET | `/repos/{id}/github-project-sync/visibility` | — | 200 `{ "public": bool }` |
| PUT | `/repos/{id}/github-project-sync/visibility` | `{ "public": bool }` | 200 `{ "public": bool }` |
| POST | `/repos/{id}/github-project-sync/collaborators` | `{ "username": string }` | 204 (grants Reader) |
| DELETE | `/repos/{id}/github-project-sync/collaborators` | `{ "username": string }` | 204 (revokes) |

Role is fixed to Reader in v1 (no `role` field in the grant body — D3); the endpoint shape leaves room to add one later without a breaking change.

### Web (`web/src/lib/api.ts`, `web/src/pages/Repos.tsx`, `web/src/mocks/mockApi.ts`) — M4

- **api.ts**: `getProjectSyncVisibility(id)`, `setProjectSyncVisibility(id, public)`, `shareProjectSync(id, username)`, `unshareProjectSync(id, username)`, with request/response types.
- **Repos.tsx**: a **Board access** sub-block inside the existing linked readout (only when `syncLinked`, GitHub-only guard already in place). It contains:
  - a **visibility Toggle** (fetch current `public` when the linked panel opens; PUT on change; an inline "internet-visible" caption when public);
  - a **Share** field (username input + "Share (Reader)" button → POST; each granted username shows a transient success confirmation; a revoke affordance on the just-granted entries within the session);
  - a one-line note stating GitHub does not expose the current sharing list, so the panel grants/revokes rather than listing (honest UX for the D2 constraint).
- **mockApi.ts**: the four mock methods must be **realistic enough that the vitest error/toggle cases are not vacuous** — a mutable visibility value the toggle round-trips, and a **422 for a designated bad username** (e.g. `"nouser"`) so the bad-username inline-error test exercises a real failure rather than a resolved success.

## Milestones

The chain is driver → service → handler → web → docs, genuinely sequential. Shared files are disjoint across milestones except `handler.go` (M3 only) and `api.ts` (M4 only). Each milestone's exit criterion is its own component gate green (`task gate:api` for M1-M3, plus `./e2e/run-store-it.sh` for M3's live-DB leg; `task gate:web` for M4); the aggregate is not a separate milestone.

| Phase | Milestone | Depends on | Files |
|---|---|---|---|
| 1 | **M1** Driver methods + typed not-found + fake stubs | — | `forge/projectsync.go`, `forge/github.go`, `forge/projectsync_test.go` (or `github_test.go`), `forgesvc/projectsync_test.go` + `projectsync_convergence_test.go` (stub the new fake methods) |
| 2 | **M2** Service methods + sentinel | M1 | `forgesvc/projectsync.go`, `forgesvc/projectsync_test.go` |
| 3 | **M3** Handlers + routes + owner-or-admin guard | M2 | `handler/handler.go`, `handler/admin_github_project_sync.go`, `handler/admin_github_project_sync_test.go`, `handler/github_project_sync_livedb_test.go` |
| 4 | **M4** Web client + Board-access panel + mocks | M3 | `web/src/lib/api.ts`, `web/src/pages/Repos.tsx`, `web/src/pages/Repos.test.tsx`, `web/src/mocks/mockApi.ts` |
| 5 | **M5** Docs refresh | M1-M4 | `docs/github-project-sync.md`, `specs/ai.md` |

- **M1 — Driver.** Add the four methods to `ProjectBoardSyncer` and implement them on `(g *github)` in `forge/projectsync.go` via `graphqlDo`, mirroring `CreateProjectV2Field`. Add the typed not-found path (`graphqlResponse.Errors[].Type`, typed error from `graphqlDo`, `ErrGitHubUserNotFound`). Stub the two fakes (`fakeProjectSyncer`, `statefulProject`) so `forgesvc` still builds. **Tests** (driver-level, no DB, no open network — the existing github driver tests drive an httptest GraphQL server): assert `GetProjectV2Visibility`/`SetProjectV2Visibility`/`SetProjectV2Collaborator` issue the right query + variables (project node id, `READER`/`NONE`), and that a **canned `NOT_FOUND` envelope** makes `ResolveUserNodeID` return `ErrGitHubUserNotFound` while a generic error does not. `go.mod` unchanged; no migration.
- **M2 — Service.** Add the four service methods + `ErrProjectSyncUserNotFound`, each through `projectSyncPreamble`, reading `ProjectNodeID` from the link row and resolving usernames via `ResolveUserNodeID`. **Tests** (pure unit, fake syncer, no DB/forge): the fake records calls; assert `SetVisibility`/`ShareWithUser`/`Unshare` build the right calls (project node id from the link, resolved user id, `READER`/`NONE`); a non-GitHub repo returns `ErrProjectSyncNotGitHub`; a syncer whose `ResolveUserNodeID` returns `ErrGitHubUserNotFound` yields `ErrProjectSyncUserNotFound`.
- **M3 — Handlers + routes.** Add the four routes to the per-repo `RequireAuth` group; each handler runs the owner-or-admin preflight (`GetRepoForUser`, 404 on `pgx.ErrNoRows`), decodes its body, calls the M2 service method, and maps errors via `writeProjectSyncError` (extend for `ErrProjectSyncUserNotFound` → 422). **Tests — partition by tier, because the harness cannot mix them (this is the review's biggest correction):**
  - **Live-DB** (`github_project_sync_livedb_test.go`, `cliLiveDB(t)`, `projectSync == nil`) proves **authorization only**, exactly like #534: the owner and an admin reach the handler past the preflight (the nil-service **500**, not a preflight 404) for each new route; a non-owner member gets **404** (existence-hiding) for each; a foreign repo survives. It does **not** — and cannot — assert 409/422/behavior. Run via `./e2e/run-store-it.sh` (CI `test:api-store-it`); a no-DB `task gate:api` SKIPS it silently (the `PASS=0` trap in `.claude/rules/go.md`) — never read a skipped run as green.
  - **httptest with a stubbed syncer** (`admin_github_project_sync_test.go`) proves **behavior + preconditions**: happy paths (200/204), the instance flag off → 409 (via the admin path or a stubbed `ErrProjectSyncDisabled`, so no DB is needed), a non-GitHub repo → 422, a bad username → 422.
- **M4 — Web.** Add the four api.ts client methods + types; add the Board-access sub-block to the linked readout in `Repos.tsx` (fetch visibility on open, PUT on toggle, POST/DELETE for share/unshare with inline confirmations and the "GitHub doesn't expose the list" note); add the realistic mock methods. **Tests** (vitest, `Repos.test.tsx`): the Board-access block shows for a linked GitHub repo and is **absent** for a GitLab repo and for a not-linked repo (paired positive+negative, not vacuous); the visibility toggle reflects the fetched value and PUTs on change; share calls the right endpoint; a 409 (instance disabled) and a 422 (bad username, from the mock) surface inline; and the "GitHub does not expose the sharing list" **note renders** (positive assertion — do not assert the absence of a list that never exists).
- **M5 — Docs.** Extend `docs/github-project-sync.md` with the Board-access flow (make public, or share with a user as Reader) and state the write-only-sharing constraint plainly (GitHub exposes no readable collaborator list). Record the visibility/sharing addition in `specs/ai.md` (auto-applied). Confirm `web/scripts/check-docs.mjs` passes.

## Success criteria

1. **SC-1 — Visibility round-trips.** For a linked GitHub repo, the Board-access toggle shows the board's current public/private state and flipping it writes through `updateProjectV2`. Proven by an M2 service unit test (fake syncer records the mutation) + an M1 driver test (right query/vars) + a `Repos.test.tsx` case (toggle reflects fetched value, PUTs on change).
2. **SC-2 — Owner reaches the new routes; behavior works.** A non-admin member who owns a GitHub repo can drive visibility and share/unshare through the panel. The **authorization boundary** (owner reaches the handler, non-owner is 404'd) is proven by the **live-DB** tests; the **operation** (200/204, right service call) is proven by the **httptest stubbed-syncer** tests + the M2 service unit tests + the `Repos.test.tsx` cases. (The live-DB harness has `projectSync == nil`, so it proves reachability, not the operation — the two tiers are complementary, not interchangeable.)
3. **SC-3 — Non-owner refused, existence-hiding.** A member acting on a repo they do not own gets **404** (never 403/500) on every new route, matching #534. Proven by **live-DB** tests with a second user's repo id (skips silently under a no-DB gate — do not read a skip as pass).
4. **SC-4 — Admin retains reach; instance gate holds.** An admin reaches any repo's routes past the preflight (proven live-DB). With the instance flag off, every new route returns **409** — proven by an **httptest** case through the admin path / a stubbed `ErrProjectSyncDisabled` (a member's 409 would need the DB, so the 409 assertion lives at the httptest tier, not live-DB).
5. **SC-5 — Forge isolation + write-only honesty + gate isolation.** The Board-access block never renders for a non-GitHub connection (paired positive/negative vitest case); a bad username returns **422** (not 500) via `ErrProjectSyncUserNotFound`; the panel renders the "GitHub does not expose the sharing list" note (positive assertion). Mapped **preconditions** (409, and the 422 family) never fall to 500; an *arbitrary* forge rejection (e.g. GitHub refusing to remove the project owner) is accepted as a logged **500** and is out of scope to special-case (see Risks). `go.mod` diff clean; `task gate:api`, `task gate:web`, and `./e2e/run-store-it.sh` green.

## Risks and mitigations

- **No live-forge test in scope — the GraphQL facts are unverified by any gate.** Every Go test injects a fake syncer or an httptest server; no automated gate exercises the real `updateProjectV2` / `updateProjectV2Collaborators` / `node(...ProjectV2{public})` calls against GitHub. So a wrong field name, a wrong `ProjectV2Roles` value, or a wrong `public` semantics passes green and only breaks in production — the go.md "a green `sqlc generate` is not evidence the query runs" family, one layer up. Mitigation: the "GitHub API facts (resolved)" section is the backstop (re-introspect before M1 if unsure), and a **manual/live smoke against a throwaway project is recommended before ship** (explicitly out of automated scope).
- **No readable collaborator list (GitHub API limit).** Baked into the design (D2): sharing is write-only, the UI states so, and there is no side-table to drift. Removing a share is by username, not by picking from a list.
- **Bad-username vs transient vs permission errors.** Only GitHub's typed `NOT_FOUND` maps to 422 (`ErrProjectSyncUserNotFound`), via the M1 `graphqlDo` typed-error change; every other GraphQL error propagates as a redacted 500. So a transient failure is never mis-reported as "bad username", and the mapped preconditions never fall to 500.
- **Revoking the project owner (and other arbitrary forge rejections) → 500.** GitHub rejects removing the project owner as a collaborator; because `graphqlDo` gives no typed error for it, `writeProjectSyncError`'s default maps it to a logged 500. This is accepted, not special-cased: the revoke control is for readers the user granted, not the owner. SC-5's "never 500" is therefore scoped to the mapped *preconditions*, not to arbitrary forge rejections.
- **Duplicate grant.** `updateProjectV2Collaborators` with `READER` **sets** the role (upsert), so re-granting an existing Reader is expected to be a no-op success rather than an error. This idempotency is a stated GitHub fact, not gate-verified (fakes only) — see the no-live-forge caveat.
- **GraphQL rate/cost.** Each Board-access action is one small GraphQL query or mutation (a few points), issued only on explicit user action, inheriting the existing client's timeout and redactor. Negligible versus the sync poller; no new backoff needed.
- **Visibility read adds a forge call to the panel.** Kept off the DB-only status endpoint (unchanged); the visibility GET is a **separate**, lazy call issued only when the linked Board-access section is shown, so the common status open pays nothing (D4).
- **Broadening write surface.** The new routes reuse #534's exact owner-or-admin existence-hiding guard and the instance-flag gate; there is no weaker path, and no new scope (the `project` scope is already required and preflight-checked).
- **CLI drift.** Deliberately out of scope, same reason as #534: these are cookie+CSRF writes and `RequireUser` forces `IsAdmin=false` for CLI tokens. Recorded so the `api/cmd/uzi/` convention check is answered.

## Out of scope

- Any CLI verb (bearer vs cookie+CSRF; same as #534 D5).
- A readable collaborator list or a uzi-owned collaborator side-table (the tracked-list option was considered and declined — D2).
- Writer/Admin collaborator roles (v1 grants Reader only — D3).
- Team-based sharing (`teamId` collaborators); only user logins in v1.
- An automated live-forge test (see Risks — manual smoke recommended before ship).
- Any change to provision/adopt/disable, the poller, or reverse sync — all shipped and untouched.

## Open questions / Decision log

- **D1 — Placement: inside #534's linked panel (RESOLVED).** The Board-access controls live in the existing linked-state readout on the Boards page, not a new page — a per-repo, owner-scoped action beside the status it acts on, matching #534's D1.
- **D2 — Sharing is write-only (RESOLVED, API-forced).** `ProjectV2` exposes no collaborators connection, so uzi cannot render an authoritative list. v1 grants/revokes by username with inline confirmations and a note, rather than a tracked side-table (rejected: adds a migration and a source-of-truth-drift caveat for little gain). Revisit only if GitHub adds a readable collaborators field.
- **D3 — Reader role only (RESOLVED).** The share action grants Reader; the use case is "let me (or a teammate) see the board uzi owns." Writer/Admin invite manual edits that race the sync. The route shape leaves room for a `role` field later without breaking changes.
- **D4 — Visibility read is a separate lazy endpoint (RESOLVED).** The #534 status endpoint stays a pure DB read; visibility (a live forge call) gets its own GET, issued only when the Board-access section opens, so status stays cheap.
- **D5 — Authorization + instance gate reused, not reinvented (RESOLVED).** Every new route uses #534's owner-or-admin preflight and the `github_project_sync_enabled` instance gate; admins skip the preflight, members act only on owned repos after an admin enables the feature.
- **D6 — Bad-username detection needs a typed driver error (RESOLVED).** Plain reuse of `ResolveProjectV2OwnerID` cannot distinguish a non-existent login (`graphqlDo` collapses all GraphQL errors into one redacted string). M1 adds a typed `ErrGitHubUserNotFound` (capturing the GraphQL error `type`), mapped to 422; all other errors stay 500. Only GitHub's `NOT_FOUND` is 422.
- **D7 — CLI out of scope (RESOLVED).** Same auth mismatch as #534 D5.
