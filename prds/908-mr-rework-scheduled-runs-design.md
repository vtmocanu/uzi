# PRD #908 — Design: MR rework for scheduled runs (prompt + self_improve)

**Status**: Proposed (design for review) — companion to `prds/908-mr-rework-scheduled-runs.md`
**Author**: architect
**Scope**: `api/` + `docs/` only. **No `agent/` change, no `web/` code change, no new migration.**

This document supersedes the M1/M2/M3 shape of the draft PRD, which is insufficient
against the `runs.mr_state` blocker (below). It records the decision, the mechanism,
the milestone decomposition, and the sequencing.

---

## 0. Two premises re-verified at HEAD (one refuted)

The dispatch carried two claims about a moving tree. Both were re-checked against the
working tree at `HEAD = 32e7af7` (`chore(release): 0.73.0`).

**Refuted — the #887 dependency is already discharged.** The dispatch says the
self_improve `fetchAgentBranch` D/F-conflict fix is "unmerged as of main HEAD 736df08."
That HEAD is stale: `736df08` is now *behind* the working tree, and the #887 fix has
landed. Verified:

```
$ git merge-base --is-ancestor 59cca21 HEAD && echo MERGED
MERGED                       # 59cca21 = "clear legacy flat tracking ref that D/F-conflicts…" (#887)
$ git merge-base --is-ancestor 2bd3dfd 736df08 || echo "not in 736df08"
not in 736df08               # PR #902 merge is NOT an ancestor of the dispatch's HEAD…
$ git log --oneline 736df08..HEAD | grep 887
2bd3dfd Merge pull request #902 from vtmocanu/agent/issue-887   # …but IS between it and HEAD
```

`agent/src/git.ts` `fetchAgentBranch` now calls `clearConflictingAncestorTrackingRefs`
(git.ts, search `issue #887`) before the fetch, and issue #887 is CLOSED. The whole
premise of task item #3 ("sequence self_improve after #887 lands or gate it") is
therefore satisfied by the merge. **No gating construct is needed.** The sequencing
recommendation in §7 stands on other grounds, not on #887.

**Confirmed — the agent side needs no change at all.** An `mr_rework` run resolves its
fold-onto branch from `claim.branch` (populated server-side from `runs.pipeline_ref`)
and seeds off `refs/remotes/origin/<branch>` for *any* branch name — see
`agent/src/runner.ts` `if (claim.kind === "mr_rework")` (search that string; the clone
call is `runnerCloneForBranch(barePath, mrBranch, mrBranch.replace(/\//g, "-"), runId)`).
`uzi/prompt-<id>` and `uzi/self-improve/<id>` slug to `uzi-prompt-<id>` /
`uzi-self-improve-<id>` with no special-casing. The review-comment snapshot builder is keyed on the MR, not the
branch or kind: `BuildReviewCommentsSnapshot(comments, botForgeUserID)`
(`api/internal/workersvc/review_comments.go`, search `func BuildReviewCommentsSnapshot`),
fed from `f.ListMergeRequestComments(ctx, projectID, mrIID)`. This closes draft D3 as a
read-verified fact, not an assumption to observe later.

---

## 1. Approach

### The requirement's difficult point

The MR-reworker (`kind='mr_rework'`, `api/internal/poller/mr_review_watch.go`) selects
candidates with `ListMRReworkCandidates` (`api/internal/store/queries/mr_rework.sql`,
search `-- name: ListMRReworkCandidates`). Two gates in that query block scheduled runs:

1. `AND r.kind = 'issue'` (mr_rework.sql, search `AND r.kind = 'issue'`) — the obvious
   one-line widening the draft proposes.
