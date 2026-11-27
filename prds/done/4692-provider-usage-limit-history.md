# PRD #4692: Record provider usage-limit history

**Issue:** #4692.
**Status:** Done (M1 implemented and reviewed on the #4692 branch, 2026-11-25; the exact-head PR checks and the migration renumber remain landing obligations).
**Priority:** Normal.
**Blocked by:** none.
**Scope:** One additive migration for two raw sample tables, a best-effort inline copy after the existing successful gauge writes, docs/specs/tests. No summaries, retention or sweeper change. No new API route, DTO, user interface, command or service. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [x] M1 provider headroom samples

## 1. Problem

The Anthropic token and Codex account rate-limit gauges hold only the latest accepted reading; each successful poll overwrites the last one. Nothing records how headroom changed over time, and a missing observation cannot be told apart from zero use. Readings that are not recorded when they are accepted cannot be recovered later.

## 2. Outcome

Every accepted provider reading is also copied, best-effort, into a raw five-minute sample series. A five-minute bucket with no accepted sample is a visible gap, never zero use. Nothing is deleted by default (only the owner's deletion cascades); a configurable deletion mechanism, off by default, is a later issue for all analytics history. Nothing reads this history yet; it starts accumulating now so later read APIs have real data. The existing gauges, their locks and their behaviour are unchanged.

Acceptance examples:

1. A poll succeeds, later polls fail, polling resumes. The history holds the accepted readings and a gap, never an invented zero. A failed history copy never undoes the accepted gauge.
2. Two Codex aliases reference one provider account. One account sample enters each five-minute bucket.
3. An Anthropic worker-reported exhaustion mark (`MarkFiveHourExhausted`, `MarkSevenDayExhausted`, which leave `synced_at` unchanged) never appears as a new poll sample.

## 3. Out of scope

- Any read API, DTO, page or command over this history (a later issue).
- Worker and slot capacity samples (a separate issue).
- Changing provider polling frequency, credential selection, enablement fences or limit decisions.
- Additional provider requests, exact token consumption or combining quota percentages across accounts.
- Provider credentials other than the Anthropic token and Codex account gauges named here.
- A new maintenance loop or registry; workflow changes.

## 4. Modules and seams

### Copy accepted gauges after success

Existing pollers: `api/internal/usagepoller/engine.go` and `api/internal/codexusagepoller/engine.go`. Both poll every five minutes by default, with a one-minute minimum configured cadence. Codex normalization permits at most 16 additional buckets and stops a label after the first whole rune that reaches 96 bytes (so ids reach at most 99 bytes). The two success writes and their lock paths, as they are today:

- **Anthropic** `UpsertRateLimits` (`queries/anthropic_rate_limits.sql`, `:execrows`): one statement whose `current_secret` CTE updates the `user_secrets` row (clears `anthropic_rejected_at`, increments `anthropic_success_generation`), taking the secret row lock before it inserts or conflict-updates the gauge.
- **Codex** `UpsertCodexAccountRateLimits` (`queries/codex_rate_limits.sql`, `:one`): writes the account gauge with `last_success_at = now()` and `RETURNING last_success_at`, so the caller already has the exact persisted success time.

Keep each success write and lock path exactly as it is: its own autocommit statement and every owner, enablement, generation and revision fence. Do not wrap gauge writes in a new transaction or savepoint.

Only when the success write reports one written row, run a separate best-effort `INSERT ... SELECT` from that gauge row:

- The five-minute bucket key is computed in SQL with `date_bin` from the gauge's accepted observation timestamp, not the time of the copy.
- `ON CONFLICT (identity, bucket) DO NOTHING`: the first accepted copy in a bucket wins.
- Copy only safe numeric, null, reset, window and source data plus internal identity: never raw provider bodies, credentials or signatures.
- Two provider-specific raw tables avoid nullable-identity uniqueness traps: Anthropic identity is owner + token UUID; Codex is owner + provider-account UUID, independent of aliases. The sample owner is immutable.
- Anthropic: match the persisted `synced_at`, enablement revision, poll source and the null-safe reading payload; never admit `source = 'limit_report'`. Timestamp-only matching is insufficient because the exhaustion marks leave `synced_at` unchanged.
- Codex: match the `last_success_at` the success write returned, plus owner, provider account, generation, credential revision, enablement signature and the normalized reading, and require that this success is still the current attempt (a later failure recorded by `RecordCodexAccountPollFailure` can leave every success field intact, so matching those fields alone is not enough).
- If the gauge changed, was replaced or disappeared before the copy, skip it: that is a gap, not a sample.

A copy timeout, error or oversize payload must not undo the committed gauge or change notifications, baseline selection, early-reset or retry behaviour. Log only a fixed, scrubbed diagnostic. Run the copy only after the existing post-success work the poller does today (notifications included), under its own 250 ms deadline, so it can never consume the time that work needs: one extra statement per successful gauge, no extra provider request, no credential lock held by the copying caller. Exhaustion updates in `workersvc/limitwait.go` write no history.

### Raw samples, no expiry

- **Schema (migration 00536, draft number):** `anthropic_usage_samples` (owner, token, `observed_at`, enablement revision, source, five-hour and seven-day percent and reset) and `codex_usage_samples` (owner, provider account, `observed_at`, generation, credential revision, `buckets` jsonb). `bucket_start` is a STORED generated `date_bin('5 minutes', observed_at, <fixed origin>)` column (IMMUTABLE on PostgreSQL 17) and part of the primary key `(user_id, identity, bucket_start)`, which arbitrates `ON CONFLICT DO NOTHING`. No other index. A `BEFORE UPDATE` trigger makes owner and identity immutable.
- **Foreign key:** only `user_id` references `users ON DELETE CASCADE`. The identity column has none, so deleting a credential keeps its history; deleting the owner's users row removes it (uzi never deletes one itself; deactivation keeps the history). There is no age-based expiry. Down drops both tables, taking ACCESS EXCLUSIVE on users with no lock timeout.
- **Copy fences:** each copy is one autocommit `INSERT ... SELECT ... ON CONFLICT DO NOTHING` from the gauge row, with no transaction, `SET LOCAL`, row lock or advisory lock (the only implicit lock is the users FK's `FOR KEY SHARE`). Anthropic matches owner, token, enabled state and revision, `synced_at`, source (`usage_endpoint` or `header_probe`, never `limit_report`) and the null-safe reading. Codex matches the account, generation, credential revision, enablement signature and the exact `buckets` bytes, requires `attempt_status = 'ok'` with `last_attempt_at` equal to the returned `last_success_at`, a live account at that generation and revision with an enabled linked alias, and the recomputed alias signature.
- **Admission:** the reading's timestamp must be newer than `now() - 1 hour` and no later than `now() + 15 minutes` (database clock). It now only guards freshness and clock skew; no expiry fence remains.
- **Codex payload policy:** any violation selects nothing (a gap, never a truncation): an array of at most 17 objects with unique string ids of 1 to 99 bytes, boolean or null flags, object or null windows, `used_percent` 0..100 or null, positive integer window seconds, non-negative integer resets, integers at most 2^53-1. Stored projection `{id, a, l, s1, s2}` with windows `{p, w, r, rd}`; a reset derived from `reset_after_seconds` is flagged approximate (`rd`). The projection is bounded to 4 KiB in the statement and by a table CHECK, so near-maximum readings and `used_percent` above 100 are gaps.
- **Placement:** inline, no copier. The pollers call the copy after their existing post-success work (notification, warner and backoff clearing) only when the gauge write was accepted, through an optional nil-safe `HistoryRecorder` seam. The test-connection handler copies after its final enablement re-read, only when the verdict is still `ok`. Each copy runs under one 250 ms context covering pool acquisition and execution and logs only a fixed category (`timeout`, `error`, `skipped`) and the token or provider account id. The gauge writes are unchanged.
- **Measured growth** (PostgreSQL 17.12, heap plus primary key as grown through the real copy path, upper bound at full cadence of 105,120 rows a year): 218 B per Anthropic row (22.9 MB per identity-year), 666 B per typical Codex row (70.0 MB), 2,128 B per largest admissible Codex row measured (224 MB; compressed inline, nothing stored out of line); 10 and 100 identities extrapolated linearly. Owner deletion of one identity-year took 0.14 to 0.80 s with 21 to 213 MB of WAL after a checkpoint, the realistic case for old history (0.10 to 0.24 s and about 6 MB otherwise). See the Decision Log for the other measurements.

### Docs and specs

Document what is recorded (sources, buckets, gaps, the fences, no expiry and owner cascade, measured growth) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store`; poller unit tests cover caller behaviour. Execute the real copy statements and `date_bin` constraints on PostgreSQL 17.
- Provider: a rejected fence never invokes the copy; a copy failure does not undo the gauge; disable/re-enable, Codex rotation/unlink, alias dedup, post-success gauge replacement, the unchanged-`synced_at` exhaustion mutation, null windows, source and size bounds, admission window, owner cascade and the immutable-owner trigger.
- Observe each regression-like mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. Sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Provider headroom samples

**Blocked by:** none.

Deliver the raw sample migration, the best-effort success-only inline copy for both providers (pollers and token test-connection), docs/specs/tests.

Acceptance: examples 1 to 3 hold; the gauge success writes stay autocommit with no savepoint and no new credential lock; rejected, overwritten and `limit_report` readings never enter history; the five-minute and 4 KiB bounds hold. Attach measured poll latency and write amplification, query plans of the copy statements, and payload and storage totals.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Capture now, read later | Readings not recorded when accepted are lost; reject waiting for a read API before recording. |
| Separate success-following `INSERT ... SELECT` | The gauge locks and autocommit are security-sensitive; reject wrapping them in a new transaction or savepoint. |
| Copy only the expected accepted gauge and source | Post-success races and exhaustion marks can mutate the row; reject timestamp-only copying. |
| Provider-specific tables and SQL `date_bin` | Correct uniqueness without nullable-identity collisions. |
| ~~Closed-hour full recompute and count-matched expiry~~ | Superseded: no summaries are built; see "No deletion by default and no summaries". |
| ~~Atomic emergency lost-coverage records~~ | Superseded: nothing is expired, so no coverage is lost. |
| ~~Existing independent sweeper passes~~ | Superseded: no sweeper pass is added; no expiry or summary work exists. |
| No deletion by default and no summaries | Raw samples are small and are the history; a configurable deletion mechanism, off by default, is a later issue. Rejected: 30/400-day retention with hourly summaries and lost-coverage records (gates, watermarks and two sweeper passes for data we want to keep). |
| Inline copy, no copier | One bounded statement after post-success work; measured before adding machinery. Rejected: a queue or copier. |
| Owner-only foreign key | Owner deletion cascades the history; the identity column has no FK, so deleting a credential keeps its history. Rejected: an FK to the credential (would delete or block history). |
| Test-connection handler included | The PRD says every accepted provider reading, and the token test is a third accepted gauge-write path (two Anthropic, one Codex). It copies after its final re-read, only for `ok`, under a 250 ms context of its own, so it can add up to 250 ms to that response. |
| Accepted copy race | A credential change that commits after the copy's snapshot can still admit a reading the gauge already accepted; reject adding locks or a transaction to the gauge path. |
| Admission window kept | 1 h back and 15 min ahead of the database clock only guards freshness and clock skew; no expiry fence remains. |
| Near-maximum Codex readings and `used_percent` above 100 are gaps | The 4 KiB bound and the 0..100 policy stay (owner instruction); a skipped reading is a gap, never a truncation. Measured: 17 buckets with ids over about 78 to 86 bytes (depending on the window values) exceed the bound. |
| Measured (PostgreSQL 17.12, one to three runs each, shared host, real `store.Migrate` and generated queries) | Rows (as grown / one identity in time order): 218/200 B Anthropic, 666/644 B typical Codex, 2,128/2,107 B largest admissible Codex measured (17 buckets, 86-byte ids, compressed inline: a less compressible admissible payload can be larger); nothing stored out of line; PK leaf density 68 to 76% (90% in time order). WAL per copy (mean over 10,000 to 12,000 copies, with and without a preceding CHECKPOINT): 570 to 617 B, 873 to 902 B, 2.3 to 2.4 KB; no-op 0. At production cadence (one copy per identity per five minutes, equal to the default checkpoint interval) most copies carry an index full-page image: a dba pg_walinspect experiment (Anthropic) measured 6.4 to 6.9 KB per copy after a checkpoint versus about 400 B without (full_page_writes on, wal_compression off), about 200 MB a day for 100 Anthropic identities and more for large Codex accounts. Latency: copy 0.69 ms (Anthropic) / 0.93 ms (Codex) p50 on an idle connection, about doubling the DB time of the gauge write; pgbench at 1/10/100 clients p50 0.6 to 3.1 ms, 0 over 250 ms without locks. Real 20-connection pgxpool, 100 goroutines, 250 ms context over acquire and exec: 0 of 8,000 copies per provider timed out without locks (4,000 inserts, 4,000 same-bucket no-ops); with 5% of owners' `users` rows held FOR UPDATE, 200 blocked copies timed out and about 185 connections were replaced; with 50% held, about 2,170 to 2,200 timeouts, about 1,900 new connections, and 20 to 38 unblocked timed-out copies (plus one blocked Codex copy) had committed. Plans: index scans only, no sequential scan of a sample table (copy 0.17 ms / 0.49 ms execution). Owner delete of one identity-year: 0.14 to 0.80 s and 21 to 213 MB WAL after a checkpoint, the realistic case for old history; uzi never deletes a users row itself. Not measured: end-to-end poll with the HTTPS call; production hardware. No plan change recommended on latency. |

**Verified locally:** storage and WAL bytes, the copy's added latency and its query plans, on PostgreSQL 17.12 (see the Decision Log). **UNVERIFIED:** end-to-end poll latency including the provider HTTPS call, and production hardware; the accepted-copy race is documented, not closed. The exact-head PR checks stay landing obligations.
