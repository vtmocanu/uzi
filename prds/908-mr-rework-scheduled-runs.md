# PRD #908: Autofix (MR rework + CI autofix) for scheduled runs

**Issue**: #908
**Status**: Draft — ready for implementation
**Priority**: Medium
**Design companion**: `prds/908-mr-rework-scheduled-runs-design.md` (the board-free recorder decision, mechanism, contracts, and risks — read it for the MR-rework half's deep design).
**Scope**: `api/` + `docs/` only. No `agent/` change, no `web/` code change, **no new migration**.

## Problem

uzi has two autonomous, poller-driven "autofix" lanes that act on an open agent MR:

- **MR rework** (`kind='mr_rework'`, PRD #700): on a new landed review comment (e.g. CodeRabbit
  `CHANGES_REQUESTED`), it folds fixes onto the branch. Admin-enabled by default.
- **CI autofix** (`kind='ci_fix'`): on a failed head pipeline, it pushes a fix. User opt-in,
  default-off.

**Both lanes fire only on `issue`-kind runs.** Scheduled runs open MRs of kind `prompt` (the
docs-hygiene / bug-hunt / test-improvement / feature-bingo schedules, branches
`uzi/prompt-<runId>`) and `self_improve` (branches `uzi/self-improve/<runId>`). So a review
comment or a red pipeline on a scheduled-run MR is never acted on — a human reviews and merges
it without the feedback applied. Observed live 2026-08-31 → 09-01: PRs #906 (prompt) and #907
(self_improve) both got `CHANGES_REQUESTED` with no follow-up, while issue-run PRs #902/#903 were
reworked automatically.

Three code facts make this larger than a one-line filter change (all verified at
`HEAD = 32e7af7`):

1. **The MR-rework candidate query gates on `r.mr_state = 'opened'`, not just kind.**
   `runs.mr_state` has one writer (`SetRunMRState`), driven only by the issue-centric
   `ListMRWatchCandidates` (`api/internal/store/queries/forge.sql`), which `JOIN issues ON
   i.forge_issue_iid = l.issue_iid` and `DISTINCT ON (r.issue_iid)`. **prompt runs are
   issue-less** (`issue_iid IS NULL`) → never watched → `mr_state` stays NULL → widening the kind
   filter alone yields **zero** prompt candidates. **self_improve** shares one tracking issue
   across cycles, and that lane already declares `runs.mr_state` unreliable (PRD #686 D12),
   reading open-state live from the forge. So MR rework needs a reliable open-MR signal for these
   lanes.

2. **Both detectors are hard-wired to the `agent/issue-N` branch convention.**
   `issueIIDFromBranch` (`api/internal/poller/ci_autofix.go:337`) only accepts `agent/issue-<N>`.
   `mr_review_watch.go:162` skips a branch that does not parse; `ci_autofix.go:120` does the same.
   The parsed issue IID is used only to post issue comments (mr_rework: cap-halt `:250`;
   ci_autofix: start `:268` + halt `:224`) — none of which exist for an issueless scheduled run.

3. **The self_improve create path never stamps the per-schedule `mr_rework_enabled` toggle.**
   The prompt path does (`workersvc/prompt.go:61`); `CreateSelfImproveRun`
   (`workersvc/self_improve.go:30`, `selfimprove.sql`) has no such column, so a self_improve run
   would honor only the per-user default, ignoring its schedule's own toggle.

**CI autofix, by contrast, needs no open-MR recorder.** Its candidate query
`ListCIAutofixCandidateRefs` (`ci_autofix.sql`) is pipeline-driven — it has **no `mr_state`
gate**, keying off `pipeline_statuses.ref` with a failed pipeline. And `ListWatchedRunRefsForRepo`
(the pipeline-status ref watch) has **no kind filter and no issues JOIN**, so scheduled branches
already get `pipeline_statuses` rows. So CI-autofix parity is just: widen its kind filter +
decouple its detector (fact 2). This is why the two lanes share M2 and M4 but only MR rework
needs M3.

## Solution

Bring `prompt` and `self_improve` runs into **both** autofix lanes, each keeping its existing
enablement model:

- **MR rework** stays default-on with its four-layer opt-out (admin `settings.mr_rework_enabled`
  → `users.mr_rework_enabled` → `runs.mr_rework_enabled` → `run_schedules.mr_rework_enabled`, all
  default-ON, any explicit `false` disables). The prompt path already stamps the schedule toggle;
  M1 adds the same stamp to self_improve.
- **CI autofix** stays **user opt-in, default-off** (`users.ci_autofix_enabled`, migration 00115,
  `DEFAULT false`). No per-run/per-schedule toggle exists for it and this PRD adds none — a
  scheduled run inherits the owner's user-level opt-in. This satisfies "don't pay unless you
  enable it" more strictly than mr_rework (you must opt in at all). A per-schedule ci_autofix
  toggle is a possible follow-up (it would need a new column + web work), deliberately out of
  scope here.

**The MR-rework open-MR signal (fact 1) is solved with a board-free MR-state recorder** (design
companion §1): reuse `runs.mr_state` as the gate signal (so the candidate query, ledger eviction,
and stop-on-close cancellation all keep working), but populate it for the scheduled lanes via a
new, board-free `forgesvc.SyncScheduledMRStates` + a runs-only query (no issues JOIN, no
`DISTINCT ON`), instead of extending the board-coupled `ListMRWatchCandidates`. The candidate
query then widens by one line and keeps its `mr_state = 'opened'` gate. Rejected alternatives
(pure Option A / pure Option B) and the "second Go writer of mr_state over disjoint run sets"
invariant reasoning are in the companion §1.

**No new migration** — every column already exists (`users.mr_rework_enabled` 00165;
`runs`/`run_schedules.mr_rework_enabled` 00179; `users.ci_autofix_enabled` 00115). Live head is
00181.

### Resolved decisions

- **D1 — Halt/start comments for issueless runs: inbox-only, no new Forge method.** The Forge
  interface has only `CreateIssueNote` (`forge.go:506`); there is no MR-note write, and adding one
  touches all three drivers + six fakes. For a branch that parses to an issue IID, keep the
  current issue comments; for an issueless (prompt/self_improve) branch, **skip the issue
  comment** and rely on the existing inbox notification (mr_rework cap-halt already has
  `notifyHalt`; ci_autofix already has `notifyHalt`). The ci_autofix informational *start* comment
  has no backing issue for these lanes and is simply skipped. (Confirmed with the user.)
- **D2 — #887 is discharged.** The self_improve rework runtime shares the `fetchAgentBranch` path
  whose #887 D/F-conflict fix (`59cca21`, PR #902) is **merged into `origin/main`** (verified:
  `git merge-base --is-ancestor 59cca21 origin/main` = true; the `slash-free` comment is gone from
  main). So prompt and self_improve ship together; no sequencing gate needed.
- **D3 — Agent side unchanged.** An mr_rework/ci_fix run folds onto any branch via
  `claim.branch` / `runnerCloneForBranch`; the review-comment snapshot is keyed on `mrIID`, not
  branch or kind (`review_comments.go:148`). No `agent/` change.
- **D4 — Concurrency guards hold for the new branches.** `uq_runs_one_active_self_improve` is
  scoped to `kind='self_improve'` (an mr_rework/ci_fix run is a different kind); the cross-kind
  branch guard `uq_runs_one_active_branch_ref` spans `('ci_fix','mr_rework')` and the per-MR
  `uq_runs_one_active_mr_rework` both apply unchanged. No schema change.
- **D5 — Recorder burst bound `LIMIT 100`** per repo per tick, mirroring `ListMRWatchCandidates`.

## Milestones

Dependency-ordered. **Offline** = unit-testable with in-memory fakes. **LiveDB** = needs
`./e2e/run-store-it.sh`. **Live forge** = end-to-end against a real MR (post-merge validation).

- [ ] **M1 — Thread the per-schedule mr-rework toggle through self_improve.** *(Offline; mr_rework)*
  Add an `mrReworkEnabled *bool` param to `CreateSelfImproveRun` (`workersvc/self_improve.go`),
  add `mr_rework_enabled = sqlc.narg('mr_rework_enabled')` to the `selfimprove.sql` INSERT, extend
  the `schedsvc` interface signature (`scheduler.go:126`) and its test fake
  (`scheduler_test.go:343`), and pass `scheduleMrRework(sched)` at the fire site
  (`schedsvc/self_improve.go:188`) — mirroring the prompt path. Regenerate sqlc.

- [ ] **M2 — Decouple both detectors from `agent/issue-N`.** *(Offline; both lanes)*
  In `poller/mr_review_watch.go` and `poller/ci_autofix.go`, stop returning early when
  `issueIIDFromBranch(ref)` fails. Make every issue-comment post conditional on the branch
  parsing to an issue IID (mr_rework cap-halt `:250`; ci_autofix start `:268` + halt `:224`);
  otherwise skip the comment and keep the inbox notification (`notifyHalt` tolerating a zero IID).
  Issue-run behavior is unchanged (the `agent/issue-N` parse still succeeds and still comments).

- [ ] **M3 — Board-free scheduled MR-state recorder.** *(Offline + LiveDB; mr_rework)*
  Add `ListScheduledMRStateWatchCandidates` (runs-only, `kind IN ('prompt','self_improve')`,
  `status='completed'`, `mr_iid` set, `mr_state` non-terminal, no issues JOIN, no `DISTINCT ON`,
  `LIMIT 100`) to `forge.sql`. Add `forgesvc.SyncScheduledMRStates(ctx, repoID, forgeProjectID,
  f)` in a new `forgesvc/scheduled_mr_watch.go`: read each candidate's live MR state, record via
  the existing `SetRunMRState` (no board move), and call the existing `cancelReworkOnClosedMR` on
  the opened→terminal edge. Call it in `poller.runPollOnce` right after `SyncMRStates`. Reword the
  `SetRunMRState` and `recordMRState` doc comments to "written by forgesvc MR-state sync over
  disjoint run sets" (wording only — `TestMRStateIsWatcherOwned` still passes). See companion §3–§4
  for the exact contract and the differential-test requirement.

- [ ] **M4 — Widen both candidate-query kind filters.** *(Offline build; LiveDB to validate)*
  `ListMRReworkCandidates` (`mr_rework.sql`): `r.kind = 'issue'` → `r.kind IN
  ('issue','prompt','self_improve')`, keeping `mr_state='opened'` and `DISTINCT ON (r.branch)`.
  `ListCIAutofixCandidateRefs` (`ci_autofix.sql`): `r.kind IN ('issue','ci_fix')` → add
  `'prompt','self_improve'`, keeping the failed-pipeline JOIN, the default-branch/`mr_iid` guard,
  and the `ci_autofix_enabled`+token gate. Reword both Decision-9/kind-awareness doc comments and
  the `CreateAutoMRReworkRun` "pipeline_ref = agent/issue-N" comment (now any agent branch).
  Regenerate sqlc and confirm the generated consts moved.

- [ ] **M5 — Tests.** *(LiveDB + live forge)*
  Recorder differential/mirror contract vs `syncOneMRState` (companion §4: seven fixture cases,
  each proven exercised), incl. the R1 no-board-move guard (a self_improve run the recorder marks
  `closed`, with a cached tracking issue, produces zero `AutoMove`). Detector decoupling for both
  lanes (issueless branch not skipped; no issue comment; inbox still fires; `agent/issue-N` still
  comments). Both candidate queries yield `uzi/prompt-…` and `uzi/self-improve/…` branches under
  the right gates, and honor each lane's opt-out (mr_rework: explicit `false` at run/user/schedule
  excludes; ci_autofix: `ci_autofix_enabled=false` or no token excludes). Cross-kind branch guard +
  per-MR uniqueness hold for the new branches. One live-forge end-to-end per lane (a landed review
  comment → rework; a red pipeline → ci_fix), and the mr_rework schedule toggle Off suppressing it.

