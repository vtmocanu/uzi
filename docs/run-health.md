---
title: Run health
order: 35
audience: user
---

# Run health

uzi watches every active run and can flag one that looks stuck, looping, or
close to its timeout — a **⚠** badge on the board, dashboard, runs list, and
run view, and (if you've set up [Slack](./slack.md)) a DM. The badge's
elapsed time counts from when the flag was raised, not from when the run
started — except **near timeout**, whose badge instead counts down the time
left to the deadline (e.g. `1h 5m left`).

## What the flags mean

| Flag | Shows up when | What to do |
|---|---|---|
| ⚠ looping | The agent has repeated the exact same tool call with the same result and success/error status 4+ times recently — or its updates can't be saved, so it keeps resending them. | Open the run view and check what it's stuck repeating (or whether it's stuck retrying a save); it may need a nudge or a cancel. |
| ⚠ stalled | No new activity for a while, and nothing is currently running (a long build or test suite in progress does **not** count as stalled) — or one tool call has been running far longer than a normal build or test suite would (see [Long tool calls](#long-tool-calls)). | Open the run view — it's either quietly working on something the flag doesn't see, or genuinely wedged. |
| ⚠ near timeout | Has used most (default 85%) of its wall-clock budget while running, gate time excluded; it will park and wait for you at the timeout, not fail — see [Running out of time](#running-out-of-time). | Let it finish if it is on its last milestone, `uzi run scope --through N` / `run stop` to finalize what is committed, or [give it more time](#giving-a-run-more-time) instead. Raising `RUN_TIMEOUT` only helps a run whose budget isn't already frozen — a milestone-scaled run freezes its `budget_wall_seconds` at plan approval, so a later `RUN_TIMEOUT` bump won't extend it. |
| ⚠ waiting for worker | Queued longer than expected with no worker claiming it — or, for a repo the Docker worker allowlist blocks, as soon as the next health sweep detects it (see below). | The reason names why, if you own the run: no worker online, your vault is locked, or just a wait — start a worker or unlock your vault as needed. A judge or self-improve run instead reads **deprioritized** (yielding to interactive work on purpose, not stuck) or, once it's waited past the grace window, **priority restored** — see [Queue priority](#queue-priority). |
| waiting for account cooldown | Required new Codex subscription work is blocked by a current account cooldown in `claimed`, `running` or `awaiting_approval`; this is an expected wait, not a fault. | Wait for a fresh admission check, or use the existing run controls. Earliest eligibility does not promise dispatch; see [Account cooldown waits](#account-cooldown-waits). |
| ⚠ needs approval | Sitting at `awaiting_approval` longer than expected (never shown for autopilot runs, which approve themselves). | Approve, reject, or request changes to the plan — see [Plan approval gate](./run-activity.md#plan-approval-gate). |

Tool-loop detection considers the newest 12 tool uses within 40 fetched tool-use/result rows. Only valid calls with a unique ID and one matching later result count; unmatched or malformed uses occupy a slot without being counted or replaced by older calls. Repeated calls with varying results can still be stuck, but this detector does not flag them.

When suitable persistent workers are finishing runs before an upgrade, the reason is
**your workers are finishing their current runs before an upgrade; this run starts after**.
Every static requirement must be satisfied by the same fresh online worker; occupied
slots do not prevent this diagnosis. A suitable draining worker already assigned to the
run vetoes it, because that worker can resume its own work. Incompatible own workers do
not veto it. Account, vault, custody and isolated-profile guards keep precedence.

One **waiting for worker** reason worth calling out is the Docker-worker
repo allowlist. When the owner has workers online but every one of them is a
Docker worker and the run's repo isn't on the Docker worker repo allowlist,
no worker is eligible to claim it, so the run stays `queued` and the reason
names it specifically, distinct from "no worker online" or "all workers busy".
Like the stale-requeue pin reason, and unlike the other waiting reasons, it is flagged as soon as the next health
sweep detects the block, without waiting for **Stuck queued after**
(`health_queued_seconds`); a higher-precedence block (locked vault, custody
limit, and so on) still hides it, and setting `health_queued_seconds` to `0`
still turns it off with every other queued flag. The wording depends on
whether the run's owner is an admin, because only an admin can change the
allowlist (**Admin → Instance → Docker worker repo allowlist**, at
`/admin/settings`). An admin viewing someone else's run sees the owner's
variant.

- Owner is an admin: _"this repo isn't on the Docker worker repo allowlist, so
  your Docker workers can't run it; add it under Admin → Instance → Docker
  worker repo allowlist"_
- Owner is not an admin: _"this repo isn't on the Docker worker repo
  allowlist, so your Docker workers can't run it; ask an admin to add it under
  Admin → Instance → Docker worker repo allowlist"_

The fix is to add the repo to the allowlist, not to start another worker. The
owner's Slack nudge for this flag arrives sooner too, still subject to the
[Slack nudge cooldown](./admin-settings.md).

One **stalled** reason worth calling out is an undelivered outcome. When a
worker has a running or awaiting-approval run's outcome journaled but not yet
delivered to the api (a _pending outcome_; the worker keeps renewing the
run's lease protection meanwhile), the api records when it first saw that
entry. Once it has been pending on the run's current worker, at its current
claim generation, under an unexpired lease, for 4 heartbeat intervals (60
seconds with the api's default `WORKER_HEARTBEAT_INTERVAL` of 15s), the run is
flagged ⚠ stalled and the owner's reason reads _"the run's outcome is
journaled on its worker but has not been delivered"_. The clock survives
heartbeat renewals and a worker re-register, and resets when the entry clears
or the claim generation changes. A worker holding more pending outcomes than
its cap lists only some of them on each heartbeat; the ones it rotates in and
out restart their clock each time, so they are not flagged. This is a warning only: it never expires the
lease or reclaims, fails or discards the run. The outcome is held on the
worker, so check the worker — [`uzi worker list`](./cli.md#disk-usage-and-checkpoint-durability)
shows its pending outcomes. A plain cancel is refused while the outcome is held;
it must be confirmed as discarding that outcome.

### Long tool calls

A long build or test suite sends one tool call and then nothing until it
finishes, so ordinary **stalled** detection leaves a run alone while a call
is in flight. That would let a hung command hide a run for hours. So a
running run is also flagged ⚠ stalled, with the owner's reason _"a tool call
has been in progress longer than the configured threshold"_, when both hold:

- the oldest tool call the run's main agent still has open has been running
  for at least **Long tool call after** (default 20 minutes), and
- nothing at all has arrived from the run, on any agent, for at least the
  **Stalled after** window (or for the long-tool-call threshold itself, if
  **Stalled after** is `0`).

A call to a subagent (a delegation) is never aged, and while one is open
among the run's recent tool calls this flag stays off. A delegation whose
subagent is still sending updates is never flagged. The one exception is a
delegation old enough to have dropped out of those recent calls: if its
subagent has gone quiet past the window while an ordinary call of the main
agent has been open past the threshold, the run is flagged. The age is
measured from when uzi received the call. A run whose older calls all fall
outside the recent window still trips the plain **stalled** flag once it
goes quiet. During an api outage, when the run's worker reports its updates
queued, the reason says so instead. The flag clears on the next check once
no open call is past the threshold or new activity has arrived. Setting
**Long tool call after** to `0` turns this signal off. Like every flag, it
never stops the run.

Only the run's owner (and admins) see the reason text behind a flag; everyone
else viewing a shared board sees just the ⚠ badge.

## Provider recovery and owner attention

A typed `recovery_wait` / `provider_outage` park shows its provider park
count, streak start and next retry. After the configured elapsed streak
bound (default 24h), the next qualifying provider park becomes `paused` /
`provider_unavailable`, an owner-attention hold rather than a running
stalled/looping warning. There is no maximum detection overshoot promise.
Positive completed model turns on the park report start a new streak.

The owner can **Resume** a fresh attempt or **Cancel**. Eligible **Stop**
sets the scope ceiling, then requires **Resume** after success; it is not
an atomic endpoint. Stop requires a non-interactive milestone issue run
with at least one completed milestone. Claim, worker affinity and the
issue lock for issue runs remain held. Clone/HOME and verified captures
depend on surviving storage or an available verified recovery copy, not
a guarantee that every latest edit is recoverable elsewhere.
See [Provider outage streaks](./run-recovery-wait.md#provider-outage-streaks)
for resets, the `0` off switch and mixed-fleet limits.

## Giving a run more time

A run that can time out shows its wall-clock budget in the header, right
next to the elapsed time — `3h 48m / 8h`, or `3h 48m / 8h+2h` once it's been
extended. When it crosses **near timeout**, the run's owner gets an
**Extend time…** button in the header and a panel explaining what will
happen: a chooser offering +1h / +2h / +4h / +8h or a custom duration (`2h`,
`90m`, `1h30m`), with the used time, the maximum per extension and the
resulting deadline shown before you confirm. The extension adds to the
run's existing budget — it never replaces it, and the underlying frozen
budget described in [Milestone-scaled
budgets](configuration.md#milestone-scaled-budgets-prd-122-m2) never moves.
From the CLI, the same action is `uzi run extend <run-id> --by 2h`
(see [the CLI reference](./cli.md#commands) for the accepted duration
formats and what a refusal looks like).

Only the run's owner can extend it; everyone else sees inert text in the
same spot. Extending a run clears its near-timeout flag on the next health
sweep, though a long-enough run can cross 85% of the new, larger budget
later and get flagged again — that's expected, not a bug. An admin-set
maximum per extension (default 16h) limits how much time one request can
add; it is not a lifetime total, so the owner may extend the same run again
and again. Each extension is an explicit, owner-authorized, audited action:
repeated grants permit longer resource use, and nothing extends a run
automatically. When an admin has turned extending off entirely, the button
and the CLI command both stop working — see [Admin
settings](admin-settings.md#run-health) for that setting.

### How much time a run can have in total

Extension admission retains a 2147483-second limit (24d 20h 31m 23s).
The room left for extensions is that limit, minus the larger of the run's
frozen wall budget (`budget_wall_seconds`), current `RUN_TIMEOUT`, current
`RUN_WALL_CEILING` and 72h (the largest `RUN_WALL_CEILING` the configuration
accepts), minus the 30-minute finalize allowance that Stop grants (or the
larger finalize allowance already granted), minus what is already extended.
A request past that room is refused with a 409 naming the limit and the
room left. The 72h reserve covers later plan scaling or a ceiling increase,
including a run with no frozen wall yet. With a frozen wall and current
configuration no larger than 72h, and the standard finalize reserve, a run
starts with 1886483 s (about 21d 20h) of extension room.

This is an admission limit, not an absolute cap on the wall total served to
the worker. A later `RUN_TIMEOUT` increase above 72h can raise the served
total for a run with no frozen wall; that larger total is honored without
clamping. The worker schedules long relative deadlines in chunks within the
native timer range, so exceeding that range no longer fires the timer at
once. Chat, judge and interactive-task runs never time out, so extending
one is refused, and a job is never extended.

## Running out of time

If nobody extends it in time, a run's wall-clock deadline doesn't fail it —
the run parks (`hold_reason: budget_exhausted`) instead and waits for its
owner, the same non-terminal `paused` status [a requested
pause](run-pause.md) uses. It is never failed for the clock alone, and the
park has no expiry — there's no setting that brings back the old
fail-at-the-timeout behavior. From there: **extend** it (`uzi run extend
<run-id> --by 2h`, which both adds time and resumes it in one step),
**stop** it (`uzi run stop <run-id>`, milestone issue runs only —
finalizes the milestones already done into a merge request), or **cancel**
it (`uzi run cancel <run-id>`).

## Queue priority

Interactive runs (issue, ci_fix, anything you start by hand) are claimed
ahead of background judge and self-improve runs on the same worker. A
background run yields but never starves: past a grace window (about 15
minutes, an operator setting, not per-run) it's restored to normal priority.

**Expedite** any queued run you own — bumping it to the front — from the
Runs list, the run page, or [`uzi run expedite <run-id>`](./cli.md)
(`--clear` to undo). It only matters while `queued`; a claimed run's
ordering is fixed.

Expedite also orders new ephemeral worker provisioning by priority within
each eligible trigger class. Class precedence stays **isolated lane → capability
gap → saturation**, regardless of priority. Saturation debounce, placement
eligibility and per-user caps still apply; expedite does not guarantee global
priority precedence or immediate provisioning.

**A run parked at `limit_wait` on a Claude or Codex usage window is excluded
from these running-state flags, even after hours.** It isn't stuck — see
[Paused on a usage limit](run-limit-wait.md) for that state.

## This is an early-warning aid, not a guardrail

A flag never stops, kills, or requeues a run — the idle and iteration caps
remain the only things that actually end a run outright; reaching
`RUN_TIMEOUT` no longer does (see [Running out of
time](#running-out-of-time) above) — it parks and waits for you instead. Run
health exists so you notice a sick run sooner than a hard deadline would
tell you, nothing more. Treat the badge as a hint to go look, not as proof
something is wrong (or that nothing is).

## Account cooldown waits

Owner surfaces show `waiting for account cooldown` with
`account_cooldown_class` (`rate_limit` or `overload`) and
`account_cooldown_eligible_at`, including finite per-run recovery spread.
Health `waiting_account_cooldown` changes neither lifecycle nor worker queue
reason. Required blocked work can explain the wait; optional background advice
cannot label an otherwise working root blocked. Waiting uses existing wall or
whole-call deadlines, without model processing, progress or productivity.

Current ownership, positive claim generation, credential binding and revision
source must match an accepted active snapshot with a 45-second lease.
Omission, expiry or source/status changes clear specialized metadata on an
accepted fresh read. Foreign shared/admin summaries omit class and time.
Missing, disabled or unsupported optional snapshots (including an old API)
leave specialized visibility unknown; operation enforcement remains independent.
Absence proves neither cooldown nor a hung model.

Independent faults, freshness and near-spent wall warnings retain precedence.
Entry, extension, refresh, expiry and exit do not send per-run Slack nudges.
Identical root renders are deduplicated for cooldown churn; unknown/faulted roots
can still need edits and later genuine faults can notify normally.
See [Temporary account admission waits](rate-limits.md#temporary-account-admission-waits).

## Timing

Threshold flags are (re-)computed on a periodic sweep, so one can appear a
little after its raw threshold — never before. This is most visible on **waiting for
worker**: the clock starts when the run enters the queue, so a run queued
right at the threshold may take one more sweep before the badge shows up.
The account-cooldown owner overlay also validates evidence at read time and can
appear before that sweep; operation admission does not immediately recompute
every health signal. The wait stays quiet until a fresh health read: reaching
the earliest eligibility time triggers no fake activity or progress event.

## Tuning or turning a flag off

An admin can change a threshold, or disable its threshold-based signal by
setting it to `0`, from **Admin → Instance → Run health** — see
[Admin settings](./admin-settings.md#run-health). The loop-detection window
itself (how many repeats, over how large a window) isn't tunable; the other
thresholds are — the plain seconds thresholds, and **near timeout**'s share of the
run's wall-clock budget (a percent, not a duration, so it means the same
thing regardless of the run's timeout or frozen budget). The undelivered-outcome
**stalled** reason is the other exception: its threshold is fixed at 4 of the
api's heartbeat intervals and setting **stalled** to `0` does not disable it;
only turning run health off entirely does. Setting **Long tool call after**
to `0` turns off only the [long tool call](#long-tool-calls) reason.

Related: [Paused on a usage limit](run-limit-wait.md) · [Why was my run stopped automatically?](run-auto-stopped.md) · [Configuration](configuration.md)
