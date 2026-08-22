# PRD #576 — GitHub Project sync: Adopt-first UX + safe column auto-create

**Issue**: [#576](https://github.com/vtmocanu/uzi/issues/576)
**Status**: Implemented (2026-08-22) — all seven milestones (M1–M7) shipped on branch `agent/issue-576`.
**Priority**: Medium
**Owner**: TBD

> Builds on PRD #364 (GitHub Projects v2 Status sync), #534 (visibility + collaborator sharing). Read `api/internal/forgesvc/projectsync.go` and `prds/364-github-project-status-sync.md` first — this PRD extends that machinery, it does not replace it.

> **Scope guard for the uzi worker**: this PRD touches **no** `.github/workflows/**` file, in neither implementation nor validation. The worker PAT lacks `workflow` scope, so any workflow-file change in the branch diff is an atomic push rejection that loses the whole branch (`.claude/rules/prds.md`). Everything here is `api/`, `web/`, `docs/`, and tests — no CI/workflow edits.

---

## Problem

The GitHub Projects v2 sync feature works but its UX is confusing and one capability is missing. Five concrete defects, all observed live on `vtmocanu/uzi` (Project #2):

1. **Provision is offered where it cannot succeed.** GitHub requires a linked project's owner to equal the repo's owner. A user-owned repo (like `vtmocanu/uzi`) therefore cannot link a bot-owned project — Provision fails with *"Only projects owned by the same owner as the repository can be linked."* The UI offers Provision anyway, and the user meets a raw error.
2. **The "Sync" badge never reflects health.** `web/src/pages/Repos.tsx:870` renders `<Badge tone="neutral" dot>Sync</Badge>` unconditionally. The sibling status badge (`Repos.tsx:734`) uses `tone={r.enabled ? "ok" : "neutral"}` where `"ok"` is green. The sync pill is a static neutral pill that says nothing about whether sync is actually working.
3. **Adopt silently skips unmatched columns.** On Adopt, each uzi board column is exact-matched to an existing Status option by name; unmatched columns (observed: **Planned, bug, Human Review, Later**) are omitted and only logged server-side (the `adoptAndSeed` log at `projectsync.go:428`; the advisory string is built by `unmatchedNote`). The user is not shown which columns were skipped, so items in those columns never sync and the cause is invisible. Note the docstring near `projectsync.go:383` claims the note is "recorded on the link row" — that is **false** (it is only logged and returned), a stale doc M3 must correct in the same change.
4. **Adopt seeds synchronously → cosmetic 502.** `AdoptGithubProjectSync` (`api/internal/handler/admin_github_project_sync.go:54`) calls `h.projectSync.Adopt(...)` inline; seeding dozens of issues took ~27s live and nginx dropped the response before it reached the browser. The link persists (adopt succeeded server-side) but the UI shows a 502, so a working operation looks broken.
5. **No supported way to auto-create the missing columns.** Ongoing option reconcile was deliberately deferred in PRD #364 (Decision D5) because GitHub's `updateProjectV2Field` is a destructive full-list replace that can wipe every item's Status on a mis-echoed option id — and that corruption can cascade back into the real forge issues (see Risk R1). So today the only fix for skipped columns is a manual GitHub-UI edit.

## Solution overview

- Make **Adopt the recommended default** in the sync panel with a terse one-line explainer; keep Provision available (it is correct and safe for **org-owned** repos).
- Add a **health-aware sync badge** driven by a new per-repo sync-health field on the `/api/repos` list payload, following the existing caller-scoped precedent.
- **Surface skipped columns** in the sync panel and add a **Resync** affordance (Adopt is already idempotent).
- Make **Adopt seed asynchronously** so it returns immediately and health reflects progress/errors.
- Add a **reverse-sync safety guard** so a botched reconcile can never cascade to strip real issue labels.
- Implement **safe column auto-create** that cannot lose existing labels: a fresh uzi-owned field on Adopt (no destructive replace), with Provision remaining the safe path for org repos.

---

## Resolved external facts (baked in — the worker has no open-web access)

> The uzi worker runs with restricted egress (forge + `*.anthropic.com` + package caches; no open web). Every GitHub Projects v2 fact this PRD relies on is stated here as a resolved fact, sourced from PRD #364's own research (`prds/364-github-project-status-sync.md`) and this repo's code. The worker must **not** attempt to look any of these up online; it may re-verify by reading the cited code/PRD, both in-repo.

- **F-A — Owner-match on linking.** `linkProjectV2ToRepository` requires the project's owner to equal the repo's owner. `createProjectV2(ownerId)` accepts a **User or Organization** node id (`prds/364:43`, F8). A PAT can create projects only for itself (`viewer`) or for orgs it belongs to with the `project` scope — **never for another user**. Consequence: for a **user-owned** repo whose owner is not the bot, Provision cannot produce a linkable project (this is defect #1). For an **org-owned** repo where the bot is an org member with project rights, Provision works and is the safe path.
- **F-B — `updateProjectV2Field(singleSelectOptions:[...])` is a full-list *replace*, not an append** (`prds/364:83,144`). To add one option you must read the field's current options and echo **every kept option back with its existing `id`**, add new ones without an id, and drop removed ones. A mis-echoed/omitted id makes GitHub treat the option as new, and **every item assigned to the original option loses its Status** (cleared to "No Status"). Corroborated by GitHub community discussion #198803 (`prds/364:144`).
- **F-C — The identity-preserving option `id` input shipped 2026-04-23** (`prds/364:44`); before that any option edit regenerated ids and orphaned assignments. It is available now but must be used exactly (F-B).
- **F-D — An empty `singleSelectOptions: []` is *believed* to be a silent no-op but is UNCONFIRMED against a primary source** (`prds/364:46`). Do not rely on it; never send an empty options list on a populated field.
- **F-E — `createProjectV2Field(dataType: SINGLE_SELECT, singleSelectOptions:[...])` creates a NEW field with its full option list in one call** (`prds/364:44`, F9). A fresh field has no existing item values, so setting its options is the **safe half** of the option surface — nothing to clear. This is what Provision already does (`provisionAndSeed` builds `newOptions` from the columns at `projectsync.go:333-339` and calls `CreateProjectV2Field` at `projectsync.go:340`). **The complementary write, `updateProjectV2Field` (F-B), does NOT exist in this codebase** — the `ProjectBoardSyncer` sub-interface (`api/internal/forge/projectsync.go`) has only `CreateProjectV2Field`, never an update. Building the option-append path would mean a new interface method + its github implementation + the project-syncer test fake — which is exactly why this PRD does not build it (see D3, M6).
- **F-H — a freshly created field reads EMPTY for every item until re-seeded.** Directly from F-E: a new field has no item values, so `ReadProjectV2ItemStatuses` returns `""` for every item on it. If `github_project_links.status_field_id` is switched to the new field while item markers still hold the OLD field's option ids, the very next reverse-sync tick sees `live("") != marker(old id)` for every issue and fires the R1 cascade (F-F). So switching fields is only safe if item markers are reset to `""` (or the board re-seeded) **atomically** with the field switch, or reverse sync is paused for that repo across the switch. This is the reverse-*read* face of the same risk F-B is the write face of.
- **F-F — Reverse sync writes Status→label back to the forge.** `ReverseSync`/`reverseDiff` (`projectsync.go:891,1143`) reads each item's live Status and, on a change vs. the stored marker, writes the mapped column label via `AutoMove` (`service.go:210`), which is **forge-first** and **strips every other column label** to enforce single-column membership (`service.go:219,247`). An **empty** live option (`""` = "No Status") is treated as "move to Open" → `AutoMove(target="")` → strips **all** board-column labels from the real issue. The unknown-option skip guard (`projectsync.go:1174-1180`) only catches a **non-empty** option not in the map; an empty one flows straight through. This is the cascade in Risk R1.
- **F-G — `repositoryOwner(login){ __typename }` distinguishes `User` vs `Organization`** and is a normal forge GraphQL call (the forge host is on the worker egress allowlist, so this is *not* open-web). This is how M1 can determine owner type for the Provision feasibility nudge.

---

## Current-state code map (all in-repo, for the implementer)

| Concern | Location |
|---|---|
| Sync pill (unconditional neutral) | `web/src/pages/Repos.tsx:870` |
| Sibling health badge (green = `"ok"`) | `web/src/pages/Repos.tsx:734` |
| Sync panel state + Provision/Adopt forms | `web/src/pages/Repos.tsx:66-79,220-258` (default owner_kind `"user"`) |
| Repo DTO (frontend) | `web/src/lib/api.ts:352-400` (`Repo` interface) |
| Repos list payload enrichment (caller-scoped per-repo fields) | `api/internal/handler/forge.go:602-604,686-688` (`GuardrailBlocked`, `DockerAllowlisted`, `DockerBlocked` — the precedent to follow) |
| Adopt handler (synchronous seed) | `api/internal/handler/admin_github_project_sync.go:54` |
| Provision handler | `api/internal/handler/admin_github_project_sync.go:118` |
| Sync-status read (health source) | `admin_github_project_sync.go:211` `GetGithubProjectSyncStatus` → `last_synced_at`, `last_error`, `item_count`, `owned_by_uzi`, `project_number` |
| Sync service (adopt/provision/reverse) | `api/internal/forgesvc/projectsync.go` |
| `AutoMove` (label strip mechanic) | `api/internal/forgesvc/service.go:199-243` |
| Reverse diff + empty-option handling | `api/internal/forgesvc/projectsync.go:1143-1220` |
| Link schema (`last_error`, `last_synced_at`, `owned_by_uzi`) | `api/internal/store/migrations/00140_github_project_sync.sql` |
| CLI (second API consumer) | `api/cmd/uzi/`, `docs/cli.md` |
| Docs to extend | `docs/github-project-sync.md`, `docs/github-bot-setup.md` |
| Reverse-sync tests (fakes to reuse) | `api/internal/forgesvc/projectsync_reverse_test.go` |

---

## Milestones

### M1 — Adopt-first sync UX (nudge, don't remove)
Make Adopt the default, recommended path; keep Provision for the org case. **This is a frontend + backend + forge-driver milestone, not frontend-only** — the feasibility nudge needs owner *type*, which no current code exposes.

- **Frontend** (`Repos.tsx`): default the mode selection to **Adopt** and present it first; add a **very terse** one-line explainer, e.g. *"Adopt a Project you already created (recommended); Provision only if uzi should create one for you (org repos)."*
- **Forge driver**: add a `ProjectBoardSyncer` method to resolve the repo owner's GraphQL `__typename` (`repositoryOwner(login){ __typename }` → `User` | `Organization`, F-G). This lands on the **GitHub-only `ProjectBoardSyncer` sub-interface** (`api/internal/forge/projectsync.go`), NOT the 3-driver `Forge` interface, so its blast radius is the github driver + the project-syncer test fake only. Copy the existing GraphQL query patterns already in the github driver.
- **Backend**: surface owner-type to the frontend (it is not derivable from `path_with_namespace`) — either on the sync-status read or as a small field the panel fetches.
- **Provision feasibility (nudge, not a hard gate):** when the owner is a `User` other than the bot, mark Provision secondary/disabled with a short reason ("uzi can't own a Project under a personal account — Adopt instead"); when `Organization`, keep Provision fully available. On resolution failure, **fall back to showing both** (current behavior) rather than blocking.
- **Do not delete Provision.** It is the safe, zero-data-loss path for org repos.
- **Success criteria** (offline-verifiable): vitest in `web/` asserts Adopt is the default-selected mode, the explainer renders, and for a user-owned repo Provision is disabled **with its reason text rendered** while for an org-owned repo it is available (drive via mock/fixture, no network). A Go unit test on the new owner-type resolver against the project-syncer fake asserts `User`/`Organization` are parsed correctly.

### M2 — Health-aware sync badge
Turn the static pill into a real status signal.

- Add a per-repo sync-health field to the repos list payload, computed caller-scoped like `GuardrailBlocked`/`DockerBlocked` in `api/internal/handler/forge.go:602-604,686-688`. Suggested shape: a small struct per repo — e.g. `github_project_sync: { linked: bool, healthy: bool, last_error?: string, last_synced_at?: time }` — derived from `github_project_links` (`last_error`, `last_synced_at`). Not linked → omit/null.
- This needs a **new batch store query** (only `GetGithubProjectLinkByRepo :one` exists in `queries/github_project_sync.sql`; add a `:many` that returns links for a set of repo ids), then `sqlc generate`. Per `.claude/rules/go.md`, a green `sqlc generate` is not evidence the query runs — it needs a live-DB test.
- **Extract a pure mapper** `syncHealthForLink(link) → DTO` (mirroring the pure `guardrailBlockedForRepo` helper that `admin_blocked_repos_test.go:25` tests offline). The handler wires the batch query to this mapper.
- Extend the `Repo` interface in `web/src/lib/api.ts`; drive the badge in `Repos.tsx:870`: **`tone="ok"` (green)** when linked and no `last_error`; **`tone="warning"`/`"danger"`** when `last_error` is set; **neutral** when not linked. Keep the "Manage" button.
- **Success criteria**: (offline) a Go unit test asserts the pure `syncHealthForLink` mapper returns healthy / errored / unlinked correctly; vitest asserts the badge tone maps to each state via a mock repo payload. (CI-only, explicitly a `*LiveDB` test — self-skips without `UZI_TEST_DATABASE_URL`, not part of `task gate:api`) a live-DB test asserts the list handler + batch query populate the field end to end, following the `TestListReposDockerBlockedLiveDB` precedent. Do **not** claim the handler path is offline-unit-testable — `Handler.q` is a concrete `*store.Queries`, not an interface.

### M3 — Surface skipped columns + Resync
Make the invisible advisory visible and give the user a one-click fix loop.

- **Persist the unmatched-columns advisory.** It is currently only logged (`adoptAndSeed`, `projectsync.go:428`) and returned as a note; nothing stores it. Add a column to `github_project_links` (e.g. `unmatched_columns text[]` or `jsonb`) via a **goose migration** (numbered above the live head at merge, per repo convention) + `sqlc generate`. The no-migration alternative — recompute on read by diffing live options against board columns — is rejected because `ProjectSyncStatus` is documented as a pure store read with **no forge call** (`projectsync.go` `ProjectSyncStatus` doc), and adding a forge call there removes the "cheap, cannot fail on a wedged upstream" property. State this tradeoff in the Decision Log.
- **Fix the stale docstring** at `projectsync.go:383` that claims the note is "recorded on the link row" — after this milestone it actually is; make the doc true.
- Have `adoptAndSeed` write the unmatched set into the link row; return it from `ProjectSyncStatus`; render it in the panel ("These board columns have no matching Status option and won't sync: …; add them as Status options and Resync").
- Add a **Resync** action that re-runs Adopt (idempotent — re-adopt re-diffs every item). Wire it in `Repos.tsx` and the CLI (M7).
- **Success criteria** (both offline, and split to match the fake's shape — an adopt→status round-trip does not connect through `fakeProjectStore`): (a) a Go test asserts `adoptAndSeed` writes the unmatched set into the captured `UpsertGithubProjectLinkParams`; (b) a Go test asserts `ProjectSyncStatus` maps a link row's stored unmatched set into the DTO. vitest asserts the panel lists the columns and Resync calls the adopt endpoint.

### M4 — Asynchronous Adopt seeding (fix the 502)
Return the link immediately; seed in the background.

- Split `ProjectSyncService.Adopt` (`projectsync.go:155`) into: **persist-link** (returns) and a **separately-invocable seed step**. The handler (`admin_github_project_sync.go:54`) persists + returns **200** with the link (keep 200, not 202 — existing web/CLI callers and tests assert 200; health from M2 is the progress signal, R3). The seed step runs in the background.
- **Use a detached context**, not the request's `r.Context()` (cancelled when the response is written), or seeding dies mid-flight. Seeding stamps `last_synced_at`/`last_error` as it progresses (mirror `stampLinkErrorReverse`).
- **Suppress reverse sync for the repo while seeding is in progress** (a flag/state on the link), so a poller reverse tick (`ReverseSync` runs every tick) does not race a partially-seeded board — `reconcileItems` would backfill issues the seed hasn't written yet and could mis-move them. Recovery is real if the API restarts mid-seed: the poller reconciles state on its next tick, so an interrupted seed converges.
- The health badge (M2) reflects seeding-in-progress / done / errored.
- **Success criteria** (offline, assert the *seam* at the SERVICE level — a handler-level "returns promptly" assertion is trivially true because `fakeProjectSync.Adopt` already returns immediately, so it proves nothing): a Go test drives the split so the seed step is called synchronously by the test, and asserts (a) the link is captured in `fakeProjectStore.links` *before* the seed step runs, and (b) on a scripted seed failure `last_error` is stamped (via `fakeProjectStore.linkErrs`). No test asserts on wall-clock duration; the reverse-suppression flag is asserted set during seed and cleared after.

### M5 — Reverse-sync blast-radius cap (fixes a STANDING data-loss bug; also a prerequisite for M6)
Bound how much damage one reverse tick can do to real forge issues.

> **This is not merely an M6 prerequisite — it fixes a live bug in shipped PRD #364 code.** The empty-option cascade (F-F) is reachable today with no M6: a user deleting/renaming a Status option in the GitHub UI, or GitHub clearing values, produces the real→empty transition that `reverseDiff` cascades into mass label-stripping. M6 makes it *easier* to trigger; it does not create it.

**Why the obvious guards do NOT work (from adversarial review — do not build these as the primary defense):**
- A **"drag signal" check** (marker present + known prior option) does not discriminate: in the F-B mis-echo the stored marker IS present and IS a valid prior option, so every corrupted item looks exactly like a legitimate single drag. It honors the cascade.
- A **"clear only" cap** misses the other corruption shape: a full-list replace can leave items on a **different VALID option id**, which resolves to the WRONG column, and `AutoMove` strips the correct label and applies a wrong one — non-empty, present in the map, not a "clear", so a clear-counting guard never sees it.
- **"Option id no longer present in the current option set → skip"** is redundant for the non-empty case (`reverseDiff` at `projectsync.go:1174-1180` already skips an unmapped non-empty option) and does not fire on the empty case (`""` is not an id); evaluating "present in the field" would also need a new live field-options read the reverse path does not do.

**The load-bearing guard: a total per-tick destructive-write cap, pre-counted (two-pass).**
- Restructure `reverseDiff` (`projectsync.go:1147`) from single-pass-execute into **count-then-decide-then-execute**: first compute every intended destructive `AutoMove` for the tick — counting BOTH clears (`target=""`, strips all column labels) AND remaps that would strip an existing column label — WITHOUT calling `AutoMove`; then, if that count exceeds the threshold, **abort the whole tick**, execute none, stamp `last_error`, and log loudly; otherwise execute. A naive running counter is insufficient — it strips the first N issues before tripping (partial corruption).
- **Threshold** must be relative, not a bare constant (a small board's full corruption fits under any fixed N): trip on `count > k` **AND** `count > x%` of tracked items (choose conservative defaults, e.g. k≈3, x≈25%, and state them). A single genuine drag (count 1) always passes; a mass event never does.
- Preserve the genuine single-item user-drag-to-Open behavior (already `TestReverseSyncOpenClears`, `projectsync_reverse_test.go:201`).
- **Success criteria**: extend `projectsync_reverse_test.go` (the `reverseSyncer`/`fakeMover`/`fakeProjectStore` fakes support this — confirmed): (a) a fixture where several items clear-to-empty at once makes **zero** `AutoMove` calls (tick aborted) and stamps `last_error`; (b) a fixture where several items remap to a wrong-but-valid option at once likewise makes **zero** `AutoMove` calls; (c) a single deliberate clear still makes exactly one `AutoMove(target="")`. Each **zero-call assertion is a negative assertion** — prove it non-vacuous with a mutation at the call site (raise the threshold / disable the cap and confirm the test reddens with the expected non-zero call count), per `.claude/agent-team.md`'s mutation sections and "a control that produces no output is not a control."

### M6 — Safe column auto-create (fresh field only)
Add missing columns without ever risking existing labels.

- **Fresh field only. Do NOT build the existing-field option-append path.** That path needs `updateProjectV2Field`, which does not exist in this codebase (F-E) — building it means a new `ProjectBoardSyncer` method + github impl + fake, and it is the destructive F-B surface. This PRD deliberately excludes it (D3). No `updateProjectV2Field` should appear in the diff.
- **Org repos**: "auto-create columns" is just the existing Provision path (uzi's own fresh field, F-E) — already built, no destructive replace.
- **Adopt (user repos)**: create a **fresh uzi-owned single-select field** on the adopted board via `CreateProjectV2Field` (F-E) with all uzi columns as options, and point `github_project_links.status_field_id` at it. The adopt code already falls back to `uziStatusFieldName`, so a uzi-owned field is a supported shape. Document the tradeoff (the board then carries two status-like fields) in the panel copy.
- **🔴 Field switch must be atomic with a marker reset (F-H, R1).** A fresh field reads empty for every item; if `status_field_id` is switched while item markers still hold the old field's ids, the next reverse tick fires the mass-empty cascade. So on any field switch: **reset all item markers to `""` (and/or re-seed) atomically with the switch, or pause reverse sync for the repo across it** (reuse M4's reverse-suppression flag). This is a hard requirement, not an optimization — without it the "safe" path triggers R1 on the reverse-read side.
- **Success criteria** (offline, via the project-syncer fake — no live GitHub): (a) fresh-field creation includes every uzi column as an option (assert on `fakeProjectSyncer` `createFieldCalls`); (b) after a field switch, item markers are reset (or reverse is suppressed) such that a reverse tick immediately after fires **zero** `AutoMove` calls — this reuses the M5 harness and is a negative assertion, so prove it non-vacuous with a mutation (omit the marker reset and confirm it reddens with mass `AutoMove(target="")` calls). Note explicitly that a "simulated mis-echo" test here can only exercise the reverse-read guard (M5's input), not a real field-update round trip, since the write path is fresh-create only.

### M7 — CLI surface, docs, and test sweep
Close the loop across the second API consumer and the user docs.

- **CLI** (`api/cmd/uzi/`, `docs/cli.md`): note there is currently **no project-sync CLI surface at all** — `repo.go` exposes only `list`/`remove`; PRD #364/#534 shipped web-only. So this is building a small new command group, not "parity" with an existing one. **Scope for v1: a read + fix loop only** — `uzi project-sync status <repo>` (linked?, health, skipped columns) and `uzi project-sync resync <repo>`. **Adopt and Provision stay web-only for v1** (they need interactive owner-kind/project-number input and the M1 nudge); state this as a deliberate scope decision (D4) rather than silently omitting them. If this group proves too large for the PR, it may split to a follow-up issue — say so, do not half-build it.
- **Docs**: extend `docs/github-project-sync.md` and `docs/github-bot-setup.md` with the full Adopt flow — create a Project under your own account, invite the uzi bot as a **Write** collaborator, name Status options to match uzi's board columns, then Adopt; when to use Provision (org repos); how Resync and auto-create behave; the two-status-field tradeoff. Keep `audience`/`order`/`title` frontmatter valid (`web/scripts/check-docs.mjs` runs in `npm run build`).
- **Success criteria**: `task gate:api`, `task gate:web`, and (if touched) `task gate:controller` green; `cd web && npm run build` passes check-docs; new tests from M1-M6 present and passing (non-vacuous per the mutation discipline).

---

## Risks & mitigations

- **R1 — Data-loss cascade into real forge issues (the headline risk), reachable in SHIPPED code today.** A real→empty Status transition (from an F-B mis-echo, or a user/GitHub editing the Status field) makes `reverseDiff` treat empty as "move to Open" → `AutoMove(target="")` strips every board-column label off the **real forge issues** (F-F), de-triaging them and disrupting the run pipeline (the `PRD` label drives runs). **Mitigations**: M5's pre-counted total-per-tick destructive-write cap (the drag-signal and clear-only guards do NOT work — see M5); prefer fresh-field creation (F-E) over mutating the user's field; M6's atomic marker-reset on field switch (F-H). M5 is a hard prerequisite for M6 **and** ships value on its own as a standing-bug fix.
- **R1b — Mis-echo to a DIFFERENT VALID option (not a clear).** A full-list replace can reassign ids so items sit on a valid-but-wrong option → wrong column → `AutoMove` strips the correct label and applies a wrong one. Clear-counting guards miss this entirely. **Mitigation**: M5's cap counts destructive remaps too, not just clears.
- **R2 — Owner-type detection is imperfect for feasibility.** Org membership + `project` scope is not implied by owner type alone. **Mitigation**: M1 treats owner-type as a *nudge*, not a hard gate — on failure it shows both paths and lets the existing clear error surface; nothing is silently blocked.
- **R3 — Async seeding response contract + concurrency.** Callers (web + CLI) expect a synchronous "linked" result, and background seeding races the reverse poller. **Mitigations**: keep the endpoint returning **200** with the link (not 202) so existing status-code assertions hold; make health (M2) the progress signal; suppress reverse sync for the repo while seeding (M4); use a detached context.
- **R4 — Unconfirmed empty-list no-op (F-D).** **Mitigation**: never send an empty options list on a populated field; the design never needs to.
- **R5 — Hidden scope creep in M1/M2/M3/M7.** Review found each larger than its heading: M1 needs a forge-driver method, M2 a batch query + LiveDB test, M3 a migration, M7 a brand-new CLI group. **Mitigation**: each is now stated in-milestone with its true surface; if the PR gets too large, split M5+M6 (the data-safety half) from M1-M4 (the UX/observability half) — see the note below.

> **Split option (if one PR is too large):** M1-M4 (Adopt-first UX + observability, no data-loss risk) is cleanly separable from M5-M6 (reverse-sync safety + safe auto-create — the only milestones that can corrupt real issues). Kept as one PRD per request; the boundary is clean if it needs to become two.

## Non-goals

- The **existing-field option-append path** (`updateProjectV2Field`, F-B) — deliberately not built (D3); auto-create is fresh-field only.
- Ongoing full bidirectional column *rename/remove* reconcile beyond safe *add* (rename/remove stays a documented manual step, consistent with PRD #364 D5).
- CLI Adopt/Provision (web-only for v1, D4).
- Deleting Provision (explicitly rejected — it is the safe path for org repos).
- Any `.github/workflows/**` change.
- Non-GitHub forges (Projects v2 sync is GitHub-only by design).

## Dependencies

- PRD #364 (core Projects v2 sync), PRD #534 (visibility/sharing). No new external services; no new trust boundary.
- **New goose migrations** (M2 batch query needs none; M3 adds an `unmatched_columns` column on `github_project_links`; M4's reverse-suppression flag may need a column too). Number each above the live head in `api/internal/store/migrations/` at merge, per repo convention; run `sqlc generate` after any query/schema change and confirm the regenerate is a no-op in CI.

## As-built notes (2026-08-22)

All seven milestones shipped; the design was followed as written, with these concrete implementation choices worth recording:

- **M1** — owner type resolves via a new **GitHub-only** `ProjectBoardSyncer.ResolveRepositoryOwnerType` (`repositoryOwner(login){__typename}`), surfaced by a new `GET /repos/{id}/github-project-sync/owner-type` endpoint (kept OFF `ProjectSyncStatus` to preserve its pure-store-read property, D5). The Provision nudge disables for a `User` owner (with visible reason), stays available for an `Organization`, and shows both on resolution failure.
- **M2** — new batch query `ListGithubProjectLinksByRepoIDs` + pure `syncHealthForLink` mapper; the DTO field is `github_project_sync: {linked, healthy, last_error?, last_synced_at?}`, computed caller-scoped in both repos-list loops.
- **M3** — `unmatched_columns text[]` added by **migration 00148** (COALESCE-guarded upsert to dodge the nil-slice→NULL trap). **Resync** ships as a dedicated `POST /repos/{id}/github-project-sync/resync` that re-seeds against the stored link (no re-input of owner_kind/project_number), reusing the M4 seed seam. Both stale "recorded on the link row" docstrings corrected.
- **M4** — the seed step runs in a background goroutine on a detached context (`context.WithoutCancel` + 8m timeout); the finalize (lease-clear + last_error stamp/clear) always runs on a fresh 30s context. Reverse suppression is a **timestamp lease** (`seeding_started_at`, **migration 00149**), not a boolean: `ReverseSync` skips a repo only while the lease is younger than 10m, so a crash-orphaned lease ages out and the poller reconciles. The async seed applies to Adopt, Resync AND Provision; response codes unchanged (200/200/201).
- **M5** — `reverseDiff` restructured plan→decide→execute; the destructive-write cap thresholds (k=3, pct=25) are injectable service fields so the negative tests carry built-in mutation controls.
- **M6** — auto-create ships as `POST /repos/{id}/github-project-sync/autocreate-columns` + `ResetGithubProjectItemMarkers` query (no migration). The field switch holds the M4 lease AND resets markers to NULL (order: lease → reset → switch → async re-seed); `owned_by_uzi` is preserved (uzi owns the field, not the project). No `updateProjectV2Field` in the diff (D3).
- **M7** — `GET .../github-project-sync` (status) and `POST .../github-project-sync/resync` moved from the cookie-only `RequireAuth` group to `RequireUser` so the CLI `uzc_` Bearer token is accepted; adopt/provision/disable/visibility/collaborators/owner-type/autocreate stay web-only on `RequireAuth`. CLI group `uzi project-sync status|resync <repo>` (read+fix loop only, D4); a `*LiveDB` test asserts the Bearer path returns an owner-scoped 404 (not 401).

## Decision log

- **D1 — Nudge toward Adopt, do not strip Provision.** Provision is genuinely infeasible for user-owned repos (F-A) but is the *safe* path for org repos (F-E). Removing it would delete the zero-risk path for org users and force everyone onto the risky F9 reconcile. Resolution: default-select Adopt + terse explainer + soft feasibility nudge; keep Provision. *(Per-user direction, this session.)*
- **D2 — M5 is both a standing-bug fix and a hard prerequisite for M6.** The R1 cascade exists in shipped code independent of M6; the reverse-sync cap fixes it now and is required before any auto-create lands. The guard is a **pre-counted total-per-tick destructive-write cap** — the drag-signal and clear-only guards were evaluated and rejected as ineffective against the actual corruption signatures (see M5).
- **D3 — Fresh uzi-owned field only; the existing-field option-append path is out of scope.** F-E (create) is the safe half; F-B (replace, via a nonexistent `updateProjectV2Field`) is the destructive half and would expand the Forge surface. Accept the two-status-field tradeoff. Any field switch must reset item markers atomically (F-H).
- **D4 — CLI is a new, read+resync-only group for v1.** No project-sync CLI exists today; Adopt/Provision stay web-only (interactive input + the M1 nudge). Only `status` and `resync` ship in the CLI now.
- **D5 — Persist the unmatched-columns advisory in a new link column, not recompute-on-read.** Recompute would add a forge call to `ProjectSyncStatus`, breaking its documented "pure store read, cannot fail on a wedged upstream" property. Cost: one goose migration.
