---
title: Pausing and resuming a run
order: 110
audience: user
---

# Pausing and resuming a run

A run's owner can park it on demand — not because anything went wrong, just
because it's an inconvenient time to keep spending — and pick it back up
later exactly where it left off. Nothing runs and nothing is spent while it
waits.

## Requesting a pause

From the run view's **‖ Pause ▾** menu (beside **Stop run**), or
`uzi run pause <id>`, with two modes:

- **After current milestone** (the default). The run finishes the milestone
  — or the turn, on a run with no frozen milestone list — it's already in,
  pushes a checkpoint, then parks. The run stays `running` until then: the
  page shows an info chip, **Pause requested · after M4** (or **· after the
  current step**), with **Cancel pause** and **Pause now** beside it.
- **Pause now** (`--now`). Drops the turn in flight and parks on the last
  checkpoint right away. This is the one that costs something: **work since
  that checkpoint, plus the turn in flight, is discarded** — the menu names
  the checkpoint's age (*"Work since the last checkpoint (9m ago) is
  discarded"*) so you know what you're giving up before you choose it.

A pending request is a flag on the still-running run, not a state of its
own — withdraw it any time before the boundary with **Cancel pause** (CLI:
`uzi run pause <id> --cancel`), or escalate it to `--now`. None of this is
synchronous: the park lands on the worker's next report, so watch the run
page or `uzi run get <id>`.

**Only these kinds can be paused**: issue, non-interactive task, prompt, and
self-improve runs. A chat run already parks between turns, an interactive
task parks after every turn, and judge/MR-rework/CI-fix runs are short and
finish on their own — pausing any of those, or a run already sitting at a
gate or an involuntary park, is refused (409) with the reason. Owner-only.

## What's durable, and where

Parking works like the checkpoint the run already keeps pushing during
normal work: the worker publishes its checkpoint to the forge *before*
reporting itself paused, so the parked run's progress lives on the branch,
not just on one worker's disk. The run's session and its HOME (skills, git
credentials, in-progress state) stay on the worker that was running it,
freeing its slot for other work in the meantime.

**If the checkpoint can't be published, the run does not park.** It stays
`running`, the pending request is cleared, and the run's activity feed
carries the reason (*"Could not pause: the checkpoint could not be
published. The run is still running and has restarted the interrupted
step."*). There's no Slack DM for this — uzi's Slack notifier only reacts to
status changes, and a failed pause never changes the run's status — so the
activity feed is where you'll see it.

## Resuming

**Resume** on the run page, or `uzi run resume <id>`, moves the run from
`paused` back to `queued` with its worker pin kept:

- If that same worker is still alive, it picks the run back up and
  **continues the same session where it stopped — no re-planning.**
- If the worker is gone, another worker claims the run, **recovers the
  branch from the pushed checkpoint, and re-plans only the milestones that
  weren't finished yet** — the ones already done stay done, from the plan
  you already approved.

`uzi run resume-now` also resumes a paused run (it's the same endpoint that
already resumes a run held for a pooled token).

## The clock stops

Nothing runs and nothing is spent while a run is paused. The time it spends
parked is banked separately and doesn't count against its budget, so the
remaining budget on resume is exactly what it was at the moment you paused
it — unlike a usage-limit wait, whose resume gives the run a fresh full
clock.

## Not the same as a usage-limit wait

A pause (this page) is something *you* asked for; [a usage-limit
wait](run-limit-wait.md) is something that happened to the run because a
token it was spending hit its Anthropic rate limit. They look similar — both
are non-terminal parks that keep the run's session and worker affinity — but
resolve differently: a usage-limit wait resumes on its own once the window
resets and gives the run a fresh clock, while a pause only ever resumes when
you ask it to and hands back exactly the budget that was left. If you pause
a run and it then happens to hit a usage limit before reaching your
requested boundary, your pause request survives that wait and takes effect
at the first boundary once the run is running again — you don't have to ask
twice.

## The CLI

```
uzi run pause <id> [--now|--cancel]
uzi run resume <id>
```

`--now` and `--cancel` are mutually exclusive. See [uzi CLI](cli.md) for the
full command reference.

Related: [Paused on a usage limit](run-limit-wait.md) · [Run activity
pane](run-activity.md) · [CLI](cli.md)
