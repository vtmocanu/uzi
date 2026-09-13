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
| ⚠ looping | The agent has repeated the exact same tool call 4+ times recently — or its updates can't be saved, so it keeps resending them. | Open the run view and check what it's stuck repeating (or whether it's stuck retrying a save); it may need a nudge or a cancel. |
| ⚠ stalled | No new activity for a while, and nothing is currently running (a long build or test suite in progress does **not** count as stalled). | Open the run view — it's either quietly working on something the flag doesn't see, or genuinely wedged. |
| ⚠ near timeout | Has used most (default 85%) of its wall-clock budget while running, gate time excluded; it will be stopped at the timeout. | Let it finish if it is on its last milestone, `uzi run scope --through N` / `run stop` to finalize what is committed, or [give it more time](#giving-a-run-more-time) instead. Raising `RUN_TIMEOUT` only helps a run whose budget isn't already frozen — a milestone-scaled run freezes its `budget_wall_seconds` at plan approval, so a later `RUN_TIMEOUT` bump won't extend it. |
| ⚠ waiting for worker | Queued longer than expected with no worker claiming it. | The reason names why, if you own the run: no worker online, your vault is locked, or just a wait — start a worker or unlock your vault as needed. A judge or self-improve run instead reads **deprioritized** (yielding to interactive work on purpose, not stuck) or, once it's waited past the grace window, **priority restored** — see [Queue priority](#queue-priority). |
| ⚠ needs approval | Sitting at `awaiting_approval` longer than expected (never shown for autopilot runs, which approve themselves). | Approve, reject, or request changes to the plan — see [Plan approval gate](./run-activity.md#plan-approval-gate). |

One **waiting for worker** reason worth calling out is the Docker-worker
allowlist. When every online worker is a Docker worker and the run's repo
isn't on the Docker-worker allowlist, no worker is eligible to claim it, so
the run stays `queued` and the owner's reason names it specifically — _"this
repo isn't on the Docker worker allowlist, so no Docker worker can run it"_ —
distinct from "no worker online" or "all workers busy". The fix is to add the
repo to the allowlist, not to start another worker.

Only the run's owner (and admins) see the reason text behind a flag; everyone
else viewing a shared board sees just the ⚠ badge.

## Giving a run more time

A run that can time out shows its wall-clock budget in the header, right
next to the elapsed time — `3h 48m / 8h`, or `3h 48m / 8h+2h` once it's been
extended. When it crosses **near timeout**, the run's owner gets an
**Extend time…** button in the header and a panel explaining what will
happen: a chooser offering +1h / +2h / +4h / +8h or a custom duration (`2h`,
`90m`, `1h30m`), with the used time, the remaining allowance and the
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
later and get flagged again — that's expected, not a bug. Total extra time
per run is bounded by an admin-set allowance (default 16h); once a run has
used up its allowance, or an admin has turned extending off entirely, the
button and the CLI command both stop working — see [Admin
settings](admin-settings.md#run-health) for that setting.

## Queue priority

Interactive runs (issue, ci_fix, anything you start by hand) are claimed
ahead of background judge and self-improve runs on the same worker. A
background run yields but never starves: past a grace window (about 15
minutes, an operator setting, not per-run) it's restored to normal priority.

**Expedite** any queued run you own — bumping it to the front — from the
Runs list, the run page, or [`uzi run expedite <run-id>`](./cli.md)
(`--clear` to undo). It only matters while `queued`; a claimed run's
ordering is fixed.

**A run paused on an Anthropic usage limit never gets one of these flags,
even after hours.** It isn't stuck — see
[Paused on a usage limit](run-limit-wait.md) for that state.

## This is an early-warning aid, not a guardrail

A flag never stops, kills, or requeues a run — `RUN_TIMEOUT` and the
idle/iteration caps remain the only things that actually end a run, exactly
as before. Run health exists so you notice a sick run sooner than a hard
timeout would tell you, nothing more. Treat the badge as a hint to go look,
not as proof something is wrong (or that nothing is).

## Timing

Flags are (re-)computed on a periodic sweep, so one can appear a little after
its raw threshold — never before. This is most visible on **waiting for
worker**: the clock starts when the run enters the queue, so a run queued
right at the threshold may take one more sweep before the badge shows up.

## Tuning or turning a flag off

An admin can change any threshold, or disable a single signal entirely by
setting it to `0`, from **Admin → Instance settings → Run health** — see
[Admin settings](./admin-settings.md#run-health). The loop-detection window
itself (how many repeats, over how large a window) isn't tunable; every other
signal is — the plain seconds thresholds, and **near timeout**'s share of the
run's wall-clock budget (a percent, not a duration, so it means the same
thing regardless of the run's timeout or frozen budget).

Related: [Paused on a usage limit](run-limit-wait.md) · [Why was my run stopped automatically?](run-auto-stopped.md) · [Configuration](configuration.md)
