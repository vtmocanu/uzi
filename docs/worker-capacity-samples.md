---
title: Worker capacity samples
order: 70
audience: operator
---

# Worker capacity samples

Every five minutes the API records one capacity sample per worker in the
`worker_capacity_samples` table. The history is for later fleet-utilization
reporting; there is no read API, UI or CLI for it yet.

## What a sample holds

| Column | Meaning |
|---|---|
| `worker_id`, `bucket_start` | The worker's UUID (kept by value, no foreign key to `workers`) and the start of its five-minute bucket. Together they are the primary key. |
| `user_id` | The worker's owner when the sample was taken. |
| `observed_at` | When the sample was taken (see [Observation instant](#observation-instant)). |
| `freshness`, `last_heartbeat_at` | `online`, `stale` or `offline`, and the worker's last heartbeat as recorded. |
| `worker_kind`, `ephemeral`, `isolated_lane` | The worker's kind, whether it is run-bound (ephemeral), and whether it is an isolated-lane worker. |
| `max_concurrent_runs`, `max_cross_check_slots` | The worker's configured caps, copied as they were. A cap that is not set stays empty (unknown), never zero. |
| `attribution` | `owner` when the run counts below are real, `unavailable` when they could not be shown (see [Attribution](#attribution-fails-closed)). |
| `ordinary_active`, `cross_check_lane_active`, `chat_active` | Runs holding a slot on the worker, by lane. Empty when attribution is `unavailable`. |

Counts are never clamped to the caps, so a worker over its cap shows the real
number.

## Freshness

Freshness uses the same staleness rule as marking workers offline, with the
`WORKER_HEARTBEAT_STALE` window (see [Configuration](./configuration.md)):

- `online`: the worker is online and its last heartbeat is no older than the window.
- `stale`: the worker is still marked online but has no heartbeat, or an older one.
- `offline`: the worker is marked offline.

Stale and offline caps are recorded, but they are not online capacity: sum only
`online` rows when you want what the fleet could run.

## Observation instant

`observed_at` is the database statement time, not the start of any enclosing
transaction. The bucket is the five-minute interval containing it, aligned to a
fixed origin so buckets are the same on every replica.

## Lanes

- Ordinary: any run that is not chat and not on the cross-check lane. A
  cross-check running on an ordinary slot counts as ordinary.
- Cross-check lane: runs on the cross-check lane.
- Chat: chat runs, counted separately.

Only runs that hold a slot count (claimed, running, awaiting approval, awaiting
input or awaiting follow-up). `pool_wait`, `recovery_wait` and affinity-only
queued rows hold no slot and are not counted.

## Attribution fails closed

Counts are recorded only when every slot-holding run on the worker belongs to
the worker's owner and has an audited run kind and lane. Otherwise the sample
has `attribution = unavailable` and no counts. Caps, freshness and heartbeat
are always recorded.

## Gaps and the at-most-one rule

A worker has at most one sample per five-minute bucket. A worker the pass does
not reach in a bucket is a gap, not a zero. While an owner's account row is
locked (for example during an account reset or owner deletion), batches that
contain that owner's workers fail, and workers later in id order are gaps for
those ticks. A failed batch can be retried in the same bucket without
duplicating samples.

## Retention

There is no age-based expiry. Samples are kept until their owner is deleted,
which removes them (cascade). Deleting a worker, including an ephemeral worker
when its run ends, keeps its samples. A configurable, off-by-default deletion
mechanism is a later issue (#4693).

## Growth

| Quantity | Value |
|---|---|
| Bytes per row (table plus indexes) | TBD-MEASURED |
| Rows per worker-year (105,120) | TBD-MEASURED |
| 50 workers, per year | TBD-MEASURED |
| 500 workers, per year | TBD-MEASURED |
| Owner-delete cost | TBD-MEASURED |

## How the pass runs

The pass runs on the existing sweeper tick, once per tick, in up to 4 batches of
1,000 workers, with a 2 second overall deadline. A session advisory lock keeps
overlapping API replicas from running it at once; a replica that does not get
the lock does nothing that tick. Each batch is one statement with a server
statement timeout of 1.75 s and a lock timeout of 300 ms. The pass ends at the
first failed batch; remaining workers are sampled on a later tick.
