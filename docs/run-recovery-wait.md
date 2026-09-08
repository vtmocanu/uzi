---
title: Recovering from an empty turn
order: 43
audience: user
---

# Recovering from an empty turn

Every so often, a resumed model turn comes back with nothing at all — zero
turns, no model activity. Rather than fail the run outright, uzi retries it a
few times in place and, if it's still coming back empty, parks the run in
`recovery_wait` and keeps trying on its own until the model starts responding
again — no re-running by hand.

## On by default, nothing to turn on

This is not a setting. Every run gets the bounded in-process retries and,
should those exhaust, the `recovery_wait` park — there is no opt-out and
nothing to configure per run.

## What you'll see

- **Board card / run status**: a **recovery wait** badge.
- **Run view**: a warn-toned strip reading *"Recovering and resuming
  automatically"*, explaining the run paused on a transient empty model
  result and resumes on its own — no countdown, because the retry instant is
  a server-owned backoff, not a fixed reset time.
- **Activity feed**: a status line each time an in-process retry fires, before
  the park is even reached.

The run's status stays visibly "recovering", never "stalled", and it is never
killed by its own run timeout while parked.

## What happens automatically

Once the bounded in-process retries are exhausted on a still-empty turn, the
worker best-effort captures the run's local restore point, then reports the
`recovery_wait` park. The server backs off on a **capped exponential
schedule** — doubling from a short base, capped at a ceiling — and promotes
the run back to `queued` once that backoff elapses. It resumes on the same
worker, in the same session, keeping its branch and its history; if it comes
back on a different worker, it recovers from its last durable checkpoint
instead. If the plan was already approved, it goes straight back to
implementing rather than asking you to approve it again.

**There is no lifetime cap on this park.** Unlike a usage-limit pause, a run
can park and resume in `recovery_wait` as many times as it takes — it keeps
backing off and retrying until the model starts responding again, or you
cancel it. Nothing is lost while it waits: the same durable-checkpoint
protections that cover every other park apply here too.

## If it never recovers

A `recovery_wait` run doesn't fail on its own — it holds, waiting, for as
long as the model keeps returning empty. If you'd rather stop waiting,
**cancel** it, the same way as any other run.

## Not the same as waiting for a usage limit or a pooled token

`recovery_wait`, `limit_wait` and `pool_wait` are all non-terminal parks that
auto-resume, but each is triggered — and cleared — by something different:

- **`recovery_wait`** (this page) means the model itself returned nothing for
  a resumed turn, after bounded in-process retries. It clears on a
  server-owned capped backoff, tried again and again until the model
  responds.
- **`limit_wait`** means a token you were actually spending hit its
  Anthropic rate limit. It clears when that window resets — see [Paused on a
  usage limit](run-limit-wait.md).
- **`pool_wait`** means an `auto`-lane worker's token pool was genuinely
  empty — there was nothing to spend at all. It clears when you opt a token
  into the pool, or on demand with `uzi run resume-now` — see [Letting uzi
  pick the token (auto-selection)](anthropic-token.md#letting-uzi-pick-the-token-auto-selection).

Related: [Paused on a usage limit](run-limit-wait.md) · [Anthropic tokens](anthropic-token.md) · [Run health](run-health.md) · [CLI: Run status, and what `--follow` waits for](cli.md#run-status-and-what---follow-waits-for)
