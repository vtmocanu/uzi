---
title: Provider usage-limit history
order: 72
audience: operator
---

# Provider usage-limit history

The Anthropic token and Codex account rate-limit gauges hold only the latest accepted reading.
Since [#4692](https://github.com/vtmocanu/uzi/issues/4692) every accepted reading is also
copied, best effort, into two raw history tables so that headroom over time is not lost. Nothing
reads this history yet: there is no API, page or command over it, so the only way to inspect it
is SQL on the database. This page describes what is recorded, what a gap means, and what the
tables cost.

## What is recorded

A copy runs only after a gauge write was accepted, and only for these three paths:

| Path | Table | Identity |
|---|---|---|
| Anthropic usage poller | `anthropic_usage_samples` | owner + Anthropic token |
| Anthropic token test-connection (only when the final verdict is `ok`) | `anthropic_usage_samples` | owner + Anthropic token |
| Codex usage poller | `codex_usage_samples` | owner + provider account (not the alias) |

Several Codex aliases linked to one provider account share one gauge row and one history row per
bucket. The Anthropic exhaustion marks written by workers when a run hits a limit
(`limit_report`) write no history, and neither does any other gauge write.

Each row holds the accepted reading's timestamp, the revision it was read under (the token's
enablement revision, or the Codex account's generation and credential revision), the Anthropic
source label or the Codex bucket ids, and otherwise only numbers, flags, window lengths and reset
times. Raw provider bodies, credentials and signatures are never copied.

## Five-minute buckets and gaps

Each row is keyed by owner, identity and a five-minute bucket computed by the database
(`date_bin`) from the reading's own timestamp, not from the time of the copy. The first accepted
copy in a bucket wins; later copies in the same bucket change nothing.

A missing bucket means no accepted copy, never zero use. Gaps come from:

- failed or refused polls, and gauge writes the credential fences refused;
- a copy that was fenced out, skipped by the payload policy, or timed out (see below);
- cadence drift: the default poll interval equals the bucket width (five minutes), so a poll
  that lands a little late can leave one bucket empty and fill the next without any failure.
  Treat the row counts below as an upper bound;
- operations that hold the owner's `users` row `FOR UPDATE` (vault operations, account reset,
  CLI login). The copy's foreign key check waits on that lock, so while one is running the copy
  can hit its 250 ms deadline and leave a gap. Ordinary updates of the `users` row do not block
  it.

## What a copy checks

A copy is one autocommit statement that re-reads the gauge row in the same snapshot as the
insert, and inserts nothing (a gap) unless all of these still hold:

- The gauge row is exactly the reading the caller wrote: same owner and credential, same
  timestamp, source and values. A later accepted poll, or a worker exhaustion mark (which keeps the
  timestamp but changes the source and percentage), therefore never gets copied in place of this
  reading.
- The credential is still enabled at the same enablement revision. For Codex the provider
  account must still be at the same generation and credential revision with an enabled linked
  alias, and the last poll attempt must still be that success (a later recorded failure skips
  the copy).
- The reading's timestamp is newer than one hour ago and no later than 15 minutes ahead of the
  database clock. This only guards freshness and clock skew; it is not an expiry.

One race is accepted: a credential change that commits after the copy's snapshot can still admit
a reading the gauge had already accepted.

## Codex payload policy

The Codex copy keeps a projection of the gauge's buckets (id, allowed, limit reached, and per
window used percent, window seconds, reset). The display name is dropped. A reading that breaks
the policy is skipped whole, never truncated:

- an array of at most 17 buckets, each with a unique string id of 1 to 99 bytes;
- flags are boolean or null; windows are objects or null;
- `used_percent` is a number from 0 to 100 or null. A provider value above 100 is a gap;
- window seconds are positive integers, resets non-negative integers, all at most 2^53-1;
- the stored projection is at most 4 KiB as text.

Because of the 4 KiB bound, near-maximum readings are gaps. Measured: with 17 buckets, ids longer
than about 78 to 86 bytes (depending on the window values) project to more than 4 KiB and are
skipped; the normalizer's maximum (17 buckets, 99-byte ids, both windows) is skipped with
realistic reset timestamps. With very small window and reset values the boundary moves above
99 bytes, so such a reading is kept.

When a window reports only a relative reset (`reset_after_seconds`), the stored reset is the
success time plus that offset, flagged approximate (`rd` true in the projection).

## Retention and removal

There is no age-based expiry and no sweeper: history is kept until the owner's `users` row is
deleted, which cascades both tables away. uzi itself never deletes a `users` row (an admin can
only deactivate a user, which keeps the history), so in practice the history is kept until an
operator deletes that row by hand. Deleting or rotating a credential keeps its history, because
the identity column has no foreign key.

