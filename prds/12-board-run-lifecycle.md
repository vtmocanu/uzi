# PRD #12: Board–Run Lifecycle Integration — Auto Column Moves, Run-aware Cards, In-app Issue View

**GitLab Issue**: [vtmocanu/uzi#12](https://gitlab.example.com/vtmocanu/uzi/-/issues/12)
**Status**: Ready (2026-07-04: reviewed by 2 agents — kanban-UX review + adversarial fact-check — all findings incorporated; three design changes resulted, see Design Decisions)
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
3. **Automation hooks a single status-change notifier, not SetState.** The fact-check enumerated five transition sites SetState never sees: server-side cancel/reject in `SubmitInput` (`workersvc/service.go:658-673`), the sweeper (`Sweep`, :722), register orphan recovery (:173-189), and claim-credential failure (`MarkRunFailedByID`, :221). A `cancelled` status is *unreachable* through SetState (switch rejects it, :426). New: `runLifecycle.Notify(runID, newStatus)` called at every status-write site; AutoMove (and the existing `bcast.PublishState`) subscribe.

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
- **Credentials**: the run row carries `user_id` + `repo_id`; the connection's bot PAT is resolved and decrypted server-side exactly as `Claim` already does (`GetRunClaimContext` pattern) — no user session needed in worker/sweeper context. Label writes are attributed to the bot user on GitLab; this misattribution (run starter ≠ label author) is accepted and documented.
- **`runs.origin_column`** (migration): text, nullable; set at CreateRun from the issue's current column; used by the restore rule. Restore is skipped when the issue's current column no longer equals the automation's last-written column (i.e. a human dragged it mid-run — their placement wins).
- **Cancelled nuance** (verified in `agent/src/runner.ts:162-166`, `steering.ts:107-109`): a live-poller cancel reaches the server as SetState `failed` with a "run cancelled" failure reason — status `cancelled` only occurs via the server-side no-poller branch. Teardown treatment is identical (restore origin), so automation is unaffected; only the badge differs (see §2), derived from status + failure_reason, flagged as a known soft heuristic.
- **Human Review column**: appended to `DefaultColumns` (`{Name: "Human Review", Color: "#6e49cb"}`) **and ensured at board load** (`GetBoard`: if the repo has columns but lacks Human Review, ensure label + insert column positioned after In Progress — `InsertBoardColumn` is already idempotent). Never created as a run side effect (a run mutating curated column config was reviewed as wrong).
- **Best-effort semantics**: AutoMove runs after the status write is persisted; forge errors are logged and dropped — a run transition never fails or blocks on the forge (tested with a failing forge stub). No retry queue; the next lifecycle event or a manual drag heals it. The poller's `FullSync` keeps the local snapshot mirroring the forge (it does not retry unwritten label changes — accepted).
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

- [ ] **M1 — Lifecycle notifier + column automation**: `runLifecycle.Notify` at all seven status-write sites; `AutoMove` (forge-first, origin-column snapshot/restore, closed-issue guard, manual-drag respect, bot-PAT resolution in worker context); Human Review in DefaultColumns + board-load ensure; `runs.origin_column` + composite index migrations; unit tests incl. forge-down no-op, terminal-race no-op, restore-skip-after-manual-drag.
- [ ] **M2 — Run-aware board payload**: `latest_run` lateral join (+ owner/worker names), `ListRuns` repo/issue filters; board gate switched off the listRuns fan-in; tests.
- [ ] **M3 — Board UI**: badge taxonomy (queued/running+elapsed/awaiting/failed/stopped/MR-chip/completed/×N), attention strip, 10s visibility-gated polling + auto-move toast, title→in-app link with `draggable={false}` + GitLab icon, subtitle copy; RunsList cancelled-tone fix; vitest for badges and gate.
- [ ] **M4 — In-app issue view**: issue endpoint, `IssueView` page (markdown description, run history, gated Start run), breadcrumbs from RunView; vitest.
- [ ] **M5 — Docs + live validation**: docs updated (board howto / README flow); full live loop verified — start from board (queued badge appears ≤10s), In Progress during run, approval surfaces in the attention strip, Human Review + MR chip on completion, failed run restores its origin column with a rose badge, issue click → history in-app.

## Success Criteria

- Starting a run moves the issue to **In Progress** (visible on the board within one poll interval, and on GitLab labels) without human action; the MR opening moves it to **Human Review**.
- A failed/cancelled run returns the card to **the column it started from** with a visible badge — never a silently stuck In Progress card, never a lost backlog placement.
- `awaiting_approval` is impossible to miss: attention strip + loudest card treatment.
- Every card with run history shows it at a glance; completed-without-MR is never invisible; cancelled is never styled as failure.
- Clicking an issue title never leaves the app; GitLab is one explicit click; past runs of an issue are one click from the board.
- Forge downtime never blocks or fails a run transition (verified by test).