2. `AND r.mr_state = 'opened'` (mr_rework.sql, search `AND r.mr_state = 'opened'`) — the
   **load-bearing blocker.** `runs.mr_state` has exactly one SQL writer, `SetRunMRState`,
   mechanically enforced by `TestMRStateIsWatcherOwned`
   (`api/internal/store/mr_state_test.go`). It is driven only by `SyncMRStates` →
   `ListMRWatchCandidates` (`api/internal/store/queries/forge.sql`, search
   `-- name: ListMRWatchCandidates`), which:
   - `JOIN issues i ON i.repo_id = @repo_id AND i.forge_issue_iid = l.issue_iid`, and
   - `SELECT DISTINCT ON (r.issue_iid)`.

   Consequence, verified by reading both call sites:
   - **prompt runs are issue-less.** `CreatePromptRun`
     (`api/internal/workersvc/prompt.go`) inserts no `issue_iid`; the run carries
     `issue_iid IS NULL`. It never satisfies the issues JOIN, so `mr_state` stays NULL
     forever. Widening only the kind filter yields **zero** prompt candidates.
   - **self_improve runs share ONE tracking issue** across cycles (`SelfImproveTrackingLabel`
     = `uzi-self-improve`, `api/internal/schedsvc/self_improve.go`). `DISTINCT ON (issue_iid)`
     watches only the newest cycle's run, so mr_state is unreliable for the
     multi-MR-per-tracking-issue lane — the exact reason PRD #686 D12 abandoned
     `runs.mr_state` for the self_improve open-MR cap and reads open-state LIVE
     (`schedsvc/self_improve.go`, search `RecentSelfImproveMRRunsForRepo` and the
     `selfImproveMaxOpenMRs` cap loop).

The `mr_state` gate is not decorative. It also drives two behaviours the new lanes must
keep: **stop-on-close/merge cancellation** (`cancelReworkOnClosedMR` →
`CancelReworkForMR`, `api/internal/forgesvc/mr_watch.go` + `workersvc/mr_rework_cancel.go`)
and **ledger eviction** (`DeleteMRReworkLedgerNotIn`, mr_rework.sql), both keyed on an MR
leaving `'opened'` as recorded in `runs.mr_state`.

### Chosen way through: a board-free MR-state recorder (a hybrid that keeps A's payoff, avoids A's cost)

**Decision: reuse `runs.mr_state` as the rework gate signal (Option A's payoff — the
candidate query, ledger eviction, and stop-on-close all keep working), but populate it for
the scheduled lanes via a NEW, board-free recorder rather than by extending the
board-coupled `ListMRWatchCandidates` / `syncOneMRState` (avoiding Option A's blast radius
on board-move semantics and the `DISTINCT ON (issue_iid)` invariant).**

Concretely:
- A new store query `ListScheduledMRStateWatchCandidates` selects completed prompt /
  self_improve runs with an `mr_iid` whose `mr_state` is not yet terminal — keyed on the
  **run/branch**, with **no issues JOIN** and **no `DISTINCT ON (issue_iid)`**.
- A new forgesvc method `SyncScheduledMRStates` reads each candidate's live MR state and
  records it via the existing `SetRunMRState` (the sole SQL writer — invariant preserved),
  performing **no board move**, and calls the existing `cancelReworkOnClosedMR` on the
  opened→merged/closed edge.
- `ListMRReworkCandidates` widens only its **kind filter**; it keeps `mr_state = 'opened'`
  and `DISTINCT ON (r.branch)` untouched — which now works for all three kinds because the
  recorder makes `mr_state` reliable for the scheduled ones.
- Ledger eviction (`DeleteMRReworkLedgerNotIn`) and cancellation (`CancelReworkForMR`) are
  **unchanged** — they were already `mr_state`-driven.

The recorder is self-bounding exactly like `ListMRWatchCandidates` Lane B: it polls a run
only until its `mr_state` reaches a terminal value (`merged`/`closed`), then the run drops
out of its own candidate set. At steady state it polls only currently-open scheduled MRs.

### Why the recorder writes mr_state, and why that is not a second-writer violation

