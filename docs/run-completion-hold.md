---
title: Completion holds and the structural interlock
order: 112
audience: user
---

# Completion holds and the structural interlock

Some issue runs carry a **structural completion interlock**: a frozen checklist
of the milestones the run's plan was approved with, checked against what the
lead actually declared complete before it can open a closing pull/merge
request. This page explains what a hold looks like when the interlock catches
something, and what you can do about it.

This is **on by default for a new, unseeded issue run using the Claude
harness**. An admin can turn it off from **Admin → Instance settings →
Completion check** (`completion_interlock_rollout`); an explicit off is the only way to
disable it. A **Codex** issue run is not checked yet — the Codex executor
doesn't run the completion-attempt loop this page describes, so the switch
has no effect on one, regardless of its setting. A run started from a
**seeded plan** (`uzi run create --plan-file`) is never checked either: it
never goes through the plan-bearing report this interlock hooks into, so it
is never interlocked in the first place. And a run created before this
default changed is unaffected either way: the switch is read once, when the
run is created, never retroactively against a run already in flight.

This default also gates who can pick up the run: a worker must advertise the
`completion_interlock_v1` protocol capability to claim a stamped run at all.
If none of the run owner's online workers does, the run stays queued, and its
health reason names it directly: "no online worker implements the completion
interlock (completion_interlock_v1); provision a capable worker". Upgrade the
owner's workers to this release to clear it (a worker from v0.83.0 onward can
claim the run, but an older one than this release may still pause it at
finalize; see the CHANGELOG). Turning Completion check off does **not**
release a run already queued this way, since the switch is only read when a
run is created: the stuck run needs a capable worker, or cancel it and create
it again.

## Why a run holds

An interlocked run signals done only after every milestone in its frozen plan
is declared complete. If it signals done with milestones still missing, the
run does not immediately fail and does not open a pull/merge request either:

1. The lead checkpoints its work, then gets the missing milestone(s) named
   back to it **in the same session** — no re-planning, no new run. Most runs
   resolve here on the next attempt.
2. If several attempts in a row make no real progress on the same missing set,
   the run asks you directly: a **Completion blocked** question naming what's
   still missing, with a window to answer before it parks.
3. If that window passes with no answer, the run parks in a **completion
   hold** — a `paused` run, but held for this structural reason rather than
   because you asked it to pause.

A completion hold never opens a pull/merge request for an incomplete run. It
also never discards a run's work to get there: the worker verifies its Git
work is safely captured before it parks, and if that verification can't be
confirmed, the run stays live and keeps trying rather than parking on
uncertain ground.

## What you'll see

The run view and `uzi run get`/`uzi run list --json` distinguish three states
for an interlocked run, all server-computed so the web UI and the CLI never
disagree about which one applies:

- **Checking completion** — the run is running, has made at least one
  completion attempt, and nothing is currently unmet (it's re-verifying before
  finalizing).
- **Reworking unmet milestones** — the run is running, has made at least one
  attempt, and named milestones are still unmet. The run page shows the
  bounded list of unmet milestone IDs and the attempt count.
- **Completion blocked** — the run is either asking you the completion
  question live, or has parked in the completion hold. Same label either way,
  since both mean the same thing: the run needs a decision.

None of these panels show raw model output, repository text, credentials, or
arbitrary logs — only the bounded unmet-milestone list, the attempt count, and
the hold reason.

## Answering the completion question

When a run asks its **Completion blocked** question, you have a fixed window
— the instance's `completion_hold_window_seconds`, defaulting to **900
seconds (15 minutes)** — to answer while the lead session is still alive and
can act on your answer immediately, with no restart. If the window elapses
with no answer, the run parks in the hold instead; it never fails outright
for this reason.

## Deciding on a held run

The owner has three decisions, all through `uzi run decide <id>` (exactly one
is required):

**Continue** — resume past the completion check unchanged:

```
uzi run decide <id> --continue [--guidance "<text>"]
```

- Answered inside the live question window, this resumes the run in place —
  no new claim, no re-planning.
- Answered after the run has already parked in the hold, this resumes the run
  through `queued` like [an ordinary resume](run-pause.md#resuming), carrying
  your guidance to the run's next session as a follow-up.

Guidance is optional: `uzi run decide <id> --continue` with no `--guidance`
simply tells the run to carry on past the check.

**Partial** — reduce scope by naming the exact milestone ids to keep, with a
required reason:

```
uzi run decide <id> --partial m1,m2 --reason "<why>"
```

The run delivers only the kept milestones; the rest are deferred. Its closing
pull/merge request does **not** close the issue and lists the deferred
milestone ids, so the remaining work stays visible for a later run.

**Accept** — waive the exact unmet criteria by id, with a required reason:

```
uzi run decide <id> --accept m2.c1,m2.c3 --reason "<why>"
```

The named criteria are accepted as met and the run continues to finalize. A
closing pull/merge request then names the accepted criteria in a warning
block, so a reviewer sees exactly what was waived and why.

Partial and accept each require `--reason` and send the run's current contract
revision, which the server fences: if someone has already recorded a decision
(the revision moved), your command is refused with a conflict rather than
silently overwriting theirs. `--guidance` is valid only with `--continue`.

## The honest limitation: same-worker-only context

**Be aware that a completion hold's conversation continuity is not durable
across workers.** The worker verifies and keeps the run's Git work safely
captured regardless of which worker eventually resumes it — that part is
never at risk. But the run's actual provider session (the live conversation
state a same-worker resume continues without re-planning) survives only as
long as the same worker that held it is still around to reclaim it, bounded
by the same worker-affinity ceiling that already governs every other parked
run (2 hours by default; see
[ARCHITECTURE.md's Run lifecycle](../ARCHITECTURE.md#run-lifecycle)). A hold
that outlives that ceiling, or whose worker is gone, still resumes — but as a
fresh session against the recovered Git work, the same as any cross-worker
resume, not a continued conversation.

The run page and `uzi run get --json` surface this plainly as
`hold_context: unavailable(same_worker_only)` rather than implying the hold
is durably recoverable everywhere. Durable cross-worker conversation context
is a separate, later piece of work — this page will be updated when it ships.

## Not the same as an owner pause

A completion hold and [an owner-requested pause](run-pause.md) both leave a
run sitting in `paused`, but for a different reason. An owner pause is
something you asked for; a completion hold is something the run's own
structural check produced after it couldn't get anywhere on a missing
milestone. The **live** completion question (before the run has actually
parked) is not a `paused` run at all, so the ordinary **Resume** control
doesn't apply to it — only `uzi run decide <id>` (continue, partial or accept)
answers it. Once a run has parked in the hold, plain `uzi run resume` will
technically move it back to `queued` too, but `uzi run decide <id>` is the
right tool: it's the one path that can carry a decision — guidance on a
continue, or a scope change on a partial/accept — to the resumed run and that
records it against the hold it's answering.

Related: [Pausing and resuming a run](run-pause.md) ·
[Autopilot](autopilot.md) · [CLI](cli.md)
