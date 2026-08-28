---
title: Paused on a usage limit
order: 38
audience: user
---

# Paused on a usage limit

When a run hits your Anthropic 5-hour or 7-day usage cap partway through, uzi can pause it instead of failing it outright, and pick it back up on its own once the window resets — no re-running by hand.

## On by default

Every new run waits out a usage limit unless you turn it off, in two places:

- **Settings → Anthropic usage limits** — *"Pause my new runs on a usage limit instead of failing them"* sets the default for every new run, and is the **only** way to change it for an autopilot, CI-fix, or self-improvement run: none of those has a start button of its own.
- **The run view** — *"Wait out future Anthropic usage limits on this run"* overrides the default for one run, live, for as long as it's still going.

Starting a run by hand stays one click and inherits the Settings default; there's no checkbox at start. The API accepts the flag per run for scripted callers.

**The run-view toggle does not stop a paused run** — it only decides what happens the *next* time this run hits a limit, and leaves an already-paused run exactly as paused. To actually stop one, **cancel** it, the same way as any other run: that works right away, without waiting for the window to reset.

## What you'll see

- **Board card**: a **limit wait** badge.
- **Run view**: a warn-toned strip reading *"Paused on an Anthropic usage limit"*, with a countdown to when it resumes, which window it hit (5-hour, 7-day, …), and — from the second pause on — *"attempt N"*.
- **Activity feed**: *"Anthropic usage limit reached — paused until it resets"*, with the window and reset time alongside it.

The run's status stays visibly "waiting" the whole time — never "stalled" — and it is never killed by its own run timeout while paused.

## What happens automatically

Once the window resets, the run resumes on the same worker, in the same session, keeping its branch and its history — including any edits it hadn't committed yet when it paused, which come back exactly as they were and never show up as their own commit in the eventual merge request. If you had already approved its plan before the pause, it goes straight back to work instead of asking you to approve the plan again.

That's the trade: a parked run keeps holding its issue and its worker's disk for as long as it waits, which is the price of never losing work to a mid-run limit.

The pause itself also updates that credential's rate-limit meter right away, so it reads as exhausted for anyone else's claim too — see [Claude rate limits](rate-limits.md) for what that looks like. And the resuming claim goes further: it skips the credential that just paused it even when the meter alone wouldn't have ruled it out, which is what keeps an `auto` worker from immediately picking the very token that just refused it.

A run can pause and resume more than once if the limit keeps recurring, backing off between attempts, up to a cap; a second cap bounds how far out any single pause may reach. Both are operator-configured — see [Configuration](configuration.md) to change them.

## If a run isn't waiting out limits

A run that isn't set up to wait still fails the moment it hits a limit, the same as before — but the failure now says why, instead of a bare error: *"Anthropic usage limit (5-hour) reached; resets at 2026-07-28T02:00:00Z"*. Re-run it once that time passes, or turn on waiting so next time it doesn't have to.

Related: [Claude rate limits](rate-limits.md) · [Run health](run-health.md) · [Configuration](configuration.md)
