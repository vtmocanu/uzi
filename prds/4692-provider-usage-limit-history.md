# PRD #4692: Record provider usage-limit history

**Issue:** #4692.
**Status:** Ready.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Additive sample, summary and lost-coverage migrations, a best-effort copy after the existing successful gauge writes, existing sweeper passes, docs/specs/tests. No new API route, DTO, user interface, command or service. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [ ] M1 provider headroom samples, hourly summaries and bounded retention

## 1. Problem

The Anthropic token and Codex account rate-limit gauges hold only the latest accepted reading; each successful poll overwrites the last one. Nothing records how headroom changed over time, and a missing observation cannot be told apart from zero use. Readings that are not recorded when they are accepted cannot be recovered later.

## 2. Outcome

Every accepted provider reading is also copied, best-effort, into a bounded five-minute sample series and rolled into hourly summaries. A five-minute bucket with no accepted sample is a visible gap; raw evidence deleted at the emergency retention boundary leaves a durable lost-coverage record. Nothing reads this history yet; it starts accumulating now so later read APIs have real data. The existing gauges, their locks and their behaviour are unchanged.

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

Existing pollers: `api/internal/usagepoller/engine.go` and `api/internal/codexusagepoller/engine.go`. Both poll every five minutes by default, with a one-minute minimum configured cadence. Codex normalization permits at most 16 additional buckets and bounds labels to 96 bytes. The two success writes and their lock paths, as they are today:

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

### Raw samples, summaries and coverage loss

- Store fixed bounded identifiers and either fixed numeric columns or a normalized payload capped at 4 KiB in the `INSERT ... WHERE`. Apply the same bound to summary payloads. An unsupported or oversized reading is skipped, never truncated into apparently complete coverage.
- Raw samples: 30 days. Hourly summaries: 400 days. Identity/hour uniqueness makes recomputation idempotent.
- Summarize only closed UTC hours, as a full recompute from that hour's raw samples. Keep first/last observation, sample count and per-window present counts, min, mean and max; keep bucket identity and window duration separate, and record missing values and reset crossings. Means are sample-weighted observed percentages, not quota consumption, and never combine credentials or windows.
- Retention boundaries are whole identity-hours. Delete raw rows only for whole hours beyond raw retention whose summary matches the current raw `sample_count`, verified and deleted in one transaction. Sampling admits only recent accepted readings, never into an hour already eligible for raw deletion. A closed hour may be recomputed while all of its raw evidence is intact, so a valid late sample can raise its count; deleting an identity-hour requires a summary whose `sample_count` matches the current raw count, and deletes that whole identity-hour atomically, after which it is never recomputed. Test the summary/deletion race and a closed hour going from one sample to two (late sample) and then expiring with both observations in the summary.
- A five-minute bucket with no accepted sample is a gap; a failed poll or a failed or skipped copy records nothing (it may fall in a bucket another accepted sample already filled). The durable lost-coverage record is narrower: it covers only raw evidence deleted at the emergency boundary. On repeated summary failure, keep raw rows up to a 45-day emergency boundary; emergency deletion of an hour and its lost-coverage record (identity, hour, range and bounded counts, no provider text) commit in the same transaction. Lost-coverage records: 400 days. Summary failure, oversize, overflow or emergency expiry is never recorded as a summarized zero.

Per continuously sampled identity: at most 8,640 raw rows over 30 days (12,960 at the emergency boundary) and 9,600 hourly rows over 400 days. At 4 KiB per payload that is about 33.75 MiB raw plus 37.5 MiB hourly (50.6 MiB raw at the emergency boundary), before row and index overhead. These are payload ceilings, not measured sizes; include fixed metadata and lost-coverage rows in the measured projection.

### Bounded sweeper passes

Provider summarization and expiry run as named passes on the existing `sweeper.Pass` (`api/internal/sweeper/sweeper.go`) at the existing cadence (default 15 s, operator setting preserved), each with its own session advisory lock constant registered in `store/migrate.go`, reusing `workersvc/checkpoint_retention.go`'s pinned-connection, try-lock and proven-unlock pattern. No new loop, registry or replica dedup layer.

Each pass gets a two-second deadline and at most four batches of 1,000 work units, one committed transaction per batch. A summary work unit is one identity/UTC-hour, which holds at most 12 raw rows because five-minute identity/bucket uniqueness is enforced. One pass failing must not block liveness or other sweeper work. Measure effective cadence, added worst-case delay before the liveness sweep, and drain against peak sample and expiry inflow (in raw rows and identity-hours), plus backlog and oldest expired record. If the bounds cannot keep up, revise before merge.

### Docs and specs

Document what is recorded (sources, buckets, gaps, retention, lost coverage) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store`; poller unit tests cover caller behaviour. Execute the real copy statements and `date_bin` constraints on PostgreSQL 17.
- Provider: a rejected fence never invokes the copy; a copy failure does not undo the gauge; disable/re-enable, Codex rotation/unlink, alias dedup, post-success gauge replacement, the unchanged-`synced_at` exhaustion mutation, null windows, source and size bounds.
- Summary: closed-hour rule, full recompute, `sample_count` match, concurrent and late samples versus deletion, retry idempotence, reset and window changes, oversize summary, emergency deletion with atomic lost-coverage record.
- Maintenance: bounded batches and deadlines, independent failure, session-lock cleanup, sustained drain and added liveness latency, using injected clocks, not wall-time sleeps.
- Observe each regression-like mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. Sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Provider headroom samples, hourly summaries and bounded retention

**Blocked by:** none.

Deliver the raw, hourly and lost-coverage migrations, the best-effort success-only copy for both providers, the summary and expiry passes, docs/specs/tests.

Acceptance: examples 1 to 3 hold; the gauge success writes stay autocommit with no savepoint and no new credential lock; rejected, overwritten and `limit_report` readings never enter history; the five-minute and 4 KiB bounds hold; summaries, deletion and lost-coverage records stay correct under races. Attach measured poll latency and write amplification, query plans, payload and storage totals, and effective drain.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Capture now, read later | Readings not recorded when accepted are lost; reject waiting for a read API before recording. |
| Separate success-following `INSERT ... SELECT` | The gauge locks and autocommit are security-sensitive; reject wrapping them in a new transaction or savepoint. |
| Copy only the expected accepted gauge and source | Post-success races and exhaustion marks can mutate the row; reject timestamp-only copying. |
| Provider-specific tables and SQL `date_bin` | Correct uniqueness without nullable-identity collisions. |
| Closed-hour full recompute and count-matched expiry | Prevents a stale summary silently losing samples. |
| Atomic emergency lost-coverage records | Storage stays bounded without pretending lost history survived. |
| Existing independent sweeper passes | No new loop or scheduler. |

**UNVERIFIED before implementation:** accepted-copy race behaviour, storage and WAL bytes, write latency and pass drain. All are local code or PostgreSQL experiments; no external lookup is left to the worker.