`TestMRStateIsWatcherOwned` scans `queries/runtime.sql` and `queries/forge.sql` and fails
if any query **other than `SetRunMRState`** writes `mr_state` (mr_state_test.go, search
`writesRuns`). The recorder issues **no new mr_state-writing SQL** — it calls the existing
`SetRunMRState` query. So the SQL-layer invariant is preserved verbatim. What changes is
that `mr_state` now has **two Go callers** in forgesvc (`SyncMRStates` for issue runs,
`SyncScheduledMRStates` for scheduled runs) over **disjoint run sets** (issue vs
prompt/self_improve). The doc comment on `SetRunMRState`
(`api/internal/store/queries/runtime.sql`, search `sole writer of runs.mr_state`) and on
`recordMRState` (mr_watch.go) must be updated to say "written only by forgesvc MR-state
sync, over disjoint run sets," not "the MR-close watcher." This is a deliberate, recorded
widening of the invariant's wording, not a break of its mechanism.

### Rejected alternatives

**Rejected — Option A (extend `ListMRWatchCandidates` to be lane-agnostic).** Adding a
third lane to the existing watch query forces `syncOneMRState` to handle candidates that
have no board card (prompt has no issue; the self_improve tracking issue is excluded from
board promotion, `handler.promotable`) and forces relaxing `DISTINCT ON (issue_iid)` for
self_improve's multi-MR-per-issue shape. That query and its state machine carry the PRD #24
board-move contract (Lane A/B, `guardedMRMove`, the close/reopen edge switch) and are
guarded by `TestMRStateIsWatcherOwned` and the mr_watch tests. Threading a card-less,
collapse-free lane through that switch risks the issue-run board behaviour for no benefit
the separate recorder does not also deliver. The recorder gets A's entire payoff
(mr_state as the signal) with none of A's blast radius.

