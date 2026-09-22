# Withdrawn work — a live run keeps spending after its issue is closed, de-labelled or deleted, and nothing says so

Line references are against `a10da71b`.

## Rationale

**uzi's own board closes an issue and deliberately leaves the run running.** The
board close handler states it as an intended non-bug:

```go
// api/internal/handler/board.go:871-872
// Two intended (not-bug) behaviours from the PRD: closing a card whose run is still in
// flight does NOT stop the run or its MR — ResolveColumn forces closed=true regardless
```

The PRD recorded the same as "define, don't block" — the run and the MR path continue
against a now-closed issue:

> "**Close with a run in flight (define, don't block)**: dragging an actively-worked
> card to Closed / does **not** stop the run or the MR path — the run keeps running and
> an MR may open against a / now-closed issue." … "State it as intended so it / is not
> "fixed" as a bug." (`prds/done/1034-close-reopen-from-board.md:196-200`)

The user doc describes the gesture but not its consequence for the run:

> "Dragging a card into **Closed** closes the issue on the forge instead of / changing a
> label — its column labels are left untouched, so it's the state, / not the labels,
> that puts it there." (`docs/board.md:71-73`)

**Eligibility is evaluated ONCE, at create time, from the cached issue row, and never
again.** `createRun` (`api/internal/workersvc/service.go:5197`) reads the cache
(`issue, err := s.q.GetIssueByIID(...)`, `service.go:5289`) and gates:

```go
// api/internal/workersvc/service.go:5309-5311
if !isEligibleIssue(issue.Labels, []string{s.uziLabel(ctx)}) &&
    !isAssignedToBot(issue.AssigneeIds, row.BotForgeUserID) {
    return store.Run{}, ErrNotPRDIssue
```

