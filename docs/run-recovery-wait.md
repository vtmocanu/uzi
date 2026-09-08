---
title: Recovering from an empty turn
order: 111
audience: user
---

# Recovering from an empty turn

Sometimes an SDK turn finishes with a reported turn count of zero and no
model activity, plan, question, or completion. Uzi retries this specific
empty result a few times in place. If it stays empty, uzi saves a verified
local recovery checkpoint before parking the run in `recovery_wait`.

This recovery is automatic and requires no per-run setting. A missing plan
after a turn that actually did work, missing turn-count metadata, a real
timeout, and cancellation are not classified as an empty-result recovery.

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

- `recovery_wait`: a positively empty SDK result persisted through bounded
  retries. The server retries automatically after a backoff.
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an owner-requested hold that requires the owner to resume it.
  See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
