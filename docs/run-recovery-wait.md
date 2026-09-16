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

## Forge unreachable at clone

A `recovery_wait` park can also come from a different cause: the forge
(GitHub, GitLab, Forgejo) was unreachable when the worker tried to clone or
fetch your repo — a DNS blip, a dropped connection, or a transient 5xx.
Unlike the empty-turn case above, nothing has been cloned yet, so there is
no checkpoint to capture; the run just parks and retries.

The run's badge and feed say **waiting for the forge, retry at HH:MM (N of
MAX)**, naming the next retry time and how many of this run's lifetime
forge parks it has used. It auto-resumes on the same capped backoff as an
empty-turn park (`RUN_RECOVERY_PARK_BASE` up to `RUN_RECOVERY_MAX_PARK`) —
no action needed while `N` stays under `MAX`.

Unlike an empty-turn park, this one **does** have a lifetime cap:
`RUN_FORGE_UNREACHABLE_MAX_PARKS` (default 6). If the forge is still
unreachable past that many parks, the run fails instead of parking again,
so a genuinely dead forge does not hold the run open forever. A permanent
forge error (401/403/404 — bad credentials or a deleted repo) fails the run
immediately instead of parking, since retrying would not help. Cancelling
the run at any point during these retries ends it `cancelled`, as usual.

## Other waiting states

- `recovery_wait`: a positively empty SDK result persisted through bounded
  retries, or the forge was unreachable at clone/fetch (see
  [Forge unreachable at clone](#forge-unreachable-at-clone) above). The
  server retries automatically after a backoff.
- `limit_wait`: a usage-limit window must reset. See
  [Paused on a usage limit](run-limit-wait.md).
- `pool_wait`: no token is available in the selected pool. It needs an
  eligible token or an explicit resume attempt, not a recovery timer. See
  [Waiting for a token](anthropic-token.md#waiting-for-a-token).
- `paused`: an owner-requested hold that requires the owner to resume it.
  See [Pausing and resuming a run](run-pause.md).

Related: [Run health](run-health.md) and
[CLI run status](cli.md#run-status-and-what---follow-waits-for).
