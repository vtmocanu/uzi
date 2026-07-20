---
title: Run health
order: 35
audience: user
---

# Run health

uzi watches every active run and can flag one that looks slow, stuck, or
looping — a **⚠** badge on the board, dashboard, runs list, and run view, and
(if you've set up [Slack](./slack.md)) a DM. The badge's elapsed time counts
from when the flag was raised, not from when the run started.

## What the flags mean

| Flag | Shows up when | What to do |
|---|---|---|
| ⚠ looping | The agent has repeated the exact same tool call 4+ times recently. | Open the run view and check what it's stuck repeating; it may need a nudge or a cancel. |
| ⚠ stalled | No new activity for a while, and nothing is currently running (a long build or test suite in progress does **not** count as stalled). | Open the run view — it's either quietly working on something the flag doesn't see, or genuinely wedged. |
| ⚠ slow | Running much longer than typical, wall clock. | Usually fine for a big task; worth a look if unexpected. |
| ⚠ waiting for worker | Queued longer than expected with no worker claiming it. | The reason names why, if you own the run: no worker online, your vault is locked, or just a wait — start a worker or unlock your vault as needed. |
| ⚠ needs approval | Sitting at `awaiting_approval` longer than expected (never shown for autopilot runs, which approve themselves). | Approve, reject, or request changes to the plan — see [Plan approval gate](./run-activity.md#plan-approval-gate). |

Only the run's owner (and admins) see the reason text behind a flag; everyone
else viewing a shared board sees just the ⚠ badge.

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
itself (how many repeats, over how large a window) isn't tunable; only the
time-based signals are.