Those two helpers (defined `service.go:5528`, `:5546`) have no other non-test caller,
and the handler comment calls this "the single run-eligibility gate … the only
eligibility / refusal" (`api/internal/handler/runs_lifecycle.go:365-369`). Candidate
selection re-tests eligibility in SQL instead of re-reading this gate
(`autopilot.sql:42-46`; `schedules.sql:294`, `:314`), never for a run already created.
No query joins a NON-TERMINAL run back to the `issues` cache: `git grep -n -F 'i.state'
-- api/internal/store/queries/` returns 10 hits in exactly three files
(`finding_issue_close.sql:22,39`; `forge.sql:584,619,623,628,637,720`;
`judge_issue_close.sql:25,65`), and forge.sql's candidate set is gated `WHERE l.status =
'completed'` (`forge.sql:614`), i.e. terminal runs only.

**The poller already computes open→closed issue edges on the fresh cache, with ZERO
forge calls, twice per tick** — for judge recommendations
(`SyncFiledIssueCloses(ctx, r.ID)`, `api/internal/poller/poller.go:421`) and filed
findings (`SyncFindingIssueCloses(ctx, r.ID)`, `:430`) — with the rationale spelled out:

```go
// api/internal/poller/poller.go:418-420
// cache the sync above just wrote and makes NO forge call, so it costs nothing on the
// wire and, like the MR watcher, is skipped entirely by the early returns when the
// forge is unreachable (a stale cache must not manufacture edges).
```

Never for a live run.

**The "forge observation stops a live run" mechanism exists for exactly one kind.**
`CancelReworkForMR` (`api/internal/workersvc/mr_rework_cancel.go:24`):

```go
// api/internal/workersvc/mr_rework_cancel.go:15-18
// CancelReworkForMR aborts the active mr_rework run for (repoID, mrIID), if any,
// when its MR has left the opened state (merged/closed) — issue #853. It reuses the
// operator-cancel decision (submitInput's cancel branch, service.go) so a LIVE worker
// is actually stopped: a raw status flip would enqueue nothing into run_user_inputs and
```

Nothing equivalent watches an `issue` run's own issue.

**The gap is recorded as deliberate ONLY for the autopilot label — never for the `uzi`
eligibility label, a close, or a deletion.** `docs/autopilot.md:120-124` ("## Mid-run
label changes" … "Removing the autopilot label while a run is active changes nothing —
the / run keeps going.") and `prds/done/19-admin-settings-and-autopilot.md:31`
("Removing the label mid-run does not cancel, and re-adding it while a run is active is
swallowed (documented) — no queued re-runs in v1"). Two other decisions note eviction
as accepted but scope it to recording, not to the run's spend:
`prds/done/4-agent-runtime-workers.md:96` ("PRD #2's reconcile can evict the `issues`
cache row (de-label, close) between queue and claim, and the run must still be
buildable") and `prds/done/527-mr-merged-state-recording.md:149` (R3: "A closed *and*
de-labelled issue can be evicted from the cache, so its run's JOIN fails and it never
records `merged`. Accepted (rare)"). All adjacent; none covers a live run's spend after
withdrawal.

## Sketch

### M1 — make the withdrawal legible. Zero forge calls, zero token spend, no behaviour change to #1034's decision

- **A same-tick pass `SyncWithdrawnIssueRuns`**, beside `SyncFindingIssueCloses`
  (`poller.go:430`), on the cache the issue sync just wrote and skipped by the same
  forge-unreachable early returns. It costs nothing on the wire, exactly as the two
  close-edge passes above.
- **One query modelled on `ListFindingIssueCloseEdges`** (`-- name:` at
  `finding_issue_close.sql:6`; comment `:7-27`, statement `:28-40`): keep its join-key warning (join on
  `(repo_id, forge_issue_iid)`, never iid alone, `:10-13`), its EDGE rationale (`:20-21`)
  and its reopen-not-handled note (`:22-23`). Working set: `kind = 'issue'` runs with
  `status NOT IN ('completed','failed','cancelled')` and a new `runs.issue_withdrawn_at
  IS NULL` edge marker — the `close_synced_at` idiom (`00081_judge_issue_close_sync.sql:20-26`,
  `00209_finding_issue_close_sync.sql:22-27`) so the pass fires EXACTLY ONCE per
  withdrawal and a reopen/re-label cannot ping-pong it.
- **It MUST be a LEFT JOIN to `issues`** (this was a blocking review finding on the
  plan; state it as a design point, not an implementation detail). `FullSync` builds a
  keep-set from exactly three fetches — uzi-labelled (state=all), every open issue, and
  finding-labelled (state=all) — then evicts everything else
  (`api/internal/forgesvc/service.go:542-601`, `DeleteIssuesNotIn` at `:601`;
  `forge.sql:670-671`, comment "Reconcile eviction: drop cached issues absent from the
  fresh forge set."). So an issue that is both closed AND de-labelled — the strongest
  withdrawal — vanishes from the cache, and an INNER join would never see it (PRD #527's
  accepted R3 hole). The row existed at create time (`service.go:5289`), so its later
  absence *is* the signal.
- **Three reasons in one column `issue_withdrawn_reason`:** `evicted` (row absent from
  the LEFT JOIN), `closed` (`i.state = 'closed'`), `ineligible` (uzi label absent AND not
  bot-assigned). Express bot assignment with the NUMERIC-containment form
  `assignee_ids @> to_jsonb(@bot_id::bigint)` exactly as `autopilot.sql:44-45` does, never
  `jsonb_exists` on ids — the PRD #767 R3 trap recorded at `autopilot.sql:28-30`
  ("assignee_ids holds NUMERIC ids, so the / string-membership form the label predicates
  use (jsonb_exists) would never match a / number"); column from
  `00175_issue_assignee_ids.sql:9` (`ALTER TABLE issues ADD COLUMN assignee_ids jsonb NOT
  NULL DEFAULT '[]';`). **The `ineligible` branch must fail CLOSED:** the negated
  predicate fails OPEN on an unresolved bot id (`bot_id = 0` makes the assignment half
  false, so `NOT(...)` is true for every de-labelled-but-assigned run), so skip the
  `ineligible` branch entirely when the bot id is unresolved — the `@bot_id > 0` guard the
  eligibility SQL already mirrors.
- **Migration:** additive `runs.issue_withdrawn_at timestamptz` and
  `runs.issue_withdrawn_reason text`, plus the partial index the marker idiom always pairs
  with (`00081_judge_issue_close_sync.sql:33-35`) over the pass's working set. The number
  is a DRAFT: goose numbers are assigned at the landing merge and a version below the
  applied head bricks upgraded instances; live head at write time is
  `00243_validate_run_wall_park.sql`.
- **Notification:** one new kind `run_issue_withdrawn`, INBOX-ONLY like `mr_rework_halted`
  (`api/internal/poller/mr_review_watch.go:316-325`, doc `:306-310`: "the web's
  notificationLink turns any kind carrying a run_id into a /runs/<id> link. Slack is nil
  (inbox-only)"). Declare its own payload type beside `CIAutofixPayload`
  (`api/internal/notifysvc/service.go:113`; siblings `:201`, `:291`) carrying soft
  `title`/`body` so the inbox needs NO web change: `notificationTitle`/`notificationBody`
  read `payload.title`/`payload.body` else humanize the kind
  (`web/src/lib/notifications.ts:25-38`), and `notificationLink` falls through to
  `` `/runs/${runID}` `` (`:77-85`) — respecting the standing kind-conditional warning at
  `:55-66`. A Slack DM is optional (M1.5): if added, the untrusted issue title goes in
  Body (whole-blob rendered by `SlackMrkdwn`,
  `api/internal/slacksvc/notifier_notify.go:64-68`) and NEVER in Facts, which "are
  caller-built TRUSTED short strings … the notifier scrubs them but does NOT
  mrkdwn-escape them" (`api/internal/notifysvc/service.go:94-97`).
- **Self-close (a hidden step in M1's sizing).** uzi's own board close writes
  `issues.state='closed'` immediately (`api/internal/handler/board.go:941`, and
  `updated, err = h.svc.CloseIssue(...)` at `:951`), so the pass would notify an owner
  about a drag they made themselves. Decision: the board close path stamps the edge
  itself (reason `closed`) and SKIPS the notice when the actor is the run's owner; a close
  by anyone else, or directly on the forge, still notifies.
- **Surfaces:** a badge on the run page (`web/src/pages/RunView.tsx`) and the board card
  ("issue closed while this run was working" / "issue no longer eligible" / "issue
  removed"), the field on `uzi run get` (`api/cmd/uzi/run_render.go`), and a
  DISCRIMINATING mock fixture in `web/src/mocks/mockApi/runs.ts` — one case per branch
  (closed / de-labelled / evicted / not withdrawn / self-closed-on-board) plus an
  assertion the fixture contains every case (the prior ideas' "mock parity is a contract"
  point: the mock is a second implementation of "is this run withdrawn", so it needs a
  case per reimplemented branch, not a snapshot of demo data). Docs: the close paragraph
  in `docs/board.md:71-73` gains the consequence and the new signal.

### M2 — opt-in auto-pause, cuttable, default OFF

State explicitly (a blocking review finding on the plan's first draft) that
`CancelReworkForMR` is NOT the shape to copy: both its branches CANCEL
(`mr_rework_cancel.go:45` `Kind: "cancel",` and the server-side flip
`CancelRunServerSide` at `:53`), and every fenced path to `paused` is worker- or
owner-scoped — `SetRunPaused` (`api/internal/store/queries/runtime.sql:2288`; its `WHERE
id = @id AND worker_id = @worker_id AND status = 'running' AND pause_requested_at IS NOT
NULL … AND pause_mode IN ('milestone', 'now')`, `:2328-2335`, and its `SET` list writes no
`hold_reason`). The only server-side SWEEPER park is
`ParkRunsAtWall` (`runtime.sql:3645`, PRD #1497 M1) with claim-fence / custody /
codex-cap bookkeeping this idea does not want to clone. So **M2 parks NOTHING
server-side**: for a `running` run it requests a pause exactly as the owner would —
`api/internal/workersvc/submit.go:232` (`if kind == "pause" {`) then `:240`
(`CreatePauseInput`), whose comment (`:225-228`) writes the pending-pause columns + audit
row in one statement and LEAVES the run running; the worker parks at its boundary. Mode
`milestone` by default, `now` selectable (`docs/run-pause.md:19-23`, `:24-26`;
`run_user_inputs.kind` enum at `00219_run_budget_extension.sql:34`; the HTTP allowlist at
`api/internal/handler/runs_lifecycle.go:32`; resume is `/resume-now`). Any other
non-terminal status (queued, awaiting_approval, pool_wait, paused) spends nothing, so the
M1 notice is the whole action. No `hold_reason` value is added (the M1 marker is the
record; `00215_completion_hold.sql:13-15` keeps `hold_reason` unconstrained, but M2 does
not need it). Gate: a three-state admin key beside `KeyMrReworkEnabled`
(`api/internal/settings/keys.go:261`) and `KeyCiAutofixEnabled` (`:268`), read
error-propagating and mapped to OFF by the caller like `CiAutofixEnabled`
(`api/internal/settings/settings_ci_autofix.go:22-23`, rationale `:14-21`). Cancel was
discussed and rejected as the default: it is irreversible.

## Where it lives / what it touches

`api/internal/store/migrations/` (one additive migration);
`api/internal/store/queries/issue_withdrawn.sql` (list edges, apply edge);
`api/internal/forgesvc/` or `api/internal/workersvc/` (the pass) +
`api/internal/poller/poller.go` (one call beside `:430`);
`api/internal/handler/board.go` (self-close stamp); `api/internal/notifysvc/service.go`
(payload + kind); `api/internal/handler/runs_dto.go`, `api/internal/apitypes/run.go`,
`web/src/lib/api.ts`, `web/src/pages/RunView.tsx`, the board card,
`web/src/mocks/mockApi/runs.ts`; `api/cmd/uzi/run_render.go`, `api/internal/uzicli/`;
`docs/board.md`, `CHANGELOG.md` at implementation time; M2:
`api/internal/settings/keys.go` + accessor + `web/src/pages/AdminSettings.tsx`,
`docs/run-pause.md`. NOT touched: `agent/src/**` (no worker-protocol change), the forge
drivers (no new API call), `main`.

## Caveats / scoping notes

- **Recorded decision, scoped honestly.** #1034's "define, don't block"
  (`prds/done/1034-close-reopen-from-board.md:196-200`) covers the board-drag gesture
  only; it says nothing about a close performed on the forge, de-labelling, or deletion,
  and it was never promoted into `specs/human.md`. M1 changes no behaviour; M2 is off by
  default. **Whether uzi should notice at all is the maintainer's call** — an open
  question, not something to decide here.
- **Riskiest assumption:** that a withdrawal edge means "stop wanting this work" rather
  than a duplicate-close where the work is still wanted elsewhere, or transient label
  churn. **Hour-scale validation:** over a live instance's history, count closes/de-labels
  of issues with a non-terminal run and read what happened to the run and its MR
  afterwards; if the "still wanted" share is material, M2 stays OFF and M1's notice gains a
  one-click "keep going" that stamps the marker without pausing.
- **No `mr_state = 'merged'` exclusion.** `mr_state` is written only for completed runs
  (`forge.sql:614`, `WHERE l.status = 'completed'`), so for the non-terminal set this pass
  targets it is inert. The pass must not gate on it.
- **Latency asymmetry.** For a uzi-labelled issue, a close on the forge is seen on the
  next incremental poll (its `updated_at` bumps); de-labelling and deletion only within one
  full reconcile (`docs/configuration.md:134`). A bot-assigned-only issue that is closed is
  in none of the incremental fetches (uzi-labelled, open, finding-labelled), so its close
  surfaces at the next full reconcile as `evicted`, not `closed`. Note that `docs/board.md:307` groups closing with
  de-labeling and deleting under "one (less / frequent) reconcile pass", so the two docs
  disagree about closing — fix `board.md` in passing.
- **One run per issue.** `uq_runs_one_active_per_issue`
  (`00170_run_pool_wait.sql:34-37`, which excludes `pool_wait` — see its rationale at
  `:24-29`) PLUS the Go pre-check `HasActiveRunForIssue`
  (`api/internal/workersvc/service.go:5337-5346`, query `autopilot.sql:96-102`), so one
  withdrawal edge maps to at most one run.
- **Size.** M1 small: one migration, one query pair, one pass, one kind, one badge, one
  fixture; the hidden step is the board self-close stamp. Minimal v1 slice: `closed` +
  `evicted` reasons, the notice, the run-page badge — drop `ineligible` (needs the bot-id
  plumbing) and the board card. M2 small on top, because it reuses the owner-pause path.
- **Explicitly out of scope.** `mr_rework`/`ci_fix` and other issue-less runs
  (`poller.go:393-395`); chat / judge / self_improve; reacting to issue edits or new
  comments; auto-cancel; closing the MR or deleting the branch; reopen handling; webhooks.
- **Adjacency.** Open PRDs `prds/1190-run-pause-resume.md` (owner pause, whose path M2
  reuses), `prds/1497-park-at-wall.md`, `prds/1229-durable-run-holds.md`,
  `prds/1202-on-demand-mr-rework.md`/`prds/1233-structured-blockers-mr-rework.md`; forge
  issues #1461 (mr_rework re-arms after owner cancel), #1324 (park on forge outage at
  finalize), #813 (harden mr_rework) — adjacent lanes, different mechanism. The six prior
  bingo files: run-queue-priority shipped (`prds/done/320-run-queue-priority.md`);
  worker-pause-quiesce's adjacent work shipped (`prds/done/496-worker-cordon-pill.md`);
  mr-review-rework-runs shipped and owns `api/internal/poller/mr_review_watch.go`;
  `ideas/bingo/2026-09-01-mr-conflict-watch.md`,
  `ideas/bingo/2026-09-08-repo-wide-ci-failure-gate.md` and
  `ideas/bingo/2026-09-15-report-only-delivery.md` are still design records with no PRD
  landed. None touches issue-state→run-lifecycle.

## Dedup checks performed

Commands re-run at `a10da71b`:

| Check | Command | Result |
|---|---|---|
| Marker name is unused | `git grep -n -i -F 'issue_withdrawn' -- .` | Empty (rc 1) |
| No issue-close→cancel already | `git grep -n -i -E 'CancelRunForIssue\|cancel.*issue.*closed\|issue.*closed.*cancel' -- api/ agent/src` | Empty (rc 1) |
| Prior withdrawal/de-label prose | `git grep -n -i -E 'withdrawn\|eligibility revoked\|de-?label' -- prds/ ideas/ adr/ docs/ specs/human.md` | 85 `de-node-label` HTML hits in `docs/diagrams/` (noise) plus 31 hits in 16 files, all incidental: the two latency docs (`docs/board.md:307`, `docs/configuration.md:134`); cache-eviction/reconcile prose (`prds/done/2-forge-integration-kanban.md:130,133,170,179,185,196`, `prds/done/4-agent-runtime-workers.md:96`, `prds/done/527-mr-merged-state-recording.md:53,118,149`, `prds/done/98-judge-menu.md:729`); "withdrawn" meaning a retracted bullet or a withdrawn pause request (`prds/done/224-worker-ephemeral-storage.md:96,249,260,1153,2945`, `prds/done/65-forgejo-support.md:502,1194`, `prds/done/98-judge-menu.md:512`, `adr/0216-fleet-aware-claim.md:45`, `adr/1190-run-pause-invariants.md:69,71`, `prds/1190-run-pause-resume.md:58`, `prds/1296-durable-run-recovery.md:207`, `prds/mockups/run-budget-extend-pause-mock.html:472`); label-word coincidences such as `decodeLabels` (`prds/done/22-prdless-label.md:98`, `prds/done/982-api-contract-fixtures.md:221,371`, `prds/done/1208-web-ux-polish.md:32`). None proposes a live run reacting to its issue's withdrawal |
| A forge-observation stop function | `git grep -n -E '^func .*\) (Cancel\|Supersede\|Abort\|Halt)[A-Za-z]*\(' -- api/ \| grep -v _test` | Only `CancelReworkForMR` keys on a forge observation, and on an MR — not an issue |
| Close-edge query passes | `grep -rn '^-- name:' api/internal/store/queries/ \| grep -i -E 'close\|reopen\|withdraw\|label'` | The two close-edge passes (`ListFindingIssueCloseEdges`, judge twin) settle dispositions, never a run |
| Any withdrawal/completion notification kind | `git grep -n -P 'Kind:\s+"' -- api/internal \| grep -v _test` | Kinds are `ci_autofix_*`, `mr_rework_halted`, `run_failed`, `schedule_error` (+ const-declared siblings); the `completion_decision`/`follow_up` hits are run-input kinds, not notifications — no withdrawal or completion kind |
| Non-terminal run ↔ issues join | `git grep -n -F 'i.state' -- api/internal/store/queries/` | 10 hits, 3 files; all gated to terminal/completed runs (`forge.sql:614`) or filed-disposition passes |

## Also considered this cycle (not proposed)

- **(a) Run file footprint + cross-run path overlap.** The worker computes the branch's
  changed paths at finalize and discards them (`agent/src/runner.ts:2842`
  `const changedForGuard = await this.git.changedFiles(barePath, trackingRef);`;
  `agent/src/git.ts:2075-2078`), while the plan-turn analogue already persists
  (`00154_run_plan_changed_files.sql:2`, `ALTER TABLE runs ADD COLUMN plan_changed_files
  text[];`), and the release runbook probes collisions by hand
  (`.agents/skills/uzi-release/SKILL.md:38`). Two prior ideas already deferred this
  cross-PR/MR overlap family (`ideas/bingo/2026-09-01-mr-conflict-watch.md:278-280`,
  `ideas/bingo/2026-09-15-report-only-delivery.md:274-279`). Lost on the worker-protocol
  tax and precision risk — most pairs overlap only on CHANGELOG/queries files.
- **(b) Plan-gate pre-flight screen for writes the run's token cannot push.** Grounded in
  the watcher's workflow-scope plan-trap (`.agents/skills/uzi-watcher/SKILL.md:141-146`),
  the finalize `fail_origin: "workflow_scope_missing"` (`agent/src/runner.ts:3170`), and
  the deterministic plan screen `internal/planpolicy` (`api/internal/planpolicy/planpolicy.go:1-2`).
  Overlaps open PRDs `prds/1416-published-branch-rewrite.md:28` (M5 plan-gate nudge) and
  `prds/1297-prevent-and-salvage-finalization-blockers.md:33` (salvaging finalization
  blockers), so it is theirs to extend, not a fresh idea.
