# PRD #1253: Run merged signal — MR-state coverage for helper runs + louder merged chip

**Issue**: [#1253](https://github.com/vtmocanu/uzi/issues/1253)
**Priority**: Medium
**Status**: Draft

## Problem

A run's MR/PR chip only reaches `merged` for the **issue-lane** run. Every other run that shares the same PR (an `mr_rework` rework run, a `ci_fix` run, and any other issue-less MR-bearing kind) keeps `mr_state = null` for its whole life, so its chip renders a plain "PR #N" with no state word even after a human merges the PR.

Confirmed on the live instance (repo `vtmocanu/uzi`, PR #1244 merged on the forge 2026-09-11, PR #1250 merged 2026-09-12):

| run kind | `issue_iid` | `mr_iid` | stored `mr_state` |
|---|---|---|---|
| `issue` (#1224) | 1224 | 1244 | `merged` (correct) |
| `mr_rework` (x3) | null | 1244 | `null` |
| `ci_fix` | null | 1244 | `null` |
| `issue` (#1249) | 1249 | 1250 | `merged` (correct) |

All runs on PR #1244 are on a merged PR, yet only the issue-lane run recorded it. The two MR-state watch lanes leave a hole:

- **Board lane** (`ListMRWatchCandidates`, `api/internal/store/queries/forge.sql:585`): JOINs `issues` on `issue_iid`, so any run with `issue_iid IS NULL` is dropped before it can be watched.
- **Board-free lane** (`ListScheduledMRStateWatchCandidates`, `forge.sql:625`): only `kind IN ('prompt','self_improve')`.

So an issue-less, MR-bearing run of kind `mr_rework`, `ci_fix`, `chat`, or `task` is watched by **neither** lane. Its `mr_state` never advances past whatever it held at run completion (here `null`, never observed). PRD #908 added the board-free lane precisely because issue-less runs need a board-free watcher, but enrolled only the two scheduled kinds and missed these helper kinds.

Two consequences:

1. **Display**: the merged/closed signal is silently absent on helper runs (cosmetic, but the exact question the maintainer asked: "for yesterday it does not show merged, is this a bug?" — yes).
2. **Behavior**: the board-free lane is also what populates `mr_state` so `ListMRReworkCandidates`' `mr_state='opened'` gate, the mr_rework ledger eviction, and stop-on-close cancellation work for these lanes (its own doc comment says so, `forge.sql:619`). An `mr_rework` run whose `mr_state` never advances is a latent gap in that machinery, not only a display miss.

Separately, even when `mr_state` **is** `merged`, the chip is only plain green text in a dotted meta line and is easy to miss at a glance.

## Solution

Two independent, disjoint-file changes shipped together, because change (2) makes the merged chip louder and would otherwise **amplify** the inconsistency change (1) fixes (a bold green "merged" badge conspicuously absent on the helper runs of the same PR).

1. **Widen the board-free MR-state watch lane** to cover issue-less MR-bearing runs, so `mr_state` reaches its terminal value (merged/closed) for helper runs. The lane's existing self-bounding predicate + LIMIT 100 burst bound backfill historical orphaned runs automatically on the next poller tick.
2. **Option A merged chip**: render a merged inline MR chip as a check icon + tinted bordered chip (louder, still a deep-link to the forge PR). `open` and `closed` inline chips are unchanged; the pill variant (board card, dashboard) already carries a check and is untouched.

## Design decisions

**D1 — Widen the board-free lane by a structural predicate, not a growing kind list.**
Change `ListScheduledMRStateWatchCandidates`' `WHERE kind IN ('prompt','self_improve')` to:

```
AND (issue_iid IS NULL OR kind IN ('prompt', 'self_improve'))
```

keeping the rest of the predicate (`status = 'completed'`, `mr_iid IS NOT NULL`, `mr_state IS NULL OR mr_state IN ('opened','locked')`, `ORDER BY created_at DESC`, `LIMIT 100`).

This is **provably disjoint** from the board lane, so no run is recorded-and-cancelled by both lanes in one tick:

- `issue_iid IS NULL` runs cannot enter the board lane at all (its `JOIN issues i ON i.forge_issue_iid = l.issue_iid` drops a null key).
- `prompt` / `self_improve` runs are excluded by the board lane's `latest` CTE (`kind NOT IN ('prompt','self_improve')`), exactly as today; `self_improve` carries a tracking-issue iid but is kind-excluded.

The predicate closes the gap for every current and future issue-less MR-bearing kind (`mr_rework`, `ci_fix`, and any issue-less `chat` / `task`) without re-editing a kind list each time a kind is added. `judge` runs carry no MR, so `mr_iid IS NOT NULL` already excludes them.

**Residual (accepted, unchanged):** a helper run that *does* carry an `issue_iid` but is not the board lane's DISTINCT-ON latest for that issue stays orphaned. That is the existing, documented "a superseded run can display a stale MR state" caveat, out of scope here. Observed reality is that helper runs are issue-less (see the table above), so this residual is rare.

**D2 — Rename the lane to `*BoardFree*` for doc honesty (recommended).**
Once the lane covers non-scheduled kinds, the names `ListScheduledMRStateWatchCandidates` / `SyncScheduledMRStates` / `recordScheduledMRState` and their "scheduled lanes (prompt, self_improve)" doc comments become wrong. Rename to `ListBoardFreeMRStateWatchCandidates` / `SyncBoardFreeMRStates` / `recordBoardFreeMRState` and rewrite the doc comments to describe the lane by what defines it (board-freedom), not by the two kinds that first used it. This follows the repo's fix-the-doc-the-moment-you-find-it-wrong rule. If the rename churn (query name, sqlc-generated symbol, poller wiring at `api/internal/poller/poller.go:379`, tests) is judged out of scope during review, the **floor** is correcting the doc comments in place; do not leave them claiming the lane is scheduled-only.

**D3 — Keep PRD #908's self-bounding terminal-state design.**
The board-free lane still drops a run once `mr_state` is terminal (merged/closed) and does not re-watch a closed-then-reopened MR. Widening the kinds does not reintroduce the unbounded-growth cost that design rejected. No `closed -> opened` arm is added.

**D4 — Backfill is automatic and bounded.**
Because the self-bounding predicate admits `mr_state IS NULL OR IN ('opened','locked')`, deploying the widened query causes the next poller tick to pick up historical orphaned runs and record their real forge state, bounded to 100 per repo per tick. No data migration and no backfill script. Acceptance: after deploy, the four PR #1244 helper runs (`mr_rework` `7260f894`, `01f7302b`, `9bb4a638`; `ci_fix` `31932c35`) show `mr_state=merged` via `uzi run get <id> --json`.

**D5 — Rework-cancel edge now fires for these kinds; desired, no new hazard.**
`recordBoardFreeMRState` (renamed) already cancels an in-flight rework via `cancelReworkOnClosedMR` on the opened->closed / ->merged edge. Firing this for `mr_rework` / `ci_fix` runs is correct (same PR, same intent) and introduces no new cancel path; it reuses the issue #853 contract verbatim.
New but benign: the board-free lane has no DISTINCT ON, so several runs sharing one PR (the three `mr_rework` runs on PR #1244) each become candidates and each call the cancel on the merge edge, a same-MR multiplicity the prior prompt/self_improve lanes (1:1 with their MR) never had. It is safe: `CancelReworkForMR` -> `GetActiveMRReworkRunForMR` is a `:one` returning nil on `ErrNoRows` (`api/internal/forgesvc/mr_rework_cancel.go`), so the first cancel makes the rework terminal and every later call in the same tick no-ops.

**D6 — Option A scope is the inline variant's merged state only.**
`MrChip` `variant="inline"` merged renders as: a `CheckIcon` (already exported from `web/src/components/icons.tsx:190`) + the label/number/"merged" word, in a tinted bordered chip (`border-ok/40 bg-ok/10 text-ok`, rounded, `text-[11px] font-medium`). `open` (brand or ok per `openTone`) and `closed` (muted, struck number) are unchanged. The chip stays an `<a>` deep-link when `href` resolves (unchanged link logic). The `variant="pill"` path (board card `IssueCard.tsx`, `Dashboard.tsx`) already carries a check and is not touched.
Implementation note: this is an **additive restructure of the inline branch**, not a className tweak. Today the `CheckIcon` in `body` is gated `variant === "pill" && merged` (`MrChip.tsx:56`) and the inline branch (`MrChip.tsx:84-104`) is color-only with no border. The implementer emits the bordered check chip for inline **merged only**, leaving inline `open` (a test pins it as *not* `rounded`, `MrChip.test.tsx:50`) and inline `closed` exactly as they are. The word "merged" stays inside the chip (`MrChip.tsx:62`), so no string is retired.

**D7 — No DTO, migration, or contract-fixture change.**
`mr_iid` / `mr_state` already ship on `LatestRun` and `Run`/`RunListItem` (`web/src/lib/apiTypes.ts`); the backend already maps them (`api/internal/handler/runs_dto.go:114`). This PRD changes only which runs get `mr_state` written and how the merged chip renders. No goose migration, no `fixtures/api-contract` churn.

## Milestones

Phase 1 (parallel, disjoint files): M1 and M2 touch non-overlapping trees (api SQL + `forgesvc` + poller vs. `web/`), so they can be implemented as independent agents. Phase 2 (M3) depends on both.

- [ ] **M1 — Board-free lane covers issue-less MR-bearing runs, with a live-DB regression test.**
  - Widen `ListScheduledMRStateWatchCandidates` per D1; `sqlc generate` and confirm the generated const in `api/internal/store/*.sql.go` moved (per `.claude/rules/go.md` mutation-testing note: the generated const is what executes).
  - Apply the D2 rename (or, if descoped in review, fix the doc comments in place).
  - Regression test pinned to this bug's seam: a completed `mr_rework` run with `issue_iid = NULL`, `mr_iid = X`, `mr_state = NULL`, forge reports `merged`, is returned by the widened candidate query and, after `SyncBoardFreeMRStates`, has `mr_state = 'merged'`. It must **fail on the unfixed query** (kind not in the old list, issue-less, so not a candidate; `mr_state` stays null) and pass after. Model it on the existing prompt-run test at `api/internal/forgesvc/scheduled_mr_watch_livedb_test.go` (which uses a `fakeForge`, so this is offline); rename the file with the symbols if D2 is taken. Watch both directions.
  - When adding an issue-less `mr_rework` **included** fixture, update the store-package test `TestScheduledMRStateWatchCandidatesLiveDB` (`api/internal/store/*scheduled_mr_state_livedb_test.go`): it asserts an **exact** candidate count (`len == 3`) and carries a doc comment quoting the old SQL verbatim, so a new INCLUDED row bumps the count and staleness the comment. (The widen alone does not break it: its one non-scheduled fixture is `kind='issue'` with a non-null `issue_iid`.)
  - A store-level test asserting lane **disjointness** (the D1 invariant): the load-bearing direction is "an issue-less helper run appears in the board-free candidate set and **not** in the board candidate set". Its mirror ("issue-lane latest run in the board set, not board-free") also requires seeding an `issues` row in a Lane-A/Lane-B state (opened + Human Review, or closed + non-terminal `mr_state`) or the board lane returns nothing; the machinery exists in `api/internal/store/mr_watch_integration_test.go` and both queries live on the same `store.Queries`.
  - `task gate:api` green; the live-DB sweep via `./e2e/run-store-it.sh` green (a live-DB test is not verified until it has executed against a real database, per `.claude/rules/go.md`).
  - **M1 closes on the tests + gates**, not on the four PR #1244 helper runs flipping to `merged`: that backfill (D4) needs a live poller tick against the real forge and is a post-deploy dev-cluster check, not a worker-blocking criterion.
  - Offline-worker note: this milestone runs `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`; the worker needs `sqlc@v1.31.1` already in its module cache (a cold cache would force a network fetch the restricted egress blocks).

- [ ] **M2 — Option A merged chip (web), still a link, with tests.**
  - Implement D6 in `web/src/components/MrChip.tsx` (inline merged -> check + tinted bordered chip). `RunsList.tsx` and `IssueView.tsx` call sites need no change (they already pass `variant="inline"`); confirm the `IssueView` `openTone="brand"` path still renders merged as ok-toned (merged is ok everywhere by contract).
  - Add a **positive** assertion in `web/src/components/MrChip.test.tsx` for the new merged treatment (the bordered chip / check present for inline merged). No negative-assertion repointing is needed: no string is retired, and the existing inline-merged test (`MrChip.test.tsx:54-60`) stays green because `CheckIcon` is `aria-hidden` and does not change `textContent` (`"!7 merged"`). Also assert inline `open` stays non-`rounded` and inline `closed` unchanged.
  - Verify the chip is unchanged for `open` and `closed`, and that a null `href` still degrades to plain text (never absent).
  - `task gate:web` green.

- [ ] **M3 — Docs, specs, and full gate.**
  - `specs/ai.md`: note the board-free lane now covers all issue-less MR-bearing runs (D1) and the merged-chip inline treatment (D6). No `specs/human.md` change is required (bug fix + presentation polish, no new user-stated requirement); if a terse sync line is warranted, tag it `(AI-synced YYYY-MM-DD)`.
  - Confirm no doc comment left stale by the rename remains (`forge.sql`, `scheduled_mr_watch.go`, `poller.go`); M1 owns those under D2's floor, so this is a verification pass, not a second edit of the same comments.
  - `check:migration-numbering` and `check-docs` unaffected (no migration, no `docs/*.md` change expected); run `task gate` (all components) green.

## Validation strategy

- **M1 correctness** is verified by the live-DB regression test executing (not by `sqlc generate` alone) and by the disjointness store test. Cite the named PASS/FAIL, not a bare tally (`.claude/rules/go.md`, the `PASS=0` family).
- **Backfill** (D4) is verified post-deploy on the dev-cluster: the four PR #1244 helper runs show `mr_state=merged` after a poller tick. This is an offline-observable check (`uzi run get --json`), not an internet lookup.
- **M2** is verified by `MrChip.test.tsx` and a mock-mode browser pass on the runs list / issue history (rendering, contrast, that merged now reads at a glance, that open/closed are unchanged, and that the chip is still clickable).

## Risks

- **R1 — Extra forge reads per tick.** The widened lane calls `GetMergeRequest` once per orphaned candidate. Bounded by the existing LIMIT 100 and self-eviction at terminal state; steady-state cost is only newly-completed non-terminal helper runs. Same burst bound the two existing lanes already accept.
- **R2 — Rename blast radius.** D2 touches the query name, the sqlc-generated symbol, the poller wiring (`poller.go:379`), and a wider test surface than "tests" implies: `api/internal/forgesvc/sync_test.go`'s fake-store field (`scheduledCandidates` / `scheduledCandidatesErr`), its `ListScheduledMRStateWatchCandidates` method and `scheduledCand` helper, plus `scheduled_mr_watch{_,_livedb}_test.go` and the store-package `*scheduled_mr_state_livedb_test.go`. Mechanical but wide. Mitigation: the rename is separable from the behavior fix; the doc-comment correction is the non-negotiable floor.
- **R3 — Lane double-ownership.** The whole correctness of D1 rests on the two candidate sets staying disjoint. Mitigation: the M1 disjointness store test is the guard; it must fail if a future predicate change lets a run into both sets.

## Decision log

- Chose a structural predicate (`issue_iid IS NULL OR kind IN (...)`) over appending kinds to a list, so a newly-added MR-bearing kind cannot silently reopen the gap (D1).
- Bundled the web chip and the backend fix into one PRD because the louder chip amplifies the exact inconsistency the fix removes; they touch disjoint files and ship as parallel milestones.
- Kept PRD #908's self-bounding, no-reopen board-free design intact (D3); this PRD widens *which runs* the lane sees, not *how long* it watches them.

## Not doing

- Watching non-latest issue-lane runs (the superseded-run staleness caveat) — out of scope, unchanged behavior.
- A new run-status pill for merged — rejected earlier: merge state is orthogonal to run status, and a status-shaped pill would imply live authority the frozen `mr_state` hint does not have. Option A keeps the signal in the MR lane (the existing chip).
- Any DTO / migration / contract-fixture change (D7).
