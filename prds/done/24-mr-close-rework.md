# PRD #24: MR Closed Without Merging → Card Back to In Progress

**GitLab Issue**: [#24](https://gitlab.example.com/vtmocanu/uzi/-/issues/24)
**Status**: Complete (2026-07-05)
**Priority**: Medium
**Created**: 2026-07-05

**Depends on**: nothing in-flight. Independent of PRD #19 (admin settings/autopilot) and #23 (web UX) — no shared tables, no web surface. Coordination only: migration `00029` reserved (see ledger in Technical Design §2), and both #19 M4 and this PRD's M3 add post-sync calls in `poller.syncRepo` + methods in `forgesvc` — whichever lands second rebases (textual, not semantic). Composition note: a #19 autopilot rework run is a "latest non-completed run" and suppresses this PRD's watcher by design (Decision 4).

## Problem

The board's column automation is driven exclusively by *run status* (`api/internal/runlifecycle/lifecycle.go`): `queued` → In Progress, `completed` → Human Review, `failed`/`cancelled` → origin. Nothing anywhere watches **MR state**. So when a reviewer closes an agent's MR *without merging* — the "rejected, redo it" signal — the card stays parked in Human Review forever, and the `Human Review` label lingers on the GitLab issue. Observed live: issue #9 / MR !13 (MR closed 2026-07-05, card stuck in Human Review).

The merge path already works end to end: the agent's MR carries `Closes #N`, so a merge closes the issue on GitLab, the poller syncs the state change, and the card lands in Closed. Only the close-without-merge path has no automation.

## Solution Overview

Watch the MR each completed run reported (`runs.mr_iid`, written by the worker on completion — `api/internal/workersvc/service.go:417,450`). When that MR transitions **opened → closed (not merged)**, move the card forge-first from Human Review back to **In Progress**, with the exact guards the run-lifecycle automation already enforces: never fight a manual drag, never touch a closed issue. Merged MRs trigger nothing (the existing `Closes #N` → issue-close → sync path owns that outcome).

## Design Decisions

1. **Target column is In Progress** (user, 2026-07-05). A closed-unmerged MR means "rework needed". In Progress is where a rework run would park the card anyway (the run-lifecycle `queued` → In Progress move becomes a no-op if the card is already there), so pre-positioning it there is coherent with the existing state machine. It does *not* claim a run is active — In Progress reads as "being worked / needs work", same as a manual drag there does today.
2. **Detection is poller-based, not webhook-based.** uzi's deployment target is a laptop where only `web` publishes a loopback-only port — GitLab can never reach in, so webhooks are structurally unavailable. The existing poller (`api/internal/poller`) already ticks every repo; MR-state checking joins that tick. This also inherits the poller's redaction, timeout, and bounded-concurrency posture for free.
3. **Edge-triggered, not level-triggered.** The move fires once per opened→closed transition, tracked by persisting the last-seen MR state on the run (new column `runs.mr_state`). A level trigger ("MR is closed and card is in Human Review → move") would re-fight a human who deliberately drags the card back to Human Review after the close; an edge trigger touches the board exactly once per state change. First observation of an MR (NULL → anything) records state without acting, so pre-existing closed MRs at upgrade time do not cause a wave of moves — except the very case that motivated this PRD (see Decision 9).
4. **Scope: only agent-created MRs uzi knows about — and the candidate is the issue's latest run *overall*, watched only while that run is `completed`** (review finding 1). Take the issue's most recent run (mirroring `ListLatestRunsForRepo`'s `DISTINCT ON` at `api/internal/store/queries/forge.sql:145-154`); if its status is not `completed`, the issue has **no** watch candidate — an in-flight rework run suppresses the watch entirely, so reopening the old MR mid-rework can never yank the card away from the active run. If the latest run is `completed` but its own `mr_iid` is NULL, there is likewise no candidate — never fall back to an older run's MR (review finding 3: "latest completed run WHERE mr_iid IS NOT NULL" would silently watch a superseded MR). A human closing some unrelated MR that happens to mention the issue does nothing. This keeps the check cheap (one `GetMergeRequest` per watched card per poll, at most) and avoids guessing at MR↔issue relationships beyond what uzi itself recorded.
5. **Same guard contract as run-lifecycle moves.** The move goes through `forgesvc.AutoMove` (single-column enforcement, forge-first; on forge error AutoMove leaves the cache untouched and the *caller* owns recovery — `api/internal/forgesvc/service.go:99-103`) and is skipped when: the issue is closed; or the card is not currently in Human Review (a human already placed it elsewhere — manual drags win, mirroring `runlifecycle.apply`'s baseline check). A skipped move still records the observed MR state (the edge is consumed; we never retry a move a human pre-empted).
6. **Reopened MR moves the card back to Human Review, symmetrically.** The closed→opened (reopen) edge restores the card Human Review-ward under the same guards (issue open, card currently in In Progress where the close-edge left it). Without this, a reviewer who closes by accident and immediately reopens leaves the board lying. Same edge-trigger discipline: fires once per transition. The mid-rework misfire (reopen while a rework run is active and has itself parked the card in In Progress) is closed by Decision 4's candidate rule — a non-`completed` latest run means no watch at all.
7. **The run record is untouched except for `mr_state`.** The run stays `completed`; closing an MR is review feedback, not a run-status event. No new run statuses, no `run_messages`, no lifecycle notification. The run-lifecycle reconciler and this watcher act on disjoint triggers (run-status markers vs MR-state edges) and cannot fight: after a completed run's move marker is resolved, run-lifecycle never touches the card again.
8. **Forge interface grows one method.** `GetMergeRequest(ctx, projectID, mrIID) (MergeRequest, error)` with a neutral `MergeRequest{IID, State, ...}` domain type (`state`: `opened|closed|merged|locked` on GitLab). Errors pass the existing PAT-scrubbing redactor; only the GitLab driver implements it today, consistent with the forge-seam rules (`api/internal/forge/forge.go`).
9. **Bootstrap catch-up is explicit.** On the first check of a run whose `mr_state` is NULL (pre-migration runs, e.g. issue #9's), the state is recorded **without moving** — with one deliberate exception documented in docs: users with already-stuck cards (like #9) fix them with a manual drag, which the guards respect. Rationale: acting on NULL→closed cannot distinguish "closed yesterday, human already triaged it elsewhere and dragged it back" from "closed and stuck"; silently moving on stale history risks fighting a decision we never saw. One manual drag beats one wrong automated move.
10. **Cost bound: the watcher only polls MRs for cards sitting in Human Review** (with an open issue and a latest-completed-run `mr_iid`). Board-wide that is typically zero to a handful of MRs per repo per tick; no high-water-mark machinery is needed. Cards in In Progress that got there via the close edge are also checked (for the reopen edge of Decision 6) — bounded by the same small set, tracked via `mr_state = 'closed'` on the latest completed run.

## Technical Design

### 1. Forge layer (api/internal/forge)

- Neutral type `MergeRequest{IID int64; State string; WebURL string}` (add fields only as needed).
- Interface method `GetMergeRequest(ctx, projectID, mrIID int64) (MergeRequest, error)`; GitLab driver implements via the official client-go v2 already in use (`MergeRequests.GetMergeRequest(pid, iid, nil)` — `MergeRequest.State` is `opened|closed|merged|locked`, distinguishable from the single-MR GET; confirmed against client-go v2.44.0 in `api/go.mod`) with the existing redaction and timeout plumbing. Driver test with a stub server (match `gitlab_test.go` conventions).

### 2. Schema + queries (api/internal/store)

- Migration **`00029`** (reserved; ledger: `00021` live head, `00022` #17, `00023`–`00028` #18, `00030+` #5, `00036`–`00039` #19, `00040+` #6, `00050+` #16): `ALTER TABLE runs ADD COLUMN mr_state text;` (NULL = never observed), with `+goose Down`. No backfill (Decision 9). `mr_state` is **watcher-owned**: no run-status path writes it (requeue paths, `runtime.sql:291-319`, touch only non-terminal runs; `SetRunCompleted` re-writes `mr_iid` but never `mr_state`) — assert this in query tests (review finding 11).
- Queries: `ListMRWatchCandidates` — per repo, `DISTINCT ON (issue)` latest run overall, **then** filter `status = 'completed' AND mr_iid IS NOT NULL` (order matters, Decision 4), issue open, and a **coarse** column prefilter (issue labels contain `Human Review` **or** `mr_state = 'closed'` — reopen watch, Decision 10). The SQL prefilter is deliberately *not* `board.ResolveColumn` (highest-position-wins across multiple column labels is not cheaply expressible in SQL); the authoritative source-column check is the Go guard in the watcher (review finding 2). Plus `SetRunMRState(runID, state)`. Regenerate with sqlc.

### 3. Watcher (api/internal/forgesvc + poller)

- New `forgesvc` method (e.g. `SyncMRStates(ctx, repoID, forgeProjectID, f)`) called from `poller.syncRepo` after the issue sync each tick, so MR checks see the freshest issue cache. This widens `forgesvc.Service`'s store seam beyond today's narrow `IssueStore` (`api/internal/forgesvc/service.go:54-57`): the watcher needs `ListMRWatchCandidates`, `SetRunMRState`, `GetIssueByIID`, `ListBoardColumns` — extend the interface (keeping it fake-able) or give the watcher its own store seam (review finding 9; pick at implementation time).
- Per candidate: `GetMergeRequest` → compare to stored `mr_state` → on `opened→closed` edge apply guarded move to In Progress; on `closed→opened` edge apply guarded move to Human Review. **State-persistence contract (review finding 4)**: `mr_state` advances only on (a) move success, (b) a deliberate guard-skip (manual drag / closed issue — the edge is consumed, we never re-fight a human), or (c) no-op observations (`merged`, `locked`, NULL bootstrap). On a **forge-side move failure the prior state is left in place**, so the next poller tick re-observes the same edge and retries — the poller cadence *is* the retry loop; consuming the edge on failure would re-create exactly the stuck-card bug this PRD fixes, and run-lifecycle's equivalent guarantee is its durable `move_pending_since` marker (`lifecycle.go:303-308`).
- **Write ordering: forge move first, then `SetRunMRState`** (review finding 5). A crash in between re-fires the edge next tick, and the source-column guard makes the retry a no-op (card already moved) — except the narrow case where a human dragged the card back to the source column in the crash window, which then gets re-moved once; accepted (crash-window-sized exposure) and documented in the watcher's code comments.
- Guards reuse the run-lifecycle semantics but are simpler: current column (via `board.ResolveColumn`) must equal the edge's expected source column; issue must be open. This is a third instance of the load-issue → closed-skip → resolve-column → baseline-compare → `AutoMove` shape (`runlifecycle.apply`, `lifecycle.go:240-317`): factor a shared guarded-move helper if it falls out cleanly, otherwise keep the copies small and cross-referenced (review finding 6).
- The watcher records only `mr_state`, never `runs.board_column` — after a close-edge move, a completed run's `board_column` still says Human Review while the card sits in In Progress. Safe **only** because completed runs are terminal for the run-lifecycle (marker already resolved, no further status transitions); note this at the `board_column` definition so future code never re-stamps `move_pending_since` on completed runs (review finding 7).
- Forge errors on the read (`GetMergeRequest`): log-and-skip that candidate (poller convention: one bad repo/call never stalls the tick).

### 4. Web

- No new surfaces required: the board converges via the existing sync, and MR links already render on cards/run views. Optional (nice-to-have, not a milestone gate): show the MR state (`closed`/`merged`) next to the existing `MR !N` chips (`web/src/pages/IssueView.tsx:215`, `RunsList.tsx:45`) if the run API starts exposing `mr_state`. Prior art if built: multica's derived PR-status enum (`inspiration/multica/packages/core/github/pull-request-status.ts`) — precompute a display status, don't surface the raw forge state string (it is display-only there, webhook-fed; no prior art exists for the automation itself).

### 5. Docs + specs

- `docs/`: board-automation page gains the MR-close/reopen behavior + the Decision 9 note (pre-existing stuck cards: one manual drag).
- `specs/ai.md`: decisions above. `specs/human.md` (needs user approval): the close-unmerged → In Progress story.

## Milestones

- [ ] **M1 — Forge method**: `MergeRequest` type + `GetMergeRequest` on the interface and GitLab driver, redaction-safe, driver tests.
- [ ] **M2 — Schema + queries**: `runs.mr_state` migration, `ListMRWatchCandidates` + `SetRunMRState`, sqlc regenerated, query tests.
- [ ] **M3 — Watcher end-to-end**: `forgesvc.SyncMRStates` wired into the poller tick (store seam widened per Technical Design §3); edge detection, both directions, all guards (manual drag, closed issue, NULL bootstrap, merged/locked no-op), the state-persistence contract (forge failure leaves the edge unconsumed → next-tick retry), move-then-state write order; unit tests with fake forge covering the transition matrix incl. the forge-failure retry and the rework-suppression case.
- [ ] **M4 — Docs + specs**: docs page section, `specs/ai.md` update, `specs/human.md` proposal for user approval.
- [ ] **M5 — E2E validation**: isolated stack scenario — completed run with MR, fake forge flips MR to closed → card moves to In Progress; flip back to opened → card returns to Human Review; manual-drag pre-emption case.

## Open Questions

1. Should the reopen edge (Decision 6) also fire when the card sits in a column *other than* In Progress because a rework run has since started? Current answer: no — guards require the expected source column; revisit if it bites.
2. Expose `mr_state` on the runs API for the optional web chips (Technical Design §4)? Decide during M3.

## Risks

- **Racing a rework run**: a human closes the MR and immediately starts a new run. The run-lifecycle `queued` → In Progress move and the watcher's close-edge move target the same column, so the worst case is a redundant no-op move. And the moment the rework run exists it becomes the issue's latest run with a non-`completed` status, which suppresses the watch entirely (Decision 4) — neither edge can fire mid-rework.
- **Poll latency**: the board reflects an MR close only on the next poller tick (default interval). Acceptable for a laptop tool; same latency every other forge-side change already has.
- **GitLab `locked` state churn**: transient `locked` during merge processing is recorded but never acted on, and the subsequent `merged` observation is also a no-op — no spurious moves.

## Decision Log

- 2026-07-05: PRD created (user request, prompted by issue #9 / MR !13 stuck in Human Review). Target column In Progress chosen by user; poller-based detection; edge-triggered with `runs.mr_state`; symmetric reopen handling; NULL-bootstrap records-without-moving.
- 2026-07-05: Review round (reviewer + fact-checker agents). Fact-check: all 12 codebase claims confirmed (incl. live #9/!13 state and closed-vs-merged distinguishability on GitLab's single-MR GET); AutoMove "snap-back" attribution corrected (caller-owned recovery). Review fixes applied: candidate redefined to latest-run-overall-watched-only-if-completed (kills the mid-rework reopen misfire and the superseded-MR fallback); SQL column filter demoted to coarse prefilter with the Go `ResolveColumn` guard authoritative; state-persistence contract pinned (forge failure leaves the edge unconsumed → poller-tick retry, preventing re-introducing the stuck-card bug); move-then-state write order + crash-window caveat documented; `forgesvc` store-seam widening, guard-helper factoring, `board_column` divergence, and watcher-owned `mr_state` invariant called out.
- 2026-07-05: Reviewer hardening (post-audit, M1–M3 implementation). The watcher records only KNOWN MR states (`opened|closed|merged|locked`); an unrecognized or empty forge state is ignored entirely (no `mr_state` write) in both the bootstrap and transition paths, leaving the prior baseline (or NULL) intact so a transient glitch self-heals. This reverses an earlier "record unknown states verbatim" choice: because edges fire on exact string compares, a garbage baseline would mask the next real close until a full reopen cycle re-synced it.