Deleting an owner removes all their history in one transaction. Measured on one identity with a
year of rows (105,120), right after a checkpoint with the data just loaded: 0.14 s (Anthropic), 0.27 s (typical Codex) and 0.80 s
(max Codex), writing 21 to 213 MB of WAL. That is the realistic case, because pages of old
history have been checkpointed since they last changed, so each one is written as a full page
image. Only when the pages changed since the last checkpoint does it drop to about 6 MB of WAL.
The cost is expected to grow roughly in proportion to identities and years (only one
identity-year was measured), and leaves dead rows for autovacuum.

Rolling the migration back (the Down section of `00547_provider_usage_samples.sql`) drops both
tables and their history. Take a database backup first; it is the only recovery. Each
`DROP TABLE` also takes an `ACCESS EXCLUSIVE` lock on `users` (to drop the foreign-key
triggers), and the Down section sets no lock timeout, so a rollback can wait behind a long reader
of `users` and block every query on it meanwhile. Run it when the database is quiet.

## Growth

Measured on PostgreSQL 17 (heap plus primary-key index, as grown; one to three runs each on a
shared host, so indications). A full-cadence identity writes at most 105,120 rows a year, so these are
upper bounds:

| Profile | Bytes per row | 1 identity / year | 10 identities | 100 identities |
|---|---|---|---|---|
| Anthropic token | 218 | 22.9 MB | 229 MB | 2.29 GB |
| Codex account, typical (2 buckets) | 666 | 70.0 MB | 700 MB | 7.0 GB |
| Codex account, largest admissible (17 buckets) | 2,128 | 224 MB | 2.24 GB | 22.4 GB |

Bytes per row are as grown through the real copy path with many identities interleaved; a single
identity loaded in time order packs a little tighter (about 200, 644 and 2,107 B). No value was
stored out of line; the largest Codex projections are compressed inline (a 4,080-byte projection
took 1,853 B), so their real size depends on how compressible the data is, and a less
compressible admissible reading can take more than the largest row measured here.
The table has one index (the primary key), about 68 to 76% full as grown (90% when loaded in
time order).

WAL per copy depends on checkpoints. In a burst of copies across thousands of identities it
averaged 570 to 617 B (Anthropic), 873 to 902 B (typical Codex) and 2.3 to 2.4 KB (max Codex). At
production cadence, though, each identity copies once per five minutes, the same as the default
checkpoint interval, so most copies are the first change to their index page since the last
checkpoint and carry a full page image. A dedicated Anthropic experiment measured about 6.4 to
6.9 KB of WAL per copy that way (with `full_page_writes` on and `wal_compression` off), against
about 400 B without an intervening checkpoint; larger Codex rows add their own size on top. Size
WAL archiving and backups for the higher figure: about 200 MB of WAL a day for 100 Anthropic
identities, more for large Codex accounts. A same-bucket no-op writes none.

## Copy cost

- One extra statement per accepted reading: about 0.7 ms (Anthropic) and 0.9 ms (Codex) at p50
  and about 2 to 4 ms at p99 on an idle database, roughly doubling the database time of the gauge
  write, against a poll dominated by the outbound provider call. A same-bucket no-op costs about
  0.2 to 0.4 ms.
- Each copy runs under a 250 ms deadline that covers pool acquisition and execution. On the test
  connection path the copy runs after the verdict is final and can add up to 250 ms to that
  response, without changing it.
- A timed-out copy may still have committed: the statement can finish on the server near the
  deadline, or after the client gave up. A deadline error does not prove there is no row.
- The copy runs on the API's main connection pool (20 connections by default), so a blocked
  copy holds a shared connection for up to 250 ms and the queueing below affects every API
  request, not only copies.
- A copy that times out while holding a connection makes the pool discard that connection and
  send a cancel request (a second connection to the server), and the pool opens a replacement.
  A copy that times out while still waiting for a connection discards nothing.
- Measured with 100 concurrent copies on a 20-connection pool, 4,000 copies per pass:
  - with no lock contention, none of 8,000 copies per provider timed out (4,000 inserts, then
    4,000 same-bucket no-ops), though the p99 reached about 110 ms in one run;
  - with 5% of owners locked, all 200 blocked copies timed out, 3,800 inserted, and the pool
    opened about 185 new connections; on a second pass, 8 to 76 unblocked copies also timed out;
  - with half locked, about 1,800 inserted, the 2,000 blocked copies and about 170 to 200
    unblocked ones timed out, and the pool opened about 1,900 new connections. 20 to 38 of those
    unblocked timed-out copies, and one blocked one, had in fact committed.
- A failed, skipped or timed-out copy logs a fixed warning with a category (`timeout`, `error`
  or `skipped`) and the token or provider account id, never the error text, and never changes the gauge, the
  notifications or the poll's backoff.
