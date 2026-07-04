# PRD #12: Board–Run Lifecycle Integration — Auto Column Moves, Run-aware Cards, In-app Issue View

**GitLab Issue**: [vtmocanu/uzi#12](https://gitlab.example.com/vtmocanu/uzi/-/issues/12)
**Status**: In progress — 3/5 milestones (2026-07-04: M1+M2 implemented, reviewed, audited, APPROVED; M3 implemented + gates green, review/audit wave pending). Branch `prd-12-board-run-lifecycle` (worktree `uzi-prd-12`), 8 commits, tip 59050fa. **Resume next session with: M3 review wave → M4 → M5.** (Spec history: reviewed by 2 agents pre-implementation; move-retry reconciliation added by user review, hardened by a second agent round; see Design Decisions.)
**Priority**: High
**Created**: 2026-07-04
**Depends on**: PRD #2 (board/forge sync, done), PRD #4 (agent runtime, done). Reuses PRD #11's Markdown component if landed.

## Problem

The board and the agent runtime don't know about each other. Current flow: board → **Start run** on an issue → agents work → MR opens. During and after all of that:

1. **The issue never leaves Open.** While agents work it should sit in **In Progress**; once the MR is open it should move to a review column — today the operator must drag it by hand.
2. **The board gives zero signal that a run happened.** No badge, no MR link on the card — the only way to notice a finished run is the Runs page. The board also never updates itself (single load + manual Refresh, `web/src/pages/Board.tsx:74-77`), so even a hand-refreshed mental model goes stale during a run.
3. **Clicking an issue ejects you to GitLab.** The card title is an external `<a target="_blank">` (`Board.tsx:288-296`). Expected: clicking an issue stays in-platform and shows its runs / a start-run action; GitLab remains reachable via an explicit icon.

Note on the existing column model: seeded columns are **In Progress, Upcoming, Later** (`api/internal/forgesvc/service.go:28-32`) — Upcoming/Later are backlog buckets, In Progress is the only workflow state. The automation below only ever touches **In Progress** and the new **Human Review**; it never moves cards between backlog buckets except to restore them (see restore rule).

## Design Decisions (from review)

1. **In Progress is applied at run *creation* (queued), not at first `running`.** The user's click is the intent signal; it also gives instant board feedback ("did my click work?") and sidesteps a real defect the fact-check found: the server cannot distinguish a first `running` from a requeue-resume (`SetRunRunning` uses `COALESCE(started_at, now())` and SetState re-reads after the write — the prior status is gone), so a running-triggered move would re-drag the card on every resume and fight manual placement.
2. **Failed/cancelled restore the *pre-run column*, not Open.** The origin column label is snapshotted onto the run row at creation (`runs.origin_column`); teardown restores it. A card you ran from "Later" goes back to "Later" — prioritization is never silently destroyed. (Moving backward to a hardcoded Open also contradicted our own "failure is a badge, not a column" stance.)
3. **Failed forge moves are retried via reconciliation, not dropped and not queued** (2026-07-04, user review; hardened by a second fact-check + design-review round). The original "no retry, next lifecycle event heals it" stance was wrong for `completed` — it is the terminal event, so a failed completed→Human Review write would never heal. Instead of a persisted move queue (dedup/ordering/staleness complexity), reconciliation: a pending-move marker stamped **in the same transaction as the status write** (closing the crash-between-status-write-and-AutoMove gap), cleared by AutoMove on success, retried by a background loop that re-derives the target from the run's *current* status. Key correction from review: the reconciler needs its own **total** status→column map — `{queued, running, awaiting_approval} → In Progress; completed → Human Review; failed/cancelled → origin` — because by retry time a queued run has usually advanced to `running`, which the live Notify hook deliberately ignores (Decision #1); reusing the notifier's partial map would no-op the most common retry forever. Bounded at 30 minutes; on give-up the marker stays (visible drift, not silent).
4. **Automation hooks a single status-change notifier, not SetState.** The fact-check enumerated five transition sites SetState never sees: server-side cancel/reject in `SubmitInput` (`workersvc/service.go:658-673`), the sweeper (`Sweep`, :722), register orphan recovery (:173-189), and claim-credential failure (`MarkRunFailedByID`, :221). A `cancelled` status is *unreachable* through SetState (switch rejects it, :426). New: `runLifecycle.Notify(runID, newStatus)` called at every status-write site; AutoMove (and the existing `bcast.PublishState`) subscribe.

## Solution Overview

1. **Server-driven column automation** (forge-first, best-effort):
   - run created (queued) → **In Progress** label; origin column snapshotted;
   - run completed (MR opened) → **Human Review** label;
   - run failed or cancelled → restore **origin column** (skip if the user manually dragged the card after the run started — see "manual drags win");
   - never auto-move a closed issue (guard).
2. **Run-aware, self-refreshing board**: cards carry `latest_run`; badges/MR chip render run state; the board polls while mounted; an **attention strip** surfaces approval-blocked runs board-wide.
3. **In-app issue view**: `/repos/:repoId/issues/:iid` — description (mandatory), run history, gated Start run; GitLab demoted to an icon-link on cards.

## Technical Design

### 1. Column automation (API)

- **Hook**: `runLifecycle.Notify(runID, status)` invoked at every status-write site (SetState `running/completed/failed`; `SubmitInput` `CancelRunServerSide`/`RejectRunServerSide`; `Sweep`; orphan recovery; `MarkRunFailedByID`; `CreateRun` for `queued`). AutoMove reacts to `queued`, `completed`, `failed`, `cancelled`; ignores the rest.
- **Move mechanics**: reuse the forge-first path the board drag uses — `planLabelMove` semantics (set target column label, strip other column labels, one atomic `UpdateIssueLabels` call, then local `UpsertIssue` snapshot; `handler/board.go:304-381` today, extracted into forgesvc as `AutoMove(ctx, repoID, issueIID, toColumn)` so both handler and workersvc can call it).
- **Credentials**: the run row carries `user_id` + `repo_id`; the connection's bot PAT is resolved and decrypted server-side following the `GetRunClaimContext` pattern (`workersvc/service.go:251-263`) — no user session needed in worker/sweeper context. Note (fact-checked): `GetRunClaimContext` as-is does **not** return `forge_project_id` or the repo's board-column set, both of which AutoMove needs (`UpdateIssueLabels(projectID, …)`, `planLabelMove` column stripping) — extend that query or add a companion query; not pure reuse. Label writes are attributed to the bot user on GitLab; this misattribution (run starter ≠ label author) is accepted and documented.
- **`runs.origin_column` + `runs.board_column`** (migration): both text, nullable. `origin_column` is set at CreateRun from the issue's current column (`""` = explicitly no column / Open; `NULL` = unknown, e.g. pre-migration rows) and drives the restore rule. `board_column` records the column automation last **successfully** applied for this run (NULL until the first successful move) — it exists because "the automation's last-written column" is otherwise not a stored fact, and on a failed write it is undefined. **Manual-drag guard** (applies to restores and to every retry): proceed iff the issue's current column `== COALESCE(board_column, origin_column)`; anything else means a human placed the card — skip the move and clear the pending marker. For failed/cancelled restores with `origin_column IS NULL`, skip the restore (unknown snapshot ≠ Open; never strip a human's column label on a guess).
- **Cancelled nuance** (verified in `agent/src/runner.ts:162-166`, `steering.ts:107-109`): a live-poller cancel reaches the server as SetState `failed` with a "run cancelled" failure reason — status `cancelled` only occurs via the server-side no-poller branch. Teardown treatment is identical (restore origin), so automation is unaffected; only the badge differs (see §2), derived from status + failure_reason, flagged as a known soft heuristic.
- **Human Review column**: one consistent board order everywhere — **In Progress, Human Review, Upcoming, Later** (the two workflow columns lead; the backlog buckets follow). Fresh boards get it from `DefaultColumns` in that order; boards seeded before Human Review existed are retrofitted **at board load** (`GetBoard`: if the repo has columns but lacks Human Review, ensure the label, then insert the column at In Progress's position + 1). Because `board_columns.position` is not unique and `InsertBoardColumn` neither shifts nor is position-keyed, the retrofit first bumps the displaced columns up one (`ShiftBoardColumnsFrom`) so Human Review lands directly after In Progress instead of tying the column it displaces. Never created as a run side effect (a run mutating curated column config was reviewed as wrong).
- **Best-effort + reconciled semantics**: forge errors never fail or block a run transition (tested with a failing forge stub). Mechanism (`runs.move_pending_since`, timestamptz nullable):
  - **Stamp**: each status write at a Notify site that AutoMove reacts to (queued/completed/failed/cancelled) sets `move_pending_since = now()` **in the same transaction** as the status write — this closes the crash window between status persist and AutoMove (flag-on-failure alone would miss it). A newer transition legitimately re-stamps (new intent, new deadline); a retry failure never re-stamps (else the deadline never elapses).
  - **Clear**: AutoMove clears it on success (and records `board_column`); the guard clears it when a manual drag is detected; the board `MoveIssue` handler clears pending markers for that issue's runs on any manual drag ("a manual drag heals it" is literal).
  - **Retry**: a background reconcile loop picks up runs with `move_pending_since` older than a short grace (avoid racing the inline AutoMove) and within the 30-min window. Target is re-derived from the run's **current** status via the reconciler's total map ({queued, running, awaiting_approval}→In Progress; completed→Human Review; failed/cancelled→origin) — NOT the notifier's partial map, see Design Decision #3. Guards: manual-drag guard (above), closed-issue skip; re-read run status and issue labels immediately before the write to narrow the clobber window vs a concurrent human drag (GitLab labels have no CAS — acknowledged residual race, ~15s cadence makes it rare).
  - **Isolation**: the retry loop must not sit inside the liveness `Sweep` pass — forge I/O (`FORGE_HTTP_TIMEOUT` 15s) against a down forge would starve worker-loss recovery (15s ticker, non-overlapping). Own goroutine/ticker, or after the liveness batch with a bounded per-pass budget + context deadline.
  - **Give-up**: after 30 minutes, stop retrying but do **not** clear the marker (silent clear would hide the drift behind a correct-looking badge); warn-log. The stale marker is cleared by the next transition or manual drag, and is available to a future "column out of sync" card indicator.
  - Reconciliation, not a queue: no move payload is stored; recomputing from current status is idempotent, survives restarts, and cannot apply stale moves (a superseded retry either recomputes the newer target or finds the marker already cleared by the newer transition's inline success). Rationale for existence: `completed` is terminal — nothing later heals a dropped Human Review move.
- **Index**: add composite index on `runs (repo_id, issue_iid, created_at DESC)` for the lateral join and per-issue history (only `idx_runs_repo` exists today).

### 2. Run-aware board (API + web)

- **DTO**: each card gains `latest_run: { id, status, mr_iid, failure_reason, owner_name, worker_name, created_at, updated_at } | null` (newest by `created_at`; LEFT JOIN LATERAL). Runs are owner-scoped today (`ListRunsForUser`), so on multi-user boards the latest run may belong to someone else: `owner_name` is shown ("started by X") and the run-view link renders only for the owner (a non-owner clicking would 403 on `GetRunByIDForUser`).
- **Badges/chips** (`Board.tsx` IssueCard):
  - `queued` — neutral "queued" (instant click feedback);
  - `running` — pulsing badge + elapsed ("running 4m") + worker name;
  - `awaiting_approval` — amber, most prominent card treatment;
  - `failed` — rose badge (+ tooltip with failure_reason); shown while the card sits back in its origin column;
  - `cancelled` (or failed with cancelled reason) — neutral calm "stopped" badge, never rose (a deliberate human stop is not breakage; also fix `RunsList` statusTone which reds cancelled today);
  - `completed` — MR chip `!{mr_iid}` linking to the MR; when `mr_iid` is null, a plain "completed" badge (never invisible);
  - retry hint: when an issue has >1 run, a subtle "×N" count next to the badge (full history in the issue view).
  - Known limitation (out of scope, documented): no MR state tracking — a chip can advertise an already-merged MR, and Human Review never auto-drains on merge. Follow-up PRD.
- **Attention strip**: above the columns, a persistent banner when any of the user's runs on this repo is `awaiting_approval`: "N run(s) awaiting your approval →" linking to the run view(s). Column-independent — this is the state where a human is the blocker and a worker is held busy.
- **Liveness**: the board polls `getBoard` + preconditions every ~10s while mounted (visibility-gated: pause when `document.hidden`). This is what makes auto-moves and badges appear without manual Refresh; a toast ("#42 → Human Review") on observed auto-moves. WS push is out of scope.
- **Start-run gate** switches from the `api.listRuns()` fan-in (`Board.tsx:55-72`) to `latest_run` (one fewer request, no race).
- Board subtitle copy updated: columns move automatically with runs; drags still work.

### 3. In-app issue view (web + API)

- Route `/repos/:repoId/issues/:iid` (`web/src/pages/IssueView.tsx`): title, iid, labels/column, author, **description rendered as markdown — mandatory, not best-effort** (it carries the PRD link and is the basis for deciding to run), full run history for the issue (status, started, duration, worker, MR link, → run view), Start run under the same `startRunGate`, GitLab link.
- API: `GET /api/repos/{id}/issues/{iid}` (card fields + description via the connection PAT) and `ListRuns` gains optional `?repo_id=&issue_iid=` filters (one query; also used by the board's attention strip).
- Card interaction (`Board.tsx`): title becomes an in-app `<Link>` with `draggable={false}` (an `<a>` is natively draggable and hijacks the card's HTML5 drag payload — real bug, not cosmetic); a small GitLab glyph (`aria-label="Open on GitLab"`) keeps the forge one click away. RunView gains a breadcrumb back to its issue and board.
- Not in the issue view (documented, GitLab icon covers them): issue comments/discussion, MR state/CI, description editing. Candidates for the MR-state follow-up PRD.

## Out of Scope

- MR state tracking (merged/closed detection, auto-drain of Human Review, chip staleness) — named follow-up PRD.
- Issue comments and description editing in-app.
- WebSocket-pushed board updates (polling only).
- Per-user column-mapping config (fixed mapping; revisit on demand).
- Worker protocol changes.

## Milestones

- [x] **M1 — Lifecycle notifier + column automation** (done 2026-07-04, commits 7c94a09 + 2770520 + 4aef131 + 858c34c; reviewer APPROVED + auditor CLEAR TO SHIP, all findings resolved): `runLifecycle.Notify` at all seven status-write sites; `AutoMove` (forge-first, origin/board-column snapshot, closed-issue guard, manual-drag guard, claim-context-style credential query extended with `forge_project_id` + column set); move-retry reconciliation (same-tx `move_pending_since` stamp, isolated retry loop with total status→column map, new store query for pending runs joined to repo+connection, MoveIssue clears pending on manual drag, 30-min give-up without clearing); Human Review in DefaultColumns + board-load ensure; migration `00021` (`origin_column`, `board_column`, `move_pending_since`, composite index) + sqlc regen; unit tests incl. forge-down leaves pending set (transition still succeeds), retry succeeds + clears + records board_column, queued-failure retried after run advanced to `running` still lands In Progress, retry respects manual drag (incl. failed-first-write case: guard compares against COALESCE(board_column, origin_column), not the never-written target), NULL-origin restore skipped, give-up leaves marker, crash-window covered by same-tx stamp.
- [x] **M2 — Run-aware board payload** (done 2026-07-04, commits bbfcfb8 + 661de3b; reviewer APPROVED + auditor clean): `latest_run` newest-per-issue query (+ owner/worker names, `is_mine`), `ListRuns` repo/issue filters; board gate switched off the listRuns fan-in; tests.
- [x] **M3 — Board UI** (code done 2026-07-04, commits 8a43ed9 + 59050fa, both gates green incl. 25 badge tests; **review/audit wave still pending** — first item next session): badge taxonomy (queued/running+elapsed/awaiting/failed/stopped/MR-chip/completed/×N via server-side `run_count`), attention strip, 10s visibility-gated polling + auto-move toast, title→in-app link with `draggable={false}` + GitLab icon, subtitle copy; RunsList cancelled-tone fix; vitest for badges and gate.
- [ ] **M4 — In-app issue view**: issue endpoint, `IssueView` page (markdown description, run history, gated Start run), breadcrumbs from RunView; vitest.
- [ ] **M5 — Docs + live validation**: docs updated (board howto / README flow); full live loop verified — start from board (queued badge appears ≤10s), In Progress during run, approval surfaces in the attention strip, Human Review + MR chip on completion, failed run restores its origin column with a rose badge, issue click → history in-app.

## Implementation notes (deviations accepted in review, 2026-07-04)

- **`latest_run` uses `DISTINCT ON`, not `LEFT JOIN LATERAL`** (§2 as written): sqlc v1.30 does not propagate LATERAL nullability (lateral run columns typed non-nullable → scan panic on no-run issues). `DISTINCT ON (issue_iid … ORDER BY created_at DESC)` + Go-side mapping (`assembleCards`) is equivalent, rides the same composite index, and the created_at tie is unreachable (one-non-terminal-run-per-issue constraint serializes creation). Reviewer-verified.
- **DTO additions beyond §2**: `is_mine` (server-computed `run.user_id == viewer`; avoids exposing the owner's user id) and `run_count` (window count powering the ×N badge without a client fan-in).
- **Notify is inline at 5 of the 7 status-write sites**; the two batch sites (Sweep, orphan recovery) stamp `move_pending_since` in the same statement and let the reconciler restore origin — the spec's own "no forge I/O in the sweep path" rule makes inline notify wrong there.
- **`bcast.PublishState` was NOT multiplexed through the notifier** (§Design Decision 4's literal wording): two independent hooks, verified no transition lost its WS emission and none double-emits.
- **Latent (pre-multi-user) note from audit**: `owner_name` falls back to the owner's email — harmless today (per-connection repos make every board self-owned) but must switch to a neutral label before boards ever become shared.
- Pending for M5: rebase onto main (99bb913+), docs, live-loop validation, spec sync.

## Success Criteria

- Starting a run moves the issue to **In Progress** (visible on the board within one poll interval, and on GitLab labels) without human action; the MR opening moves it to **Human Review**.
- A failed/cancelled run returns the card to **the column it started from** with a visible badge — never a silently stuck In Progress card, never a lost backlog placement.
- `awaiting_approval` is impossible to miss: attention strip + loudest card treatment.
- Every card with run history shows it at a glance; completed-without-MR is never invisible; cancelled is never styled as failure.
- Clicking an issue title never leaves the app; GitLab is one explicit click; past runs of an issue are one click from the board.
- Forge downtime never blocks or fails a run transition (verified by test); a move that failed during downtime is applied by the reconcile loop once the forge is back (within the 30-min window), without overriding a manual drag; a queued-move failure retried after the run advanced to `running` still lands the card In Progress; retry backlog never delays worker-liveness sweeps.
