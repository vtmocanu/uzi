---
title: Instance maintenance mode
order: 56
audience: operator
---

# Instance maintenance mode

Maintenance mode lets an admin stop uzi from starting new work while the work
already in progress finishes, so nodes can be drained or the database upgraded
without losing runs ([PRD #3454](../prds/done/3454-instance-maintenance-mode.md)).
New runs are accepted and wait in the queue; they are never rejected.

## When to use it

Use it before infrastructure work that evicts pods or takes the database down:
a Kubernetes node drain, a database upgrade, a storage move. It is instance
wide and needs an admin. The per-worker [manual
cordon](hosted-workers.md#pause-new-work-with-a-manual-cordon) is a different
tool: it closes intake on one persistent worker, for that worker's owner only,
and never covers ephemeral workers.

It is not a freeze of the stack. The controller keeps rolling workers, running
DinD pressure maintenance and reaping, and persistent workers stay as they are.

## Turning it on and off

Open **Admin → Instance → Maintenance mode**, set an optional reason
(at most 280 characters), and switch it on. The reason is shown to every user,
so write it for them: control characters are stripped and the text is capped,
but it is never rejected for content. Saving again while it is on updates the
reason and keeps the start time.

The web UI is the only way to change it. `PUT /api/admin/maintenance` accepts a
cookie session with a CSRF token only; a Bearer token (including an admin
token) is refused, and the CLI has no on/off command. Every change is recorded
with who and when. Switching it on records the reason; the reason is cleared
when maintenance is lifted, so the "off" event has none.

**Enable it only after an api rollout has completed.** During a rolling update
the old replicas do not have the check and keep starting new runs until they are
replaced. Switching it off is always safe.

Turning it off lets the queue start, bounded by the existing worker slots.
Nothing is lost or re-created. Held runs restart their "stuck for" clock, and
no burst of Slack nudges is sent for the backlog.

## What happens to work

| Work | While maintenance is on |
|---|---|
| A run never claimed before (`claim_generation` 0), of any kind: issue, prompt, sweep, schedule, chat, chat Continue, judge, CI-fix, MR rework, task review and then-fix phases, `/api/v1` job, recovery successor, Slack-created runs and chats | Created as usual and stays `queued`. No worker claims it and no ephemeral pod is started for it. |
| A run claimed at least once | Keeps going: finishes, resumes after approval, input, follow-up, limit, pool, recovery or wall-clock waits, and may be requeued and re-claimed by any worker that accepts it, an ephemeral replacement included. |
| A cross-check child | Counts as admitted: it can only be claimed while its lead is claimed or running (or, for a code check, an interactive task lead at `awaiting_followup`). |
| Persistent workers | Unchanged. They simply find nothing new to claim. |

A `--review --then-fix` task handoff stops between its phases until
maintenance is lifted, because each phase is a new run. A repeat schedule fire
skips as `already_running` while its earlier run is still queued, so schedules
do not stack up.

Per-worker cordon, drain, maintenance fences and capability rules still apply
to admitted work as before.

## What users see

- Every signed-in user sees a banner with the reason.
- A queued run held by maintenance reads "waiting: uzi is in maintenance mode
  (<reason>)" on its board card, run lists and run view. `uzi run get` shows a
  `QUEUE_REASON` row.
- `/api/v1` jobs carry an optional `queue_reason: "maintenance"`. The status
  stays `queued`, and the job's wall-clock limit counts from claim, so waiting
  does not use it up.
- Creating a run (web, CLI, chat, `/api/v1`) succeeds as usual with an added
  `notice`. It reads neutrally: new work is queued and starts when maintenance
  ends. The CLI prints it as a warning on stderr, including `uzi run rework`
  and `uzi ci fix`. Slack replies that open or continue a chat, the "Start run"
  card reply, and the refusal of a second message while a queued chat is
  waiting also mention maintenance.

## Reading the readout

The Maintenance card and `uzi admin maintenance status` show the same snapshot:

| Category | What it counts | Blocks "clear" |
|---|---|---|
| `active` | Runs `claimed` or `running`. A live chat counts until it idles out. | yes |
| `held` | `awaiting_approval`, `awaiting_input`, `awaiting_followup`, `paused`, `limit_wait`, `recovery_wait`, `pool_wait`, and queued runs that are admitted | yes |
| `custody` | Every open recovery custody hold, including holds on finished runs | yes |
| `queued` | Runs held back by maintenance (not admitted) | no, informational |
| idle bound ephemeral workers | Ephemeral workers bound to a not-admitted run | no, informational |

Each held and custody item is tagged `progressing` or `needs_owner`, with its
owner and how long it has been open. `needs_owner` covers an approval without
auto-approve, `awaiting_input`, `paused`, a `recovery_wait` caused by
`worker_requeue_exhausted`, `worker_memory_pressure`,
`retained_recovery_adoption_failed`, or owner-held `retained_recovery_exhausted`
(NULL retry), and a custody hold that
needs an owner decision. Each list shows up to 50 items; the counts are exact.

**Clear** means active, held and custody are all zero in one snapshot, shown
with its `as_of` time. Open custody blocks even when a capture exists, because a
capture does not prove it holds the latest work (see [Run
recovery](run-recovery.md)); held work on an ephemeral worker is not
eviction-safe because the worker's volume goes with it.

Two cautions:

- **Clear can need an owner.** An admin cannot dispose of another user's holds.
  Contact the owner the readout names (approve, answer, resume, or decide the
  custody hold), or accept that the item stays.
- **Clear is "no blockers visible", not a race-free authorization.** A claim
  that was already in flight when you enabled maintenance can still commit and
  appear afterwards. Refresh the readout once more right before evicting nodes.

## Ephemeral and hosted workers

- The ephemeral provisioner starts no pod for a run that is not admitted, and
  still provisions replacements for admitted runs. A pod that is a user-created
  persistent hosted worker is still created, but it holds no new work and is
  safe to evict.
- An ephemeral worker bound before you enabled maintenance to a run that is not
  admitted is retained. It claims nothing, is safe to evict, is listed as an
  idle bound worker, and claims its run after the lift.
- A pre-enable ephemeral pod that never registered is still reaped at its
  deadline and is not re-provisioned while maintenance is on.
- Edge case for `/api/v1` jobs: a job whose ephemeral worker was requested
  before maintenance and never registered can still be failed with
  `ephemeral_worker_never_registered`. This is the existing never-registered
  check for jobs, which maintenance does not suspend.

## Health check

[`maintenance.active`](admin-health.md#the-checks) (instance scope,
housekeeping) is `warn` while maintenance is on and carries the start time,
reason and readout counts, so it cannot be forgotten. It is `unknown` when the
state cannot be read. Liveness and readiness are unaffected. If the state row is
missing, uzi treats the instance as paused (new work waits) and the check warns;
a database read error fails a claim attempt closed and it retries.

## From the CLI

```sh
uzi admin maintenance status [--json]
```

Read-only; needs an admin token or session. Prints the state and the readout
above. See [CLI reference](cli.md).

## Not covered

The check on a real hosted k8s instance (enable, confirm no pod for a new run,
drain a node once clear, disable, confirm the queue starts) is a manual
acceptance step tracked in
[#3465](https://github.com/vtmocanu/uzi/issues/3465); it is not yet done.
Scheduled windows, auto-disable timers, and per-repo or per-user maintenance are
not provided.