**Rejected — Option B pure (relax the mr_state gate; live-read open-state in the
detector).** This is viable and has precedent (self_improve's D12 cap already live-reads),
but it forces reimplementing **both** load-bearing correctness pieces the dispatch flags:
a replacement stop-on-close cancellation (the detector would have to own it) and a
replacement ledger eviction, plus a **recency bound** to stop the candidate set from
growing without limit — under pure B a completed scheduled run with an `mr_iid` stays a
candidate forever (nothing ever moves it out of scope), so every historical scheduled MR
gets a live `GetMergeRequest` every tick. The recorder avoids all three: it reuses the
proven cancellation and eviction paths unchanged and is self-bounding via terminal-state
recording. The one thing pure B has that the recorder does not is zero interaction with
`ListMRWatchCandidates` (§5, Risk R1) — a benign, test-guarded interaction is a smaller
cost than reinventing two correctness paths that must not regress on the unattended lane.

---

## 2. File map

**Create**
- `api/internal/forgesvc/scheduled_mr_watch.go` — the board-free recorder
  `SyncScheduledMRStates(ctx, repoID, forgeProjectID, f)`: enumerate
  `ListScheduledMRStateWatchCandidates`, read each MR live, `recordMRState`, and
  `cancelReworkOnClosedMR` on the opened→terminal edge. (Kept in a new file rather than
  appended to `mr_watch.go` so the board-coupled watcher and the board-free recorder read
  as distinct components.)

**Modify — api (store / queries)**
- `api/internal/store/queries/forge.sql` — add `-- name: ListScheduledMRStateWatchCandidates :many`
  (runs-only, no issues JOIN, no `DISTINCT ON`, terminal-state self-eviction, `LIMIT` burst
  bound). Update the `ListMRWatchCandidates` doc comment to point at the new sibling.
- `api/internal/store/queries/runtime.sql` — reword the `SetRunMRState` doc comment
  ("written only by forgesvc MR-state sync, over disjoint run sets"). **No SQL change** —
  wording only, so `TestMRStateIsWatcherOwned` still passes.
- `api/internal/store/queries/mr_rework.sql` — `ListMRReworkCandidates`: change
  `AND r.kind = 'issue'` → `AND r.kind IN ('issue', 'prompt', 'self_improve')`; keep
  `mr_state = 'opened'` and `DISTINCT ON (r.branch)`. Reword the Decision-9 scope note and
  the `CreateAutoMRReworkRun` "pipeline_ref = agent/issue-N" comment (now any agent branch).
- `api/internal/store/queries/selfimprove.sql` — `CreateSelfImproveRun`: add
  `mr_rework_enabled = sqlc.narg('mr_rework_enabled')` to the INSERT column list.
- Regenerate: `cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate`
  (`api/internal/store/*.sql.go` consts + params move — confirm they did).

**Modify — api (services / poller)**
- `api/internal/poller/poller.go` — after the `SyncMRStates` call in `runPollOnce`
  (search `if err := e.svc.SyncMRStates`), add a `SyncScheduledMRStates` call, guarded the
  same way (runs on the same tick, log-and-continue on error).
- `api/internal/poller/mr_review_watch.go` — `detectOne`: stop returning early when
  `issueIIDFromBranch(ref)` fails; make the per-MR-cap halt **issue-comment conditional**
  (post `CreateIssueNote` only when the branch parses to an issue iid; otherwise inbox-only)
  and make `notifyHalt` tolerate a zero issue iid.
- `api/internal/workersvc/self_improve.go` — `CreateSelfImproveRun`: add an
  `mrReworkEnabled *bool` parameter and pass `pgBoolPtr(mrReworkEnabled)` into
  `CreateSelfImproveRunParams`.
- `api/internal/schedsvc/scheduler.go` — extend the `CreateSelfImproveRun` interface
  signature (search `CreateSelfImproveRun(ctx context.Context, userID, repoID`) with the
  new `*bool`.
- `api/internal/schedsvc/self_improve.go` — the fire site (search
  `e.runs.CreateSelfImproveRun`) passes `scheduleMrRework(sched)` (the existing helper,
  scheduler.go, search `func scheduleMrRework` / `s.MrReworkEnabled.Valid`), mirroring the
  prompt path (`scheduler.go`, search `e.runs.CreatePromptRun(... scheduleMrRework(sched)`).

**Modify — wiring / docs**
- Wherever `SetReworkCanceller` is wired onto the forgesvc `Service` (search
  `SetReworkCanceller`) — no change, the recorder reuses the same field. Verify the poller
  constructs forgesvc with the canceller already set (it does today for `SyncMRStates`).
- `docs/scheduling.md` — state that prompt and self_improve MRs now participate in MR
  rework, default-on, with the per-schedule mr-rework toggle as the opt-out; note
  ci_autofix parity is a separate follow-up.

**Entry point**: `poller.runPollOnce` — the new `SyncScheduledMRStates` call slots between
`SyncMRStates` and the existing `mrReviewWatch.detect`, so a scheduled run's `mr_state` is
fresh in the same tick the detector reads candidates.

---

## 3. Contracts

### New store query

```
-- name: ListScheduledMRStateWatchCandidates :many
-- Board-FREE MR-state watch for the scheduled lanes (prompt, self_improve). Sibling of
-- ListMRWatchCandidates but with NO issues JOIN and NO DISTINCT ON (issue_iid): prompt
-- runs are issue-less and self_improve runs share one tracking issue, so neither the
-- board-move machinery nor the per-issue collapse applies. Self-bounding like Lane B:
-- poll a run only until its mr_state is terminal, then it drops out.
SELECT id, branch, mr_iid, mr_state
FROM runs
WHERE repo_id = @repo_id::uuid
  AND kind IN ('prompt', 'self_improve')
  AND status = 'completed'
  AND mr_iid IS NOT NULL
  AND (mr_state IS NULL OR mr_state IN ('opened', 'locked'))
ORDER BY created_at DESC
LIMIT 100;
```

### Widened rework candidate gate (only the kind filter changes)

```
-- in ListMRReworkCandidates' per_branch CTE:
-      AND r.kind = 'issue'
+      AND r.kind IN ('issue', 'prompt', 'self_improve')
--   mr_state = 'opened' and DISTINCT ON (r.branch) are UNCHANGED.
```

### Component interaction

```mermaid
sequenceDiagram
    participant P as poller.runPollOnce
    participant W as forgesvc.SyncMRStates<br/>(issue runs — board-coupled, UNCHANGED)
    participant R as forgesvc.SyncScheduledMRStates<br/>(prompt/self_improve — NEW, board-free)
    participant F as forge driver
    participant S as store (runs.mr_state)
    participant D as poller.MRReviewWatch.detect
    P->>W: issue-run MR states (JOIN issues, board moves)
    W->>S: SetRunMRState (issue runs)
    P->>R: ListScheduledMRStateWatchCandidates(repo)
    R->>F: GetMergeRequest(mrIID) per candidate
    R->>S: SetRunMRState (scheduled runs, NO board move)
    R->>R: on opened→merged/closed: cancelReworkOnClosedMR → CancelReworkForMR
    P->>D: detect()
    D->>S: ListMRReworkCandidates (kind IN issue/prompt/self_improve, mr_state='opened')
    D->>F: ListMergeRequestComments; green-pipeline + review-landed + high-water + cap gates
    D->>D: CreateAutoMRReworkRun (unchanged); halt = issue-comment iff branch parses, else inbox-only
```

### Go signature deltas

```
// workersvc/self_improve.go
-func (s *Service) CreateSelfImproveRun(ctx, userID, repoID uuid.UUID, issueIID int64,
-    title, description string, model *string, overrideSubagentModel bool) (store.Run, error)
+func (s *Service) CreateSelfImproveRun(ctx, userID, repoID uuid.UUID, issueIID int64,
+    title, description string, mrReworkEnabled *bool, model *string, overrideSubagentModel bool) (store.Run, error)

// forgesvc — new method (mirrors SyncMRStates' shape and error contract)
func (s *Service) SyncScheduledMRStates(ctx context.Context, repoID uuid.UUID,
    forgeProjectID int64, f forge.Forge) error
```

The four-layer opt-out (admin `settings.mr_rework_enabled` → `users.mr_rework_enabled` →
`runs.mr_rework_enabled` → `run_schedules.mr_rework_enabled`) is **unchanged**. The
candidate query already resolves `COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled)
IS NOT FALSE`; prompt already stamps `runs.mr_rework_enabled` from the schedule
(`prompt.go`, search `MrReworkEnabled: pgBoolPtr`), and this design adds the same stamp to
self_improve. No migration: columns `users.mr_rework_enabled` (00165),
`runs.mr_rework_enabled` and `run_schedules.mr_rework_enabled` (00179) exist (verified in
the migration files) and `store.models.go` already carries the `MrReworkEnabled pgtype.Bool`
fields.

---

## 4. The demo/mirror contract (the recorder is a second implementation of MR-state sync)

`SyncScheduledMRStates` is a **second implementation** of "observe an MR's forge state and
persist it to `runs.mr_state`, cancelling an in-flight rework when it leaves opened." The
first is `syncOneMRState`. Per the duplication-is-a-contract rule, the two must be pinned
together where their behaviour must agree, and the fixture must discriminate — not merely
snapshot one of them.

The behaviours that MUST agree (assert in tests as a differential contract):
- **Unknown/empty forge state → no write** (mr_watch.go `forge.IsKnownMRState`). The
  recorder must apply the same guard.
- **opened→merged and opened→closed → `cancelReworkOnClosedMR` called exactly once**, and
  `locked` (transient) → **no cancel** (mr_watch.go `default` arm).
- **Terminal state, once recorded, stops polling** (recorder self-eviction) — the analogue
  of Lane B's decay.

The behaviours that MUST differ (and the test must prove the difference, not paper over it):
- **The recorder performs NO board move** (`guardedMRMove` is never called; there is no
  card). A test must assert a recorder tick over a self_improve run whose tracking issue is
  a real cached issue moves **no** card — this is exactly the R1 entanglement guard (§5).
- **The recorder has no bootstrap suppression** — first observation records the live state
  and (for `opened`) immediately opens the rework gate; `syncOneMRState` deliberately
  suppresses acting on a NULL→closed bootstrap to avoid a wave of board moves, which is
  moot with no card.

Fixture cases the differential test must contain (one per reimplemented behaviour, plus an
assertion that each is exercised): `{opened-first-observation}`, `{opened→merged}`,
`{opened→closed}`, `{opened→locked}`, `{unknown-state}`, `{already-terminal, not re-polled}`,
and `{self_improve run whose tracking issue is cached → no board move}`.

---

## 5. Risks

- **R1 (riskiest assumption) — a recorder-set terminal `mr_state` on a self_improve run
  makes it a benign `ListMRWatchCandidates` candidate.** `ListMRWatchCandidates` has no kind
  filter; its Lane A admits an **open** issue whose latest run has `mr_state = 'closed'`
  (forge.sql, search `l.mr_state = 'closed'`). The self_improve tracking issue is always
  open, so a self_improve run the recorder marks `'closed'` could be selected by the existing
  watcher. **Validated by reading the state machine: it is a no-op.** `syncOneMRState` reads
  the MR live, finds `observed == stored` (`closed == closed`) and returns before any move;
  the only path that reaches `guardedMRMove` is the reopen-edge, and `guardedMRMove` resolves
  the tracking issue's column via `board.ResolveColumn(labels=[uzi-self-improve], …)`, which
  is not `In Progress`, so it returns `moveSkipped` (no move). **Validate early** with a unit
  test in `forgesvc` (mirror `mr_watch_test.go`): a candidate row for a self_improve run with
  stored `mr_state='closed'` and a cached tracking issue produces zero `AutoMove` calls. If
  this ever regressed to a real move, the fallback is a kind exclusion on
  `ListMRWatchCandidates` — but that reopens Option A's blast radius, so the test is the
  cheaper guard. (Pure Option B avoids R1 entirely; it is the one thing B buys.)

- **R2 — mr_state now has two Go writers.** Disjoint run sets (issue vs prompt/self_improve)
  and both read live forge state, so they cannot corrupt each other. The SQL-level invariant
  (`TestMRStateIsWatcherOwned`) is untouched. Mitigation is documentation (reword the two doc
  comments) plus R1's no-move guard.

- **R3 — bootstrap burst on historical scheduled MRs.** On first deploy, every completed
  prompt/self_improve run with `mr_iid` and NULL `mr_state` gets one live `GetMergeRequest`,
  then settles (merged/closed → terminal → out of scope; open → polled while open). Bounded
  by `LIMIT 100` per repo per tick and by the number of historical scheduled MRs. A
  historical **open** scheduled MR with a pre-existing landed review comment will get a
  rework on the first eligible tick — intended (that is the dropped feedback the PRD exists
  to address), and bounded by the per-MR cap, the admin cap, and the review-landed
  quiet-period + head-SHA staleness gates in `detectOne`.

- **R4 — cost on an unattended lane.** Each rework cycle spends the owner's Anthropic token.
  Unchanged risk model from PRD #700; bounded by the default-on four-layer opt-out and the
  admin `mr_rework_cap`. This design adds no burn a user has not opted into.

- **R5 — no new migration, but confirm the goose head.** This design adds no migration. If
  an implementer believes one is needed, that is a signal the design was misread — escalate
  rather than add a column. (Numbers are assigned at merge time regardless.)

- **R6 — the `issueIIDFromBranch` decoupling must not change issue-run behaviour.** For an
  `agent/issue-N` branch the parse still succeeds and the issue comment still posts; only the
  `!ok` path changes from "skip the whole candidate" to "process it, inbox-only halt." A
  detector unit test must cover both an `agent/issue-N` (comment still posts) and a
  `uzi/prompt-…` (no comment, still inbox-notifies, still fires the rework) branch.

---

## 6. Handoff — milestones, files, acceptance

Dependency-ordered. **Offline** = unit-testable with in-memory fakes, no live forge. **LiveDB**
= needs `./e2e/run-store-it.sh`. **Live forge** = end-to-end validation against a real MR.

| M | Title | Files | Env |
|---|---|---|---|
| **M1** | Thread the per-schedule mr-rework toggle through self_improve | `workersvc/self_improve.go`, `store/queries/selfimprove.sql`, `schedsvc/scheduler.go` (iface), `schedsvc/self_improve.go`, `schedsvc/scheduler_test.go` (fake) | Offline |
| **M2** | Decouple the detector from `agent/issue-N` | `poller/mr_review_watch.go`, `poller/mr_review_watch_test.go` | Offline |
| **M3** | Board-free scheduled MR-state recorder | `store/queries/forge.sql` (+regen), `forgesvc/scheduled_mr_watch.go`, `poller/poller.go`, `store/queries/runtime.sql` (doc), `forgesvc` tests | Offline + LiveDB |
| **M4** | Widen `ListMRReworkCandidates` kind filter | `store/queries/mr_rework.sql` (+regen) | Offline build; LiveDB to validate |
| **M5** | Tests (integration) | `poller/*_test.go`, `forgesvc/*_livedb_test.go`, `store/*_livedb_test.go` | LiveDB + live forge |
| **M6** | Docs | `docs/scheduling.md` | Offline |

**Parallelism.** M1, M2, M3, M4, M6 touch disjoint files and can run as parallel workers.
M4's *validation* depends on M3 (the gate yields scheduled candidates only once the recorder
populates `mr_state`), but the query edit itself is independent. M5 is Phase 2 — it runs
after M1–M4 land. Suggested waves: **Wave 1** {M1, M2, M6} + {M3, M4 as a pair}; **Wave 2**
{M5}.

