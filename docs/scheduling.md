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
  ordinary PRD-issue sweep. A sweep also caps how many issues one fire
  starts, oldest issue first — see [Sweep cap](#sweep-cap) below.
- **Ad-hoc prompt** — no issue at all. A stored prompt runs against the repo
  and opens a merge request directly, for standing "hunt for X and open an
  MR" work with no throwaway tracking issue.

A pinned issue or label sweep can also carry optional **guidance** — see
[Guidance](#guidance) below. A prompt target carries its own prompt text
instead, so guidance isn't offered there.

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

A schedule also has its own **wait-on-limit** toggle, **on by default** for a
new schedule: a fired run parks until the Anthropic usage window reopens
instead of failing — the right behavior for something unattended, typically
firing off-hours. This takes effect for auto-approve schedules too (the
default case), not just the ones that stop at the plan gate. Turn it off for
a given schedule to have a fired run fail outright on a usage limit instead
of waiting. An existing schedule keeps whatever it already had — this is a
create-time default, not a retroactive change.

## Sweep cap

A label sweep also has a **max issues per fire**, applied oldest issue first
(lowest issue number). A new sweep defaults to **10**, so one fire can't fan
out across an entire label's backlog at once; raise it, or in the web modal
blank the field for unlimited (today's original behavior — the CLI always
sends a cap, defaulting to 10, so an unlimited sweep is web-only). An
existing sweep created before this cap existed stays unbounded until you set
one. If the oldest N candidates are all still mid-run from a previous fire,
this fire starts none of them and doesn't reach newer issues — bounded, and
self-correcting on the next fire.

## Guidance

A pinned-issue or label-sweep schedule can carry optional **guidance**: free
text from the owner, injected into the run instruction as a section clearly
separate from the issue body, to steer *how* a run approaches its task
("always add a failing test first", "keep the diff small, no new deps")
without editing every issue. It doesn't change *what* the task is, and it
has no effect on which issues are eligible to fire. Guidance is capped at
8 KiB; on the rare issue whose body is already near the run's size limit,
the guidance is truncated rather than the issue being skipped, so the run
still happens. A prompt schedule has no guidance field — its prompt text
already is the instruction.

## Managing schedules

- **Web**: the **Schedules** page lists your schedules, and a "Schedule…"
  entry point on an issue opens the create modal pre-pinned to it. The modal
  offers cadence presets (weekdays, every day, every N hours) plus an
  advanced raw-cron field, and a live "next fires" preview. Pause, resume,
  and run-now are per-schedule row actions on the list.
- **CLI**: `uzi schedule create | list | get | pause | resume | run-now |
  delete` — see [the CLI reference](./cli.md#commands) for the full flag list.

## Fire outcomes

A schedule can fire right on time and still start **zero** runs — every
candidate can be benign-skipped by the same gate a manual start goes
through. The motivating case: a `bug` label sweep with `max_issues: 1`
whose single oldest candidate has no `prds/*.md` link and no `PRDLESS`
label — the fire runs every night, `Last run` keeps advancing, and
nothing ever starts. Without a fire outcome, that looks identical to a
healthy schedule.

Each fire records how many candidates it **matched**, which ones
**started** (paired with the run they produced), and which were
**skipped**, each with a typed reason — never free text:

- `no_prd_link` — the candidate issue has no `prds/*.md` link and no
  [PRDLESS](./prdless.md) label.
- `not_eligible` — the candidate isn't a run-eligible issue at all.
- `already_running` — an active run already exists for that issue (or,
  for the schedule itself, a dedup at fire time).
- `description_too_large` — the composed run instruction (issue body
  plus any [guidance](#guidance)) exceeds the size limit.
- `fetch_failed` — a transient forge or database error while checking
  a sweep candidate. The same underlying error is handled differently
  by target: on a **pinned issue**, it's transient and the fire retries
  next tick with nothing recorded; on a **sweep**, one bad candidate
  can't stall the rest of the fan-out, so that candidate is bucketed
  `fetch_failed` and the sweep continues.

`matched == started + skipped` always holds — every candidate lands in
exactly one bucket, so the tally never silently drops one.

The outcome surfaces everywhere a schedule's status does: the
Schedules page's `Last run` cell (an outcome badge — started work vs.
started nothing) with an expandable **Last fire** panel giving the
per-issue breakdown; `uzi schedule get`'s **Last fire** block (and its
`--json` `.last_fire`); and `uzi schedule run-now`'s per-candidate
summary. A sweep fire that was truncated by the [cap](#sweep-cap) and
started nothing also carries the actionable hint — raise `max_issues`
or add PRDLESS / a PRD link to the issues behind it.

Only the **last** fire is kept, and only the last *scheduled* one:
`last_fire` is written on the same path that advances the schedule, so
a **parked** schedule (bad repo or config) or a fire that hit a
transient error (retried next tick, see `fetch_failed` above) leaves
`last_fire` untouched — it shows whatever fired before, or nothing. A
`run-now` fire reports its own outcome in the response without
touching `last_fire` at all, since a manual fire must not disturb the
cadence. A schedule that has never fired reads `last_fire: null`.

## Restarts and missed fires

A recurring schedule survives an api restart: its next fire time is stored,
not held in memory. A fire missed while the process was down (or a slow
tick) still fires **once**, promptly, on the next wake — never a backfill of
every cadence missed. A one-time schedule left overdue by a restart fires
once, the same way, then goes terminal.
