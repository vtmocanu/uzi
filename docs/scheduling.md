---
title: Scheduling
order: 82
audience: user
---

# Scheduling

A **schedule** starts run(s) at a future time, unattended — a third way to
start work alongside the manual **Start run** button and
[autopilot](./autopilot.md). Where autopilot reacts to a label appearing on
an issue, a schedule reacts to the clock: **one-time** (fires once at a
timestamp, then goes terminal) or **recurring** (a cron cadence, interpreted
in a named IANA timezone so DST doesn't drift it).

## Targets

Every schedule fires against exactly one target:

- **Pinned issue** — one repo + issue, the same shape as clicking Start run
  on that issue yourself.
- **Label sweep** — at fire time, every *open* issue on the repo matching a
  label selector; an empty selector defaults to the PRD label, i.e. today's
  ordinary PRD-issue sweep.
- **Ad-hoc prompt** — no issue at all. A stored prompt runs against the repo
  and opens a merge request directly, for standing "hunt for X and open an
  MR" work with no throwaway tracking issue.

## The same gates a manual start has, plus auto-approve

Pinned-issue and label-sweep fires go through the **exact run-creation path**
autopilot uses, so PRDLESS gating, a fresh forge fetch of the issue's labels,
active-run dedup, and the usage-limit park all behave exactly as for a
manual start — a schedule can't do anything a manual start couldn't. A label
selector only picks *candidates*; the gate still decides what fires. A sweep
over already-PRD'd issues fires directly, but a sweep over plain, un-PRD'd
issues (e.g. everything tagged `bug`) fires only on the ones that also carry
the [PRDLESS](./prdless.md) label — pair a raw-bug-report sweep with PRDLESS
if you want it to run anything. The **ad-hoc prompt** target is the
deliberate exception: with no issue to gate on, it bypasses the PRD-issue
requirement by design.

Each schedule also has its own **auto-approve** toggle, **on by default** —
the point of a 02:00 fire is that it actually proceeds instead of sitting at
the plan-approval gate waiting for someone awake. Turn it off for a given
schedule to make its runs stop and wait for a human, same as a manual start.
Either way, the plan is still recorded on the run to read afterwards, and a
human still merges the resulting MR: `main` is never written under any
target, timing, or approval setting.

## Managing schedules

- **Web**: the **Schedules** page lists your schedules, and a "Schedule…"
  entry point on an issue opens the create modal pre-pinned to it. The modal
  offers cadence presets (weekdays, every day, every N hours) plus an
  advanced raw-cron field, and a live "next fires" preview. Pause, resume,
  and run-now are per-schedule row actions on the list.
- **CLI**: `uzi schedule create | list | get | pause | resume | run-now |
  delete` — see [the CLI reference](./cli.md#commands) for the full flag list.

## Restarts and missed fires

A recurring schedule survives an api restart: its next fire time is stored,
not held in memory. A fire missed while the process was down (or a slow
tick) still fires **once**, promptly, on the next wake — never a backfill of
every cadence missed. A one-time schedule left overdue by a restart fires
once, the same way, then goes terminal.