**Per-milestone acceptance (mechanically verifiable):**

- **M1** — `TestTick…SelfImprove…MrRework…`: a self_improve fire with `sched.mr_rework_enabled
  = false` inserts a run whose `mr_rework_enabled = false`; with `NULL` inserts NULL; with
  `true` inserts true. Mirror the existing prompt test (search
  `TestTickThreadsScheduleMrReworkEnabled`). `task gate:api` green.

- **M2** — detector unit test: a `uzi/prompt-<uuid>` candidate is **not** skipped (it reaches
  the gate chain); the per-MR-cap halt on an issueless branch posts **no** `CreateIssueNote`
  (fake forge records zero issue notes) but **does** emit one inbox notification; an
  `agent/issue-N` candidate **still** posts the issue comment. `task gate:api` green.

- **M3** — (a) LiveDB: `SyncScheduledMRStates` over a fake forge records `mr_state` for a
  prompt run and a self_improve run and moves **no** board card; (b) LiveDB: an opened→merged
  transition calls `CancelReworkForMR` once, `locked` calls it zero times; (c) LiveDB: a run
  already at terminal `mr_state` is not returned by `ListScheduledMRStateWatchCandidates`;
  (d) R1 guard (offline `forgesvc` unit): a self_improve run with stored `mr_state='closed'`
  and a cached tracking issue produces zero `AutoMove` calls through `SyncMRStates`.
  `sqlc generate` is a no-op after regen (CI `validate:api`). `task gate:api` green,
  `*LiveDB` sweep green via `./e2e/run-store-it.sh` (positive control: named tests show
  `--- PASS`, `RUN > 0`, zero `--- SKIP`).

