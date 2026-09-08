# ADR-1190: `paused` is a status, a pending pause is a flag, and both stay out of every sweep

**Status**: Accepted (implemented, issue #1190, M1–M5)
**Date**: 2026-09-08
**Deciders**: architect + UX review of the mock (2026-09-07); coder (implementation); reviewer.
**PRD**: [prds/1190-run-pause-resume.md](../prds/1190-run-pause-resume.md) — carries the full milestone breakdown, code anchors, and Decision Log D1–D15; this ADR restates the negative-space invariants a future edit to a status list or a park-clearing statement would break silently, exactly the kind of rule the `TERMINAL_RUN_STATUSES` 🔴 note (`agent/src/home-reclaim.ts`) and [ADR-628](0628-cross-worker-resume-durability.md)'s affinity predicate already exist for.
**Related**: reuses [ADR-628](0628-cross-worker-resume-durability.md)'s affinity leg for the cross-worker resume, [ADR-35](0035-run-limit-retry.md)'s park/promote shape as the direct precedent, and the gate-park budget accounting issue #783 introduced (kept, not the limit park's fresh-wall reset).

## Decision (summary)

A run's owner can park a `running` run on demand (`pause`) and resume it later (`resume`). The design leans on two load-bearing distinctions that are each easy to erode one edit at a time:

1. **A *pending* pause is a flag on a `running` run (`pause_requested_at`/`pause_mode`/`pause_after_count`), never a status of its own.** The run stays `running` — visibly, to every status-keyed query — until the worker actually parks it.
2. **`paused` (the status the *parked* run carries) must be absent from every status list that would otherwise treat it as live-and-recoverable-by-force.** A worker roll, an over-cap sweep, or a stale/reordered report must never un-pause a run whose worker has already exited and freed its slot.

## Why this needs its own record

Every invariant below is a status this PRD deliberately left OUT of a list, or a guard clause it deliberately left IN a statement. Neither shows up in a diff of "what changed" the way an added feature does — a `git grep -F pool_wait` from the next status this codebase adds would not surface `paused`'s *absence* from a list it was never in. The PRD's Decision Log (D6, D9) argues each of these once; this ADR is where the next editor of `RequeueRunsOfStaleWorkers` or `SetRunRunning` actually looks.

## The invariants

### I1 — a pending pause is a flag, never a status (D3)

`pause_requested_at`/`pause_mode`/`pause_after_count` live on the `running` row (`CreatePauseInput`, `api/internal/store/queries/runtime.sql`). No transient status (a `pausing` value was explicitly rejected) exists for "asked to pause but not parked yet" — the worker may never reach the park at all if the checkpoint publish fails (I6). The web's pending chip, the CLI's `PAUSE_REQUESTED` row and the TUI's `pauseRequestedLine` all key off the columns being non-null, not off any status.

### I2 — `paused` is absent from every status list that would un-pause a dead worker's run (D9)

Verified against the live queries: `RequeueRunsOfStaleWorkers`, `FailWorkerRunsOverCap`, `RequeueWorkerRuns`, and `FailRunsOfStaleWorkersOverCap` (`api/internal/store/queries/runtime.sql`) all scope their `status IN (...)` to `'claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup'` — `paused` is not, and must never be, a member of any of the four. A paused run's worker has already exited by design (the park frees its slot); a worker-death sweep finding that same worker gone must leave the run exactly where it is, paused, not fold it into a requeue or an over-cap failure it never asked for. The same reasoning that keeps a paused run out of these four keeps it out of two more:

- **`TERMINAL_RUN_STATUSES`** (`agent/src/home-reclaim.ts`) — `{completed, failed, cancelled}` only. `paused` must never join it: doing so would let the worker's reclaim sweep delete a paused run's HOME (skills, git identity, in-progress state) out from under a resume that expects it there for a same-worker session continuation.
- **`ListActiveRunsForHealth`** (`api/internal/store/queries/runtime.sql`) — a positive allowlist of `queued, running, awaiting_approval`. `paused` must never enter it: the health detector would then flag a run that is, by design, doing nothing on purpose.

A live-DB test pins each of the six lists against a paused fixture row.

### I3 — `SetRunRunning`, `SetRunAwaitingApproval` and `FailRunAutoStop` keep their `<> 'paused'` guards

All three statements start from a **negative** predicate (`status NOT IN (terminal)` or equivalent) that, left alone, *admits* `paused` — the same shape that made `limit_wait` and `pool_wait` need their own explicit exclusions before `paused` existed. Each carries `AND status <> 'paused'` for the reason its own comment gives:

- **`SetRunRunning`** — a paused run's worker has exited; a stale or reordered `running` heartbeat (the batcher retries, and pre-gate fire-and-forget reports already exist) arriving after the park would flip `paused` back to `running` under a worker that is gone, and the run would sit ownerless until `RUN_TIMEOUT`. A resume is unaffected: `ResumePausedRun` lands the row at `queued` before any worker reports, so the guard never blocks a legitimate resume.
- **`SetRunAwaitingApproval`** — a paused run's only exit is the server-side resume to `queued`; a stale or re-delivered gate report must not un-pause it into a plan gate under an already-exited worker. (Contrast with `awaiting_input`, which deliberately gets **no** such guard on this statement — that park's legitimate pre-run path runs *through* `awaiting_approval`; a paused run is never pre-run, so no such path exists for it.)
- **`FailRunAutoStop`** — a paused run's message writes have *stopped* on purpose (the worker parked and exited), not looped; auto-stopping it would be wrong on the merits, the same argument that already exempts `limit_wait`/`awaiting_input`/`awaiting_followup`/`pool_wait` on this statement.

