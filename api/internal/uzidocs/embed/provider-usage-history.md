---
title: Provider usage-limit history
order: 70
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

Each row holds the accepted reading's timestamp, the credential revision it was read under, and
only numbers, flags, window lengths and reset times. Raw provider bodies, credentials and
signatures are never copied.

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
- vault operations that hold the owner's `users` row lock. The copy's foreign key check waits on
  that lock, so while one is running the copy can hit its 250 ms deadline and leave a gap.

## What a copy checks

A copy is one autocommit statement that re-reads the gauge row in the same snapshot as the
insert, and inserts nothing (a gap) unless all of these still hold:

- The gauge row is exactly the reading the caller wrote: same owner and credential, same
  timestamp, source and values. A later accepted poll, or a worker exhaustion mark (which keeps the
  timestamp but changes the source and percentage), therefore never becomes a sample.
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
skipped; the normalizer's maximum (17 buckets, 99-byte ids, both windows) is always skipped.

When a window reports only a relative reset (`reset_after_seconds`), the stored reset is the
success time plus that offset, flagged approximate (`rd` true in the projection).

## Retention and removal

There is no age-based expiry and no sweeper: history is kept until the owner is deleted, which
cascades both tables away. Deleting or rotating a credential keeps its history, because the
identity column has no foreign key. Measured on one identity with a year of rows (105,120),
deleting the owner as one statement took 0.14 s (Anthropic), 0.27 s (typical Codex) and 0.80 s
(max Codex) right after a checkpoint, writing 21 to 213 MB of WAL (mostly full-page images), and
about 0.1 to 0.25 s with about 6 MB of WAL otherwise.

Rolling the migration back (the Down section of `00536_provider_usage_samples.sql`) drops both
tables and their history. Take a database backup first; it is the only recovery.

## Growth

Measured on PostgreSQL 17 (heap plus primary-key index, as grown; one run each on a shared
host, so indications). A full-cadence identity writes at most 105,120 rows a year, so these are
upper bounds:

| Profile | Bytes per row | 1 identity / year | 10 identities | 100 identities |
|---|---|---|---|---|
| Anthropic token | 218 | 22.9 MB | 229 MB | 2.29 GB |
| Codex account, typical (2 buckets) | 666 | 70.0 MB | 700 MB | 7.0 GB |
| Codex account, largest admissible (17 buckets) | 2,128 | 224 MB | 2.24 GB | 22.4 GB |

Bytes per row are as grown through the real copy path with many identities interleaved; a single
identity loaded in time order packs a little tighter (201, 645 and 2,108 B). No row needed TOAST.
The table has one index (the primary key), about 70 to 75% full as grown (90% when loaded in
time order). WAL per copy is about 550 to 600 B (Anthropic), 850 to 900 B (typical Codex) and
2.3 to 2.4 KB (max Codex) on average, including occasional full-page images; a same-bucket no-op
writes none.

## Copy cost

- One extra statement per accepted reading: about 0.7 ms (Anthropic) and 0.9 ms (Codex) at p50
  and 2 to 4 ms at p99 on an idle database, roughly doubling the database time of the gauge
  write, against a poll dominated by the outbound provider call. A same-bucket no-op costs about
  0.2 to 0.4 ms.
- Each copy runs under a 250 ms deadline that covers pool acquisition and execution. On the test
  connection path the copy runs after the verdict is final and can add up to 250 ms to that
  response, without changing it.
- A timed-out copy may still have committed: the statement can finish on the server near the
  deadline, or after the client gave up. A deadline error does not prove there is no row.
- Under heavy lock contention (many owners' `users` rows locked at once) every timed-out copy
  makes the pool discard its connection, so the pool opens a replacement for each. Measured with
  100 concurrent copies on a 20-connection pool, 4,000 copies per pass: with no lock contention,
  none of 8,000 copies per provider timed out (4,000 inserts, then 4,000 same-bucket no-ops).
  With 5% of owners locked, all 200 blocked copies timed out, 3,800 inserted, and the pool opened
  about 185 new connections. With half locked, about 1,800 inserted, the 2,000 blocked copies
  and about 170 to 200 unblocked ones (queued for a connection) timed out, the pool opened about
  1,900 new connections, and 20 to 38 of those unblocked timed-out copies had in fact committed.
- A failed, skipped or timed-out copy logs a fixed warning with a category (`timeout`, `error`
  or `skipped`) and the row id, never the error text, and never changes the gauge, the
  notifications or the poll's backoff.