- **M4** — LiveDB: `ListMRReworkCandidates` returns a `uzi/prompt-…` branch and a
  `uzi/self-improve/…` branch when their run has `mr_state='opened'` and no opt-out; an
  explicit `false` at the run, user, or schedule layer excludes the branch; NULL everywhere
  keeps it in; a token-less owner excludes it. Confirm the generated const in
  `api/internal/store/mr_rework.sql.go` moved.

- **M5** — the differential/mirror contract of §4 passes with the seven fixture cases, and an
  assertion proves each case is exercised. Cross-kind branch guard
  (`uq_runs_one_active_branch_ref`) and per-MR uniqueness (`uq_runs_one_active_mr_rework`)
  still hold for a `uzi/prompt-…` / `uzi/self-improve/…` branch (LiveDB). One live-forge
  end-to-end: a real prompt MR and a real self_improve MR each receive a landed review
  comment on a green, still-open MR and get an automatic follow-up rework run; setting the
  schedule toggle Off suppresses it.

- **M6** — `docs/scheduling.md` states the new participation + opt-out; `npm run build`'s
  `check-docs.mjs` passes (frontmatter/links).

**Global success criteria (from the draft, unchanged):** a prompt or self_improve MR with a
landed review comment on a green, still-open MR gets an automatic `mr_rework` follow-up
(subject to the four-layer opt-out); the schedule toggle Off suppresses it end-to-end for
both lanes; the per-MR cap halt on an issueless branch inbox-notifies without posting to a
nonexistent issue and without crashing; full gate green plus the `*LiveDB` sweep.

