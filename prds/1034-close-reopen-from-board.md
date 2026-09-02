# PRD #1034 — Close/reopen an issue from the board (all forges)

**Issue**: [#1034](https://github.com/vtmocanu/uzi/issues/1034)
**Status**: Ready for implementation (see Sequencing — must land after #1025)
**Priority**: Medium
**Route**: uzi (Auto)

## Problem

The board's Closed column is inert. A card can never be dragged into it or out of it, so
closing or reopening an issue forces the user to leave uzi and do it on the forge. This was
a deliberate MVP cut, not an oversight — but it is now a recurring friction point.

Verified current behaviour (read at `main` this session):

- The Closed lane is declared non-droppable: `web/src/pages/Board.tsx:721` —
  `cols.push({ key: CLOSED_KEY, label: "Closed", droppable: false, ... })`.
- Frontend guards early-return on the Closed key in both `move()` (`Board.tsx:662-663`) and
  `applyDrop()` (`Board.tsx:904-906`); the lane's `onDragOver` never calls `preventDefault()`
  because it gates on `col.droppable` (`Board.tsx:1306-1310`), so the browser refuses the drop.
- A closed card cannot be dragged at all: `dropIntent` returns `null` for a dragged closed
  card — `web/src/lib/boardOrder.ts:284` (`if (!dragged || dragged.closed) return null;`).
- The backend rejects it outright: `api/internal/handler/board.go:851-854` returns HTTP 400
  `"moving to the Closed column is not supported; close the issue on the forge"`.

## Solution (one line)

Add a neutral `SetIssueState` primitive to the forge layer (one interface method, three driver
impls), a forge-first close/reopen path in `forgesvc`, lift the board handler's Closed guard,
and make the board's Closed column a real drag target: **drag an open card into Closed = close;
drag a closed card out onto any open lane = reopen (and place it in that lane).**

## Scope decisions (settled)

- **UX trigger = drag only** (no card-menu action this PRD). Drag an open card into the Closed
  lane closes it; drag a closed card onto Backlog or any configured column reopens it and moves
  it to that lane.
- **Reopen target = the lane it is dropped on.** Reopen onto a configured column applies that
  column's label; reopen onto Backlog clears all column labels (same as a normal move to Open).
  Reopen is therefore `SetIssueState(opened)` **then** the existing label-move, in that order.
- **Close leaves labels untouched.** A closed card renders in the Closed lane regardless of its
  column labels (`board.ResolveColumn` returns `closed=true` whenever `state == "closed"`,
  `api/internal/board/board.go:23-42`), so no label change is needed on close. Reopening later
  restores whatever column the user drops it on.
- **All three forges**, behaviour-identical. The neutral state vocabulary already exists:
  `forge.IssueState` with `StateOpened = "opened"` / `StateClosed = "closed"`
  (`api/internal/forge/forge.go:405-415`); each driver already ships inbound/outbound state
  translators to reuse.
- **No schema migration.** Open/closed is the single existing `issues.state text` column
  (`api/internal/store/migrations/00002_forge.sql:47-60`); there is no `closed_at`. Close/reopen
  only flips that value in the cache after the forge write.
- **No new Card DTO field.** The card already surfaces both `State string` and `Closed bool`
  (`api/internal/handler/board.go:45-70`), so reusing them avoids the contract-test three-file
  tax (`contract_test.go`).

## Sequencing (read before starting)

**This PRD shares the three driver files with open PRD #1025** ("forge driver file splits — per
seam incl. an `issues` seam"), which is queued for the nightly `refactor` sweep. Per epic #915's
contract ("parallel PRDs never share files"), **land this PRD after #1025 merges.** By then the
issue methods (`CreateIssue`, `UpdateIssueLabels`, `UpdateIssueDescription`) will likely have
moved into per-seam files (e.g. `github_issues.go`, `forgejo_issues.go`). **Add `SetIssueState`
to whichever file holds those issue methods at implementation time**, next to
`UpdateIssueDescription` in each driver — do not assume the monolithic `github.go`/`forgejo.go`.
GitLab keeps its issue methods in `gitlab.go` unless #1025's `gitlab_pipelines.go` split moved
adjacent code; check before editing.

No other in-flight work conflicts: `handler/board.go`, `forgesvc/service.go`, `store/queries/forge.sql`,
`web/src/pages/Board.tsx`, `web/src/lib/boardOrder.ts`, `web/src/lib/api.ts` and the mock are all
free of open PRDs (the Board.tsx IssueCard extraction #1007 and the api.ts/mockApi splits #960/#991
already merged — base on current `main`).

## Verified technical facts (no open-web needed; all read locally)

The forge SDKs already carry the state field on the same edit struct each driver uses for labels
and description — no dependency bump. Pinned versions from `api/go.mod`:

> **Locate driver methods by SYMBOL, not by the line numbers below.** #1025 lands first and moves
> the issue methods into per-seam files, so every driver line cited here will be stale — grep for the
> function name (`UpdateIssueLabels`, `UpdateIssueDescription`, the `*IssueStateParam` mapper) and add
> `SetIssueState` beside it in whatever file now holds it.

- **GitLab** `gitlab.com/gitlab-org/api/client-go/v2 v2.59.1`: `UpdateIssueOptions.StateEvent *string`
  (`"close"` / `"reopen"`) — **field confirmed locally via `go doc`.** Mirror `UpdateIssueLabels`
  (`gitlab.go:374-391`) which already calls `client.Issues.UpdateIssue(projectID, issueIID, opt, ...)`.
  Outbound mapper precedent: `gitlabIssueStateParam` (`gitlab.go:812`).
- **Forgejo** `code.gitea.io/sdk/gitea v0.25.1`: `EditIssueOption.State *StateType`
  (`gitea.StateOpen = "open"` / `StateClosed = "closed"`) — **`State` field and the no-`omitempty`
  `Title` both confirmed locally via `go doc`.** Mirror `UpdateIssueDescription` (`forgejo.go:411-416`)
  which calls `EditIssue(...)`. **Hazard (documented at `forgejo.go:383-395`):** `EditIssueOption.Title`
  has no `omitempty`, so a naive edit wipes the title — the driver reads the issue first (`c.GetIssue`,
  `forgejo.go:405`) and sends the current title back. `SetIssueState` must do the same read-then-write.
  Outbound mapper to reuse: `forgejoIssueStateParam` (`forgejo.go:750`); note Forgejo spells it
  `"open"`, not `"opened"`.
- **GitHub** `github.com/google/go-github/v90`: the driver already edits issues via
  `gh.UpdateIssueRequest` (`github.go:432`, `client.Issues.Update(...)` at `:433`) — confirmed by the
  compiling driver. It carries a `State *string` (`"open"` / `"closed"`) and optional
  `StateReason *string` (`completed` / `not_planned` / `reopened`). **v90 is NOT in this machine's
  module cache, so these two fields were NOT re-verified here** — before coding, confirm with
  `go doc github.com/google/go-github/v90/github.UpdateIssueRequest` (the worker's cache has it; a
  wrong field name fails `task gate:api` at compile, so this is self-correcting). Because `State` is
  `omitempty` and Body/Title/Labels stay nil, state is sent alone with nothing clobbered. Outbound
  mapper: `githubIssueStateParam` (`github.go:1185`). Set `StateReason=completed` on close and
  `reopened` on reopen. **Semantic note:** drag-to-Closed cannot express `not_planned`, so all
  board-closes read as `completed` on GitHub — acceptable for this PRD; a "not planned" affordance is
  out of scope.

Interface-change tax (measured this session): `forge.Forge` has **25 methods** today
(`api/internal/forge/forge.go:448`). Adding one costs: the interface line, three driver impls, one
`forgetest.BaseFake` stub (`api/internal/forge/forgetest/basefake.go`, empty-struct impl returning
`notStubbed("SetIssueState")`), one entry in the table-driven `basefake_test.go`, and updating the
`"25"` / `"all 25 methods"` prose in `basefake.go:3,40` to 26. The six test fakes that embed
`BaseFake` inherit the new method for free.

Forge-first write pattern to mirror (`api/internal/forgesvc/service.go`): `AutoMove`
(`service.go:243-295`) and `SetIssueLabel` (`service.go:316-375`) both do (1) forge write first,
(2) return `store.Issue{}, err` untouched on failure (the snap-back contract), (3) re-cache only on
success. `SetIssueLabel` also carries the diff-first idempotency guard (`service.go:324-326`: skip
the forge call when the cache already matches) — copy it so a redundant close/reopen is a no-op.
The mover contract is also declared as an interface at `forgesvc/projectsync.go:147`.

Cache write: `UpsertIssueLabels` (`store/queries/forge.sql:326`) sets `state = EXCLUDED.state`
(`:347`) and deliberately omits `assignee_ids`/`board_position` so a state/label write can't clobber
them. Reopen-onto-a-column can reuse it (pass flipped `State`); a bare close can either reuse it or a
new narrow `UpdateIssueState` sqlc query — there is no state-only query today. Prefer the narrow
query for a bare close (clearer intent, touches only `state`).

## Milestones

### M1 — `SetIssueState` primitive across the forge layer
- Add `SetIssueState(ctx context.Context, projectID, issueIID int64, state IssueState) error` to the
  `forge.Forge` interface, accepting `StateOpened` / `StateClosed`.
- Implement in all three drivers, each next to its issue methods — **locate the file by grepping the
  method name (`UpdateIssueDescription` / the `*IssueStateParam` mapper), not by the stale line numbers
  above**, since #1025 moves them. Reuse the existing outbound state mappers and each driver's error
  redactor (`wrapErr`). Forgejo must read-then-write to preserve the title. **Before coding the GitHub
  impl, confirm the field with `go doc github.com/google/go-github/v90/github.UpdateIssueRequest`**
  (v90 wasn't in the author's module cache; the worker's has it).
- Add the `BaseFake` stub + `basefake_test.go` entry; bump the "25"→"26" prose.
- **Tests**: per-driver unit tests asserting the correct API param is sent for close and for reopen
  (mirror the existing `UpdateIssueLabels`/`UpdateIssueDescription` driver tests), plus a Forgejo test
  proving the title is preserved (regression for the no-`omitempty` hazard).
- **Done when**: `task gate:api` green; each driver has a close and a reopen test that fails if the
  state param is wrong.

### M2 — Forge-first close/reopen service in `forgesvc`
- Add `CloseIssue` and `ReopenIssue` (or one `SetIssueState(target)`) to `forgesvc`, mirroring
  `SetIssueLabel`: idempotency guard (skip forge when `issue.State` already matches), forge write via
  `f.SetIssueState`, then cache flip; return `store.Issue{}, err` untouched on forge failure.
- Reopen composes with a move: `SetIssueState(opened)` first, then the existing `AutoMove` label path
  to the drop-target column (Backlog target `""` clears column labels). **🔴 Cache-clobber trap:
  `AutoMove` re-writes `State: issue.State` into `UpsertIssueLabels` (`service.go:288`), and that
  query does `state = EXCLUDED.state` (`forge.sql:347`). If reopen calls `AutoMove` with the original
  (still `State:"closed"`) struct, the label write clobbers the cache back to closed — forge opened,
  cache closed, desync until the next poll.** So reopen MUST set `issue.State = "opened"` in-memory
  (or thread the row `SetIssueState` returns) before calling `AutoMove`. Ordering: state flips first;
  if the subsequent label move fails, return the reopened-but-unmoved re-cached row (state = opened)
  and let the handler surface a 502 for the move half. Close is state-only.
- **`board_position` on reopen**: both `UpsertIssue` and `UpsertIssueLabels` deliberately omit
  `board_position` (`forge.sql:300-311,:329`) and cards sort `ORDER BY board_position ASC NULLS LAST`
  (`forge.sql:377`), so a reopened card would silently inherit its stale pre-close slot. Null
  `board_position` on reopen so the card lands at the bottom of the destination lane (the intuitive
  "it comes back as new" behaviour). This needs a dedicated cache write (the two existing upserts
  can't null it) — fold it into the `UpdateIssueState`/reopen query.
- Add the narrow `UpdateIssueState` sqlc query for the bare-close cache flip and the reopen
  state+position write. Reopen-onto-a-column still routes the label change through the already
  live-tested `UpsertIssueLabels` (with `State` flipped first, per the trap above).
- **SyncFiledIssueCloses side effect (expected, no code needed)**: flipping cache state to closed
  makes the next poll's edge auto-resolve any judge recommendation filed as that issue
  (`judge_issue_close.sql`), which is desirable. Note it is **once-only** — `close_synced_at` is never
  un-stamped on reopen, so a reopen-then-reclose won't re-resolve. Document and add a test asserting
  the once-only behaviour so nobody "fixes" it later.
- **Tests**: service tests with a `BaseFake`-embedded fake overriding `SetIssueState`: close flips
  cache, reopen flips + moves, reopen leaves the cache **opened** (the clobber-regression test — must
  fail if `AutoMove` is called with the stale closed state), idempotent no-op when already in target
  state, snap-back (cache untouched) on forge error, reopen nulls `board_position`. The new
  `UpdateIssueState` query needs a `*LiveDB` test (forgesvc is already in the store-it sweep lists) —
  a green `sqlc generate` is not evidence the query runs (`.claude/rules/go.md`).
- **Done when**: `task gate:api` green; the clobber-regression, snap-back, idempotency and
  once-only-close-sync paths are each covered; the new query has a live-DB test.

### M3 — Board handler: lift the Closed guard, wire close/reopen
- In `MoveIssue` (`handler/board.go:832`), replace the `target == "closed"` 400 (`:851-854`) with a
  branch that calls the M2 close path; a move whose source card is closed and whose target is an open
  lane calls the M2 reopen path. Keep the existing forge-first-then-cache flow, the 502-on-forge-error
  behaviour, `ClearIssueRunsMovePending`, and the single-card re-render (`issueToCard`). Update the
  handler doc comment (`:827-831`) which currently says Closed is unsupported.
- **GitHub Projects v2 projection** (`ForwardMove`, `board.go:914`): `ForwardMove(target="closed")`
  hits the unmapped-column no-op (`projectsync_sync.go:88-93`), leaving the linked Projects v2 Status
  stale (e.g. "In Progress") after a close, even though a `doneOptionID` exists in provisioning
  (`projectsync_provision.go:718`). On **close**, drive the Status to Done; on **reopen**, drive it to
  the target column's Status (Backlog → the board's open/no-status default). Keep it best-effort like
  the existing `ForwardMove` (a Projects v2 failure must not fail the move). If wiring Done is larger
  than expected, it may be split to a follow-up issue — but the PRD must not silently leave the Status
  stale; state which was chosen.
- **Close with a run in flight (define, don't block)**: dragging an actively-worked card to Closed
  does **not** stop the run or the MR path — the run keeps running and an MR may open against a
  now-closed issue. `ResolveColumn` forces `closed=true` regardless of lifecycle labels
  (`board.go:24`) and `mr_watch` keeps polling the run; this is coherent. State it as intended so it
  is not "fixed" as a bug.
- Request shape: reuse `moveIssueRequest{ to_column }` with `to_column: "closed"` now accepted (the
  source card's state disambiguates reopen), so no new endpoint/DTO. **CLI parity: no change needed —
  verified there is no board/issue-move command in `api/cmd/uzi/`** (`git grep` for `MoveIssue`/
  `to_column`/`issues/.../move` under `cmd/uzi/` is empty), so this PRD adds no CLI surface.
- **Poller race (one line, pre-existing)**: `upsertIssues` writes forge state verbatim
  (`service.go:644`); a poll fetched *before* the manual close lands can momentarily write "opened"
  back over the cache flip, briefly rendering a just-closed card as open until the next poll. Same
  pre-existing race as label moves (the frontend `suppressToastIids` covers move toasts, not state
  flips). Low severity — note it, no fix required this PRD.
- **Tests**: handler tests for drag-to-Closed (close), drag-closed-to-column (reopen+move),
  drag-closed-to-Backlog (reopen+clear), and forge-failure → 502 with cache untouched. There is **no
  existing 400-message test** to update (`git grep` confirmed), so these are net-new.
- **Done when**: `task gate:api` green; the four handler paths above are covered.

### M4 — Web: Closed column as a drag target
- Make the Closed lane droppable (`Board.tsx:721`) and remove the `CLOSED_KEY` early-returns in
  `move()` / `applyDrop()` (`:662-663`, `:904-906`), translating a drop on the Closed lane to
  `to_column: "closed"` (mirror the Backlog `""`→`"open"` translation at `Board.tsx:673`).
- Allow a closed card to be dragged out: lift the `dragged.closed` bail in `dropIntent`
  (`boardOrder.ts:284`) and attach drag handlers to closed cards; on drop onto an open lane, send the
  reopen move. Keep the Closed lane non-reorderable (no board_position semantics for closed cards).
  **Reopened-card landing position**: `dropIntent` computes its anchor from `openCards` only
  (`boardOrder.ts:285`, `filter(c => !c.closed)`), and the backend nulls `board_position` on reopen
  (M2), so a reopened card lands at the **bottom** of the destination lane (NULLS LAST) regardless of
  drop anchor. Match the optimistic frontend render to that (append to the lane), so the optimistic
  position and the server truth agree.
- Update the board file-header comment (`Board.tsx:1-8`) and the API client `moveIssue`
  (`web/src/lib/api.ts:818-823`) if the call shape changes (it should not — same endpoint).
- Update the mock (`web/src/mocks/mockApi/boards.ts:83-87`) to accept `"closed"` and flip mock state,
  so `task gate:web` and the mock-mode board exercise close/reopen.
- **Tests**: board DnD unit tests — drag open→Closed sets closed; drag closed→column reopens and
  lands in that column; drag closed→Backlog reopens and clears labels; Closed→Closed is a no-op.
- **Done when**: `task gate:web` green; the DnD tests assert the emitted move payload, not just DOM.

### M5 — End-to-end acceptance + docs
- Verify the full path against a real forge fake through the existing board/e2e harness: an open issue
  dragged to Closed is closed on the forge and re-rendered in the Closed lane; a closed issue dragged
  out is reopened and lands at the bottom of the dropped lane. Do this for GitLab, Forgejo and GitHub
  driver fakes at the service/handler layer (no live forge needed).
- Docs (worker-completable): update any board/user doc that states closing stays on the forge —
  `grep -rn 'close the issue on the forge' docs/` and the MVP wording must return zero stale matches,
  and the board doc must name the new drag-to-close/reopen behaviour. Add an `ai.md` design note
  directly (allowed without approval).
- **`specs/human.md` is a maintainer step, NOT the worker's.** If a `human.md` requirement line
  references the old "close on the forge" limitation, changing it needs user approval, which an offline
  worker cannot obtain. Flag the specific line for the maintainer in the PR body; do not edit
  `human.md` in the worker branch.
- **Done when**: `task gate` fully green; `grep -rn 'close the issue on the forge' docs/` is empty and
  the board doc + `ai.md` describe the behaviour; any `human.md` line needing a change is flagged in
  the PR body for the maintainer.

## Success criteria

1. On all three forges, dragging an open card into the Closed lane closes the issue **on the forge**
   (verified by the driver sending the correct state param), then the cache reflects it.
2. Dragging a closed card onto an open lane reopens it on the forge and places it at the bottom of
   that lane (configured column applies the label; Backlog clears column labels; `board_position`
   nulled).
3. Reopen leaves the cache **opened** — the label-move step never clobbers state back to closed
   (the M2 clobber-regression test proves it).
4. A forge write failure leaves the cache untouched and the card snaps back (502 surfaced), matching
   the existing `AutoMove` contract.
5. A redundant close/reopen (already in the target state) makes no forge call.
6. A GitHub-linked Projects v2 board's Status is driven to Done on close (and to the target column's
   Status on reopen), best-effort — or the chosen deferral is explicitly recorded, never silently
   stale.
7. No new Card DTO field and no schema migration; `task gate` green across api + web.
8. Forgejo close/reopen preserves the issue title (no-`omitempty` regression guarded by a test).

## Risks & mitigations

- **File collision with #1025** (forge driver splits): mitigated by the Sequencing section — land after
  #1025 and place `SetIssueState` in the post-split issue-seam file, located by symbol.
- **Cache-clobber on reopen** (the sharpest correctness risk): `AutoMove` re-writes `state` from its
  input struct, so reopen must flip `issue.State` to opened before the label move — see M2's trap
  callout, guarded by the M2 clobber-regression test.
- **Forgejo title clobber**: mitigated by read-then-write, with a dedicated regression test (M1).
- **`board_position` on reopen**: reopened cards would inherit a stale slot; M2 nulls it so they land
  at the bottom. Frontend optimistic render must match (M4).
- **GitHub Projects v2 stale Status**: `ForwardMove` no-ops on the Closed target today; M3 drives the
  Status to Done on close (or records the deferral).
- **Reopen partial failure** (state flips, move fails): defined behaviour in M2 (reopen-but-unmoved row
  returned with state=opened, 502 for the move half); the card still renders open, never lost.
- **Close with a run in flight**: intended, not blocked (M3) — the run keeps running.
- **Poller race**: a stale poll can briefly re-render a just-closed card as open (pre-existing class,
  low severity, noted in M3, no fix this PRD).
- **Reopen of an issue with no column labels**: not a gap — `AutoMove` with `target=""` computes empty
  add/remove sets and makes no forge label call (`service.go:260,273`), a clean re-cache.
- **CLI parity**: none needed — no board/issue-move command exists in `api/cmd/uzi/` (verified).

## Files touched (for reviewer file-disjointness checks)

`api/internal/forge/forge.go`, the three driver files (post-#1025 issue-seam files),
`api/internal/forge/forgetest/basefake.go` (+`basefake_test.go`), `api/internal/forgesvc/service.go`
(+ `projectsync.go` interface line), `api/internal/store/queries/forge.sql` (new `UpdateIssueState`),
`api/internal/handler/board.go`, `web/src/pages/Board.tsx`, `web/src/lib/boardOrder.ts`,
`web/src/lib/api.ts`, `web/src/mocks/mockApi/boards.ts`, plus tests and docs. No `.github/workflows/**`
(worker PAT lacks workflow scope). No schema migration.