- [ ] **M6 — Docs.** *(Offline)*
  Update `docs/scheduling.md`: prompt and self_improve MRs now participate in both autofix lanes;
  mr_rework default-on with the four-layer opt-out (per-schedule toggle in the web ScheduleModal +
  CLI `--mr-rework`), ci_autofix user opt-in default-off. `npm run build`'s `check-docs.mjs` passes.

**Parallelism** (per user CLAUDE.md PRD-parallelization convention): M1, M2, M6 touch disjoint
files → parallel. M3 and M4 are a pair (M4's *validation* needs M3's recorder to populate
`mr_state`, though the M4 query edits are independent). **Wave 1**: {M1, M2, M6} + {M3, M4};
**Wave 2**: {M5}.

## Success criteria

1. A `prompt` or `self_improve` MR with a landed review comment on a green, still-open MR gets an
   automatic `mr_rework` run (subject to the four-layer opt-out) — the prompt half proven end to
   end, which is the criterion that would have caught the `mr_state` blocker.
2. A `prompt` or `self_improve` MR with a failed head pipeline gets an automatic `ci_fix` run when
   the owner has `ci_autofix_enabled` (and none when they have not).
3. Setting a schedule's mr-rework toggle Off (web or `--mr-rework=false`) suppresses rework on that
   schedule's runs — verified for a prompt schedule and the self_improve schedule.