---

## 7. Sequencing — prompt and self_improve ship together

**Recommendation: ship prompt and self_improve together, in one PRD.**

The draft's prompt-first rationale was "self_improve has the #887 dependency, prompt does
not." §0 refutes that: **#887 is merged**, so the dependency is discharged and no longer
gates self_improve. With it gone, the two lanes share one mechanism end to end — the recorder
query is `kind IN ('prompt','self_improve')`, the detector decoupling (M2) serves both, and
the candidate widening (M4) is a single line covering both. The only self_improve-specific
work is M1 (thread the toggle) and the R1 guard test — both small and independent. Splitting
would mean two passes over the same `forge.sql` / `mr_rework.sql` / `mr_review_watch.go`
files for no risk reduction.

If the team still wants to de-risk the self_improve lane specifically, the clean seam is: land
M2 + M3 + M4 + M6 (which make **both** lanes work) plus M1, and treat the self_improve
*live-forge* validation as a fast-follow behind a known-good self_improve MR — the code path
is identical to prompt, so there is little residual risk to isolate. There is no code reason
to withhold self_improve.

---

## 8. Open questions (for the lead → user)

1. **Halt-comment channel for issueless runs.** The design keeps draft D1: on the per-MR cap
   halt for a prompt/self_improve branch, emit the inbox notification only and post no forge
   comment (there is no natural issue to post to; the self_improve tracking issue is a shared
   container and a per-MR halt there is noise). The alternative — adding a
   `CreateMergeRequestNote` method to post the halt on the MR itself — is a cross-driver Forge
   interface change (gitlab/forgejo/github + six test fakes) for a rarely-hit edge, and is
   deliberately deferred. **Confirm inbox-only is acceptable**, or the MR-note method becomes
   its own scoped issue.

2. **ci_autofix parity is explicitly out of scope.** `ListCIAutofixCandidateRefs`
   (`api/internal/store/queries/ci_autofix.sql`, search `AND r.kind IN ('issue', 'ci_fix')`)
   has the identical `kind`-and-`issueIIDFromBranch` gating, so scheduled-run MRs also get no
   CI-autofix today. Note that ci_autofix has **no** `mr_state` gate (it is pipeline-driven),
   so bringing it to scheduled runs would **not** need the recorder — it is a smaller, separate
   change. This design does not touch it. Confirm that stays a follow-up issue.

3. **Recorder `LIMIT` burst bound = 100** (mirrors `ListMRWatchCandidates`' hardcoded 100).
   Confirm that is a fine per-repo-per-tick ceiling for scheduled MRs, or set a smaller value.

No other assumptions are silently guessed. Everything else is grounded in the files cited
above at `HEAD = 32e7af7`.