Removing any of the three guards is a silent regression: nothing else in the schema stops the flip, because each statement's own negative predicate is what admits `paused` in the first place.

### I4 — the involuntary parks do not clear a pending pause; it re-arms at the next boundary (D6)

`SetRunLimitWait`, `SetRunPoolWait` and `SetRunAwaitingInput` leave `pause_requested_at`/`pause_mode`/`pause_after_count` untouched. So a `pause` requested while `running`, then overtaken by a usage-limit park, a pool hold, or a clarification question, survives the park **and** its promotion: the running-report ACK's `pause_requested` rule re-evaluates the (unchanged) columns on the first `running` report after the run resumes, and the worker parks at its next boundary — for `--now`, before spending a turn. An owner who asked to pause and then watched the run hit a limit does not have to ask again. The rejected alternative (D6) was clearing the columns on every park, which silently drops the owner's intent, and admitting `pause` on an already-parked status, which would need a second exit condition on each park.

### I5 — the terminal transitions clear the pause columns; `SweepIdleChatRuns` is the one exempt sweep, and only because chat can never carry one

Every terminal-transition statement (`SetRunCompleted`, `SetRunFailed`, `MarkRunFailedByID`, `CancelRunServerSide`, `CancelRunByWorker`, `FailRunAutoStop`, `RejectRunServerSide`, `SweepRunningTimeout`, `FailRunsOfStaleWorkersOverCap`, `FailWorkerRunsOverCap`, all in `api/internal/store/queries/runtime.sql`) sets `pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL` in the same statement — a terminal run carries no pending pause, root-cause cleared rather than left to rot.

**`SweepIdleChatRuns`** (`api/internal/store/queries/chat.sql`) is the one terminal sweep that does **not** clear them, and that is correct today only because `CreatePauseInput`'s kind allowlist (`kind IN ('issue', 'task', 'prompt', 'self_improve')`) excludes `chat` outright — a chat run can never have the columns set in the first place, so there is nothing for this sweep to clear. **If that allowlist ever widens to admit `chat`, `SweepIdleChatRuns` must gain the same three-column clear the other nine terminal statements already carry.** This is the one invariant in this ADR that a change to a *different* query (the allowlist) can silently invalidate; a reviewer widening `CreatePauseInput` should treat this paragraph as a checklist item, not an aside.

### I6 — a failed checkpoint publish means no park, ever (D8)

The worker publishes its checkpoint **before** reporting `paused` (`agent/src/runner.ts`). If the publish fails, the worker reports `pause_failed` instead — a worker **report**, not a status transition — and `ClearPauseRequest` withdraws the pending pause while the run's status is left exactly as it was (`running`). A `--now` pause whose publish fails additionally **restarts** the aborted turn at the next iteration, the same way a cancelled turn is restarted on `revise`. The reason this needs its own line: a paused run whose only copy of its work is one worker's disk is a promise the resume cannot keep if that worker rolls before the resume happens — staying `running` is the honest fallback, never a `paused` row backed by nothing recoverable.

`pause_failed` intentionally posts **no Slack DM** — the Slack notifier (`api/internal/slacksvc/notifier_state.go`) is driven entirely by *status* transitions, and `pause_failed` is not one (the run's status never leaves `running`). The owner learns why through the run's in-app activity feed instead, via the worker's own feed message (*"Could not pause: the checkpoint could not be published. The run is still running and has restarted the interrupted step."*). A Slack DM for this case would need a reason-carrying seam from the handler into `slacksvc` that does not exist today; it is a deferred follow-up, not an oversight to "fix" by bolting a DM onto an unrelated status-transition hook.

### I7 — the budget rule on resume is the gate-park rule, not the limit park's (D2)

`ResumePausedRun` **banks** the parked wall-clock time into `budget_paused_seconds` and **keeps `started_at`** untouched — the run resumes with exactly the budget it had left. This is the *opposite* of `PromoteLimitWaitRuns`, which resets `started_at` and zeroes `budget_paused_seconds` so a limit-park resume gets a fresh full wall (Decision 6d, issue #35). Swapping the two would be a real regression in either direction: a fresh wall on a voluntary pause is an uncapped extension that makes any future budget cap pointless; the limit park's fresh wall exists because that park was never the owner's choice to make in the first place. A mutation test (flip `ResumePausedRun` to zero `budget_paused_seconds` instead of adding to it) is expected to redden the live-DB assertion that pins this.

## Consequences

- A future ninth or tenth status added to this codebase gets its own audit against these six lists (I2) and three guards (I3) the same way `paused` got audited against `pool_wait`'s own PRD (#754) — this ADR is where that checklist should be extended, not re-derived.
- Widening `CreatePauseInput`'s kind allowlist to admit `chat` is not a one-line change: it also obligates the `SweepIdleChatRuns` clear from I5.
- A reason-carrying `pause_failed` → Slack DM seam remains open as a follow-up (I6); until it lands, `pause_failed` is discoverable only in-app.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's Run lifecycle section (the `running → paused` bullet) and from the ADR-628 affinity reference, per repo convention.
