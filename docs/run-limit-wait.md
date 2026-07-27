---
title: Paused on a usage limit
order: 38
audience: user
---

# Paused on a usage limit

When a run hits your Anthropic 5-hour or 7-day usage cap partway through, uzi
can pause it instead of failing it outright, and pick it back up on its own
once the window resets — no re-running by hand.

## What you'll see

- **Board card**: a **limit wait** badge.
- **Run view**: a warn-toned strip reading *"Paused on an Anthropic usage
  limit"*, with a countdown to when it resumes, which window it hit (5-hour,
  7-day, …), and — from the second pause on — *"attempt N"*.
- **Activity feed**: *"Anthropic usage limit reached — paused until it
  resets"*, with the window and reset time alongside it.

The run's status stays visibly "waiting" the whole time. It never reads as
"stalled", and it is never killed by its own run timeout while paused.

## What happens automatically

Once the window resets, the run resumes on the same worker, in the same
session — it picks up exactly where the agent left off. If you had already
approved its plan before the pause, it goes straight back to work instead of
asking you to approve the plan again.

A run can pause and resume more than once if it keeps hitting the limit.
There's a cap on how many times one run may pause before it's failed
instead, and a cap on how far out any single pause may reach — both are
operator-configured; see [Configuration](configuration.md) if you need to
know or change them for your instance.

## Cancelling a paused run

Cancel it the same way as any other run. It stops right away — it does not
wait for the window to reset first.

## If a run isn't waiting out limits

A run that isn't set up to wait still fails the moment it hits a limit, the
same as before — but the failure now says why, instead of showing a bare
error: *"Anthropic usage limit (5-hour) reached; resets at
2026-07-28T02:00:00Z"*. Re-run it once that time passes.

Related: [Claude rate limits](rate-limits.md) · [Run health](run-health.md) ·
[Configuration](configuration.md)