4. The per-MR cap halt (mr_rework) and the start/halt notices (ci_autofix) on an issueless branch
   produce an inbox notification and post to no nonexistent issue, without crashing.
5. Full gate green (`task gate:api`, `task gate:agent` unaffected, docs check) plus the `*LiveDB`
   sweep with a positive control (named tests `--- PASS`, `RUN > 0`, zero `--- SKIP`).

## Risks & dependencies

- **R1 (recorder ↔ existing watcher entanglement)** — a recorder-set terminal `mr_state` on a
  self_improve run could make it a benign candidate of `ListMRWatchCandidates` (its tracking issue
  is always open). Read as a no-op (state machine returns before any board move); guarded by a
  required unit test (companion §5 R1). Fallback if it ever regressed: a kind exclusion on
  `ListMRWatchCandidates`.
- **R2 (`mr_state` two Go writers)** — `SyncMRStates` (issue runs) and `SyncScheduledMRStates`
  (scheduled runs) over disjoint run sets; the SQL invariant `TestMRStateIsWatcherOwned` is
  untouched (both call the same `SetRunMRState`). Mitigation is the doc-comment reword + R1's guard.
- **R3 (bootstrap burst)** — on first deploy, historical scheduled MRs each get one live
  `GetMergeRequest`, then settle; bounded by `LIMIT 100` and the per-MR/admin caps. A historical
  open scheduled MR with a pre-existing landed review comment will get one rework — intended (that
  is the dropped feedback this PRD addresses).
- **R4 (cost on unattended lanes)** — each cycle spends the owner's Anthropic token; bounded by
  mr_rework's default-on four-layer opt-out + admin cap, and by ci_autofix being opt-in default-off.
- **R5 (no migration)** — if an implementer thinks a migration is needed, the design was misread;
  escalate rather than add a column.

## Workflow-scope note (uzi worker)

Per `.claude/rules/prds.md`: implementation and validation touch **no** `.github/workflows/**` —
changes are in `api/` and `docs/`, and the LiveDB sweep runs via `./e2e/run-store-it.sh` (a
script, not a workflow file). Live-forge validation is post-merge, outside the branch diff. So
`git diff --name-only <base>..HEAD` shows zero `.github/workflows/**` entries — safe for the
worker PAT that lacks `workflow` scope.
