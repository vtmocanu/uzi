---
title: Recovering from a transient interruption
order: 111
audience: user
---

# Recovering from a transient interruption

Uzi retries in place and, if the trouble persists, parks the run in
`recovery_wait` on two kinds of transient interruption:

- A **positively empty SDK turn** — a turn that finishes with a reported
  turn count of zero and no model activity, plan, question, or completion.
- A **transient provider error** — the Anthropic API returning 429, 500,
  502, 503, or 529 (or a status-less transport error).

Uzi retries the interruption a few times in place. If it does not clear,
uzi saves a verified local recovery checkpoint before parking the run in
`recovery_wait`.

This recovery is automatic and requires no per-run setting. A missing plan
after a turn that actually did work, missing turn-count metadata, a real
timeout, and cancellation are not classified as a transient recovery.

A **permanent** provider error — for example 401, 403, or 400 (bad
credentials or a bad request) — is not a transient interruption. The run
fails fast with an accurate reason and does not park here.

One empty result is routed elsewhere on purpose: if the turn came back empty
**because that attempt hit a hard usage limit** (its final rate-limit verdict
was a rejection), uzi treats it as a usage limit rather than retrying here. It
enters [`limit_wait`](run-limit-wait.md) and respects your `wait_on_limit`
setting — so it parks for the reset only if you opted into limit waiting, and
otherwise fails fast instead of cycling through recovery. An empty turn from
any other cause, or one where a later signal in the same turn cleared the
limit, still parks here as `recovery_wait`.

## What you'll see

- A **recovery wait** badge on the run.
- A run-view strip reading **Recovering and resuming automatically**.
- Activity-feed messages for in-process retries and the checkpoint saved
  before the park. The feed distinguishes a checkpoint saved on this worker
  from one successfully published for recovery on another worker.

If local capture cannot be verified, the run remains active while the
worker keeps its local work and session and retries capture. The feed says
so explicitly. It does not enter an automatically resumable park with only
an unverified capture attempt.

## What happens automatically

After verified capture, the server parks the run on a capped exponential
backoff and returns it to `queued` when that delay passes. There is no
lifetime cap on recovery parks and no manual resume step.

A same-worker resume can recover from the verified local checkpoint and
retained SDK session. Publishing a checkpoint to the forge is best-effort:
a different worker can recover only what was successfully published.
Losing the original worker's storage before publication can therefore
still lose newer work. Recovery does not promise protection against loss
of every copy.

If capture fails during shutdown, the worker retains the source clone and
session. A worker-owned ownership record prevents a later claim from
deleting that clone before recovering it. This protection requires the
worker's persistent storage to survive.

A run whose approved plan and work are available continues implementation
without requesting the same approval again. If a human-approved run loses
its work entirely, the existing plan-review safeguard still applies.

This fix does not reconstruct plan text missing from older runs. A legacy
auto-approved run whose stored plan is empty can still need a new planning
turn; an approval flag alone is not an implementation-ready plan.

## If it never recovers

The recovery park itself has no timeout or lifetime-attempt failure. You
can **cancel** the run at any time. Normal watchdogs still apply while it
is running, including while it retries capture; recovery does not override
a genuine timeout or cancellation.

## Other waiting states

- `recovery_wait`: a positively-empty SDK turn OR a transient provider error
  persisted through bounded retries. The server retries automatically after a
  backoff.
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an owner-requested hold that requires the owner to resume it.
  See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
