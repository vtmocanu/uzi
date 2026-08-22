# PRD #584 — Reflect closed issues as a Done status on the GitHub Projects board

**Issue**: [#584](https://github.com/vtmocanu/uzi/issues/584)
**Status**: Draft
**Priority**: Medium
**Owner**: TBD

> Extends PRD #364 (core Projects v2 sync) and #576 (adopt-first + auto-create). Read `api/internal/forgesvc/projectsync.go` and `api/internal/board/board.go` first. Revises PRD #364 Decision D1 (closed issues left untracked) — see the Decision Log.

> **Scope guard for the uzi worker**: touches **no** `.github/workflows/**` file (worker PAT lacks `workflow` scope, `.claude/rules/prds.md`). All changes are `api/`, `docs/`, and tests, plus one goose migration.

---

## Problem

uzi's Projects sync deliberately **stops tracking a closed issue** and leaves its GitHub card at whatever Status it last had (PRD #364 D1). Concretely, forward sync skips closed issues:

- `seedItems` (`api/internal/forgesvc/projectsync.go:874`): `if issue.State == "closed" { … continue }` — comment: *"D1: closed issues carry no Status option — they are skipped entirely."*
- `board.ResolveColumn(labels, state, position)` (`api/internal/board/board.go:23`) returns a `closed bool`; every forward site treats closed as "skip".
- `reverseDiff` likewise skips closed issues (`projectsync.go:1648`).

So a closed issue never moves to a Done/Closed column on the linked board. Users expect a closed issue to land in a **Done** column. (Observed on `vtmocanu/uzi`: closed issues simply do not appear in any status column on Project #2.)

**The periodic hook, precisely (this is where the real change lives).** The poller runs **only `ReverseSync`** per tick (`api/internal/poller/poller.go:425`); there is **no periodic forward sync**. `seedItems` is one-shot, called only from Adopt/Resync/Provision/AutoCreateColumns via `launchSeed`. `ForwardMove` (`projectsync.go:1119`) is driven by a resolved `targetColumn` from label-move call sites (a drag, `handler/board.go:837`; the run lifecycle, `runlifecycle/lifecycle.go:377`) and never fires on a state change — and run-lifecycle explicitly refuses to move a closed issue (`lifecycle.go:313-316`). **The one periodic reaction to an issue closing is `reconcileItems` Pass 1 close-prune (`projectsync.go:1469-1484`), which DELETES the item row and makes no GitHub call by design.** That delete is exactly why reopen "just works" today (a reopened issue is untracked again and Pass 2 backfills it). Any close→Done design must change that delete and then add explicit reopen restoration — the two are coupled.

## Solution overview

On issue **close**, forward-sync sets the linked item's Status to a dedicated **`Done`** option; on **reopen**, the normal column/label path restores its Status. The Done option is obtained **only via the safe create path**: uzi includes a `Done` option when it *creates* its own field (Provision / auto-create), so uzi-owned boards get it by construction. For a field that already exists **without** a `Done` option (an adopted built-in `Status`, or a `uzi Status` created before this feature), uzi **surfaces an advisory** and the user adds the option manually or re-provisions — uzi never adds an option to an existing field, which is the destructive full-list-replace path PRD #576 D3 deliberately left unbuilt. Reverse direction (a Done card **reopening/closing** the issue) is out of scope: uzi never changes issue open/closed state from the board.

## Resolved facts (baked in — the worker has no open-web access)

> Worker egress is forge + `*.anthropic.com` + package caches; no open web. All facts below are from this repo's code and prior PRDs; re-verify by reading the citations (all in-repo).

- **F-1 — The safe option surface is CREATE-ONLY.** `CreateProjectV2Field(projectID, name, options)` creates a field with a full option list (`api/internal/forge/projectsync.go`); there is **no** add-option-to-existing-field method (`updateProjectV2Field` does not exist in this codebase — PRD #576 D3/F-B). So a `Done` option can be *created as part of a new field* but **cannot be safely appended** to an existing field. This is why "auto-create Done if missing" is achievable for uzi-created fields and is a manual/advisory step for pre-existing ones.
- **F-2 — Forward sync sets item Status via `SetProjectV2ItemStatus`.** The target option is computed per item from its column (`seedItems` `:907-917`; the single-item forward path `:1239`; the reconcile path `:1531`). An empty target clears to "No Status" (D2). A closed issue currently yields "skip".
- **F-3 — The link row stores the column→option map (`status_options`) and field id (`status_field_id`).** Adding a `done_option_id` needs one new nullable column (a goose migration); the existing `00148`/`00149` (from #576) are on `main`, so this PRD drafts **`00150`**, renumbered above the live head at merge (repo convention).
- **F-4 — Reopen "just works" TODAY only because close DELETES the item row.** A reopened issue is untracked again, so Pass 2 backfill (`:1490-1559`, which handles untracked *open* issues) re-adds it and sets its column Status. **The moment close keeps the row (required to hold a Done status), this stops being automatic** — Pass 2 skips *tracked* items (`:1493`), `reverseDiff` no-ops when `live == marker == done_option_id` (`:1619`), and nothing else reacts to the state change. So keeping a Done row REQUIRES adding explicit reopen restoration (see M2). This corrects an earlier assumption that reopen needed no code.
- **F-6 — An adopted BUILT-IN `Status` field already has a `Done` option.** GitHub's default Status ships `Todo` / `In Progress` / `Done` / `No Status` (`docs/github-project-sync.md`). So the adopt/resync path populates `done_option_id` automatically for a built-in Status board — the feature works there with no user action. The advisory (M4) is for a **`uzi Status`** field (built from board columns only, which are `Planned/In Progress/bug/Human Review/Later` — no `Done`) or a custom field lacking a `Done` option. `vtmocanu/uzi`'s board uses `uzi Status`, so it is exactly the advisory case.
- **F-5 — Field-creation option sets are built from board columns today.** `provisionAndSeed` (`:402-409`) and `AutoCreateColumns` (`:776`) build `newOptions` from the columns and call `CreateProjectV2Field`; appending one `Done` option there and capturing its id is the create-path mechanism for F-1.

## Milestones

### M1 — Resolve and store the `Done` option
- Add a nullable `done_option_id text` column to `github_project_links` (goose migration draft `00150`, renumber at merge) + `sqlc generate`. It is written by `UpsertGithubProjectLink` (`:603-611`) and read by `GetGithubProjectLinkByRepo`.
- **Create path** (`provisionAndSeed` `:402`, `AutoCreateColumns` `:776`): append a `Done` `ProjectV2NewOption` (distinct palette color) to `newOptions` **unless a board column is already named `Done`** (name collision — `optionByName` is name-keyed at `:416-419`/`:794-797`, so a duplicate name makes the captured id ambiguous; if a `Done` column exists, reuse its option id and do not append). After `CreateProjectV2Field`, capture the `Done` option's id into `done_option_id`.
- **Adopt/Resync path** (the option mapping in `prepareSeedLink` `:568-582`): if the resolved field has an option named `Done` (exact match; reserved name for v1), set `done_option_id`; otherwise leave it empty. `Done` is a reserved projection option, not a board column, so it is **not** added to `status_options` and **not** reported as an unmatched column. Per F-6 this populates automatically for an adopted built-in Status.
- **Budget the assertions this breaks.** Appending `Done` makes created-option-count ≠ board-column-count, reddening `projectsync_test.go:762-763` (`!= 2`), `:847-848` (`!= 10`), `:991-1002` (exact column-name list), and `TestProvisionColorsCycle`. Update these to expect the extra `Done` option (they are the existing create-path tests, not a behavior change to revert).
- **Success criteria**: (offline) Go unit tests assert (a) the create paths include a `Done` option and persist its id, and skip appending when a `Done` column already exists; (b) adopt/resync populates `done_option_id` when the field has a `Done` option, empty otherwise. (CI-only `*LiveDB`) extend `api/internal/store/github_project_sync_livedb_test.go` to round-trip `done_option_id` through `UpsertGithubProjectLink`/`GetGithubProjectLinkByRepo` — per `.claude/rules/go.md`, a green `sqlc generate` is not evidence the query runs.

### M2 — Close → Done at the periodic hook, with reopen restoration (the core change)
- **The forward hook is `reconcileItems` Pass 1 close-prune (`projectsync.go:1469-1484`), NOT `seedItems`/`ForwardMove`/Pass 2.** Today Pass 1 deletes a closed issue's item row with no GitHub call. Change it: when the issue is closed **and** `done_option_id != ""`, call `SetProjectV2ItemStatus(..., done_option_id)`, advance the marker to `done_option_id`, and **keep the row**. When `done_option_id == ""`, keep the current delete-prune (no Done option to use). Do **not** touch `ForwardMove` (it has no issue state and a closed issue resolves to `column=""` → it would clear, not Done) or `lifecycle.go:313-316` (run-lifecycle must not move closed).
- **Reopen restoration (required by F-4, coupled to keeping the row).** Add explicit handling for a tracked item whose issue is now `opened` and whose stored marker is `done_option_id`: set its Status to the issue's current column option (or clear to No Status if the issue has no column label), advance the marker, and keep tracking. Choose one shape and state it: either (a) handle it in Pass 1 alongside the close case, or (b) on reopen delete the Done row so the same tick's Pass 2 backfill re-adds it — both are acceptable; (a) avoids a double write. Without this, a reopened issue stays stuck on Done.
- **Also update `seedItems`' closed branch** (both guards: `:874` and the `board.ResolveColumn`-driven `:884`) so a manual Resync/Adopt/Provision re-seed projects closed issues to Done too, consistent with the periodic path. This is the one site that already has `issue.State` in hand.
- **Success criteria** (offline, one test PER path — a single shared test would pass via `seedItems` while the periodic path stays broken): (1) a `reconcileItems` fixture with a newly-closed tracked issue and `done_option_id` set asserts Pass 1 calls `SetProjectV2ItemStatus(done_option_id)`, keeps the row, and advances the marker — and with `done_option_id` empty still deletes (prove BOTH non-vacuous with call-site mutations: revert to unconditional delete → the set-Done case reddens; force done set → the empty case reddens); (2) a reopen fixture starts with a tracked item at `marker == done_option_id` and issue `opened`, and asserts its Status moves to the column and the marker advances off Done; (3) a `seedItems` re-seed test asserts a closed issue is set to Done. The `fakeProjectStore` already records `SetProjectV2ItemStatus` (`setCalls`) and item rows; add a per-issue state read only if the chosen reopen shape needs one.

### M2 — Forward sync: closed issue → Done
- Replace the "skip closed" branches on the forward path with "map closed → `done_option_id`": at `seedItems` (`:874`), the single-item forward path (`:1239` region), and the reconcile path (`:1490-1531`), when `issue.State == "closed"` **and** `done_option_id != ""`, set the target option to `done_option_id` (and advance the marker to it) instead of `continue`. When `done_option_id == ""`, keep the current skip (no Done option to use). Centralize in a shared `targetOptionForIssue(issue, columnOption, doneOptionID)` helper so all forward sites agree.
- **Reopen** needs no new code (F-4) — verify a reopened issue's marker/Status returns to its column via an explicit test.
- **Success criteria** (offline): a fixture with a closed issue and a set `done_option_id` asserts forward sync calls `SetProjectV2ItemStatus(..., done_option_id)` and stores that marker; with `done_option_id` empty it still skips. A reopen test asserts the Status returns to the issue's column. Prove the close→Done assertion non-vacuous with a call-site mutation (revert to `continue` → test reddens), per `.claude/agent-team.md`.

### M3 — Reverse sync leaves the Done projection alone (mostly a guard + invariant test)
- Because M1 keeps `Done` **out of `status_options`**, `reverseDiff` already skips a live Done item via the unknown-option skip (the option-map miss at `:1630-1638`), so it makes no `AutoMove` and is never destructive-counted — R3 largely holds **by construction**, not by new code. M3's job is to (a) make that a deliberate, tested invariant (Done is never in `status_options`; a live Done item yields zero `AutoMove` and zero destructive count), and (b) keep the existing closed-issue skip (`:1648`).
- **Known edge, document it (narrow):** if a user manually **clears** a Done card's Status, it reads `live == ""` (not `done_option_id`), so the skip above misses it; if that card's issue is meanwhile reopened and carries a column label, `reverseDiff` would classify the clear as destructive (`:1664-1665`) and could strip the label / count toward #576's cap. This requires manual-clear + reopen together; v1 documents it as a limitation rather than special-casing an empty-live-on-a-Done-item.
- **Success criteria** (offline): assert the invariant directly — a fixture with a live item on `done_option_id` makes **zero** `AutoMove` and is absent from the destructive count. Since Done is never in the map by construction, prove non-vacuity by asserting the construction invariant (Done id ∉ `status_options`) rather than a synthetic map entry. Fix the citation: the option-map skip is `:1630-1638`; `:1648` is the closed-issue skip.

### M4 — Advisory when the synced field has no Done option
- When a link has an empty `done_option_id` **and** is linked (the genuine case is a **`uzi Status`** field, whose options are the board columns only — no `Done` — or a custom field lacking `Done`; **not** an adopted built-in `Status`, which ships a `Done` option per F-6), surface an advisory in the sync panel + status read, mirroring the unmatched-columns advisory: *"Closed issues won't show a Done status. Add a `Done` option to the synced field and Resync, or re-provision."* uzi does **not** add the option to an existing field itself (F-1 / #576 D3).
- **Success criteria** (offline): Go test asserts the status DTO carries the "no Done option" flag when `done_option_id` is empty on a linked field; vitest asserts the panel renders the advisory (via the web mock, mirroring the unmatched-columns advisory already tested in `Repos.test.tsx`).

### M5 — Docs, tests, gate
- `docs/github-project-sync.md`: document closed→Done (uzi-created fields get `Done` automatically; existing fields need a `Done` option added manually or via re-provision), the reopen behavior, and that reverse never reopens/closes an issue. Update the "What the model does and doesn't cover" section, which currently states closed issues stop being tracked (that decision is being revised — D1 below).
- `web/scripts/check-docs.mjs` passes. `task gate:api` and `task gate:web` green. Verify `api/cmd/uzi/` needs no change (the CLI `project-sync status` may optionally surface the new advisory — decide and note).
- **Heads-up**: the Go lint ratchet is `whole-files: true` (`.golangci.yml`); editing `projectsync.go` gates pre-existing findings in that whole file too — read `.claude/rules/go.md` before treating one as a regression.

## Risks & mitigations

- **R1 — "Auto-create Done if missing" is only safe on the create path.** Appending to an existing field is the destructive F-B path. **Mitigation**: create-path adds `Done`; existing fields get an advisory (M4), never a silent destructive append. The Decision Log records this reconciliation of the requested behavior.
- **R2 — Reopen restoration is the hard part, coupled to keeping the row.** Keeping a Done row (needed to show Done) breaks today's delete-on-close reopen path (F-4). **Mitigation**: M2 adds explicit reopen restoration and tests it with a stuck-marker fixture; this is the milestone's real risk, not the close write.
- **R3 — Interaction with #576's reverse destructive-write cap.** A closed→Done forward write must not read back as a destructive reverse move. **Mitigation**: Done stays out of `status_options`, so reverse already skips it (M3) — holds by construction. The narrow manual-clear-then-reopen edge is documented in M3, not fixed in v1.
- **R4 — `uzi Status` and custom fields lack a `Done` option (adopted built-in Status does NOT — F-6).** `vtmocanu/uzi`'s `uzi Status` is the advisory case. **Mitigation**: M4 advisory + docs recovery (add `Done` manually or re-provision); no back-fill.
- **R5 — Order after #582.** M1 populates `done_option_id` during Resync, and #582 fixes Resync re-pointing the field; landing after #582 keeps a Resync from also re-pointing. **Mitigation**: note the ordering; still correct if #582 is unmerged, but a pre-#582 Resync could re-point the field (that separate bug, not this one).
- **R6 — Create-path `Done` name collision.** A board column literally named `Done` would duplicate the appended option. **Mitigation**: M1 reuses an existing `Done` column's option instead of appending.

## Non-goals

- Adding an option to an **existing** field (destructive path, #576 D3) — closed→Done needs `Done` created with the field or added by the user.
- Reverse control of issue open/closed state from the board.
- A configurable Done-option name (fixed `Done` for v1).
- Column rename/remove propagation (still manual, #364 D5).
- Any `.github/workflows/**` change.

## Dependencies

- PRD #364 (core sync), #576 (auto-create / uzi-owned field), and **#582** (Resync field-id fix — see R5). One new goose migration (`00150`, renumber at merge). No new external services or trust boundaries.

## Decision log

- **D1 — Revise PRD #364 D1 (closed = untracked) to closed = a `Done` Status, which changes the reconcile close-prune from delete-row to set-Done-and-keep-row.** A closed issue projects to a dedicated `Done` option instead of being left at its last status. Because keeping the row breaks today's delete-on-close reopen path, M2 also adds explicit reopen restoration. This is a deliberate behavioral change to `reconcileItems` Pass 1, not just a new branch; M5 updates the docs.
- **D2 — `Done` is obtained via the create path only; existing fields get an advisory.** Honors "auto-create if missing" where the safe API allows (uzi-created fields) and is explicit that an existing field needs the option added manually — never a destructive append (F-1, #576 D3).
- **D3 — Reverse ignores `Done`; no board→issue-state control.** The Done projection is forward-only; a Done card never reopens or closes the issue.
- **D4 — Reserved option name `Done`, fixed for v1.** A configurable name is a later enhancement.
