# PRD #4693: Record worker capacity samples

**Issue:** #4693.
**Status:** Done.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Additive sample migration, one sampling pass on the existing sweeper (no age-based expiry, by owner decision), docs/specs/tests. No new API route, DTO, user interface, command or service; no change to claim, admission or worker deletion behaviour. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [x] M1 five-minute worker capacity samples (no age-based expiry)

## 1. Problem

A worker's advertised slots (`workers.max_concurrent_runs`, `workers.max_cross_check_slots`) and the runs it holds are only known as of now. When an ephemeral worker is deleted, or a cap changes, the past is gone. Nothing records how much capacity existed and how much was in use over time, and a worker that was not observed cannot be told apart from an idle one. Samples not taken when they happen cannot be reconstructed later.

## 2. Outcome

Each worker has at most one committed sample per five-minute bucket (a worker the pass does not reach in a bucket is a gap): observed advertised caps, ordinary occupancy and separately classified supporting lanes, with its owner and freshness. Samples survive worker deletion and are removed with their owner. No sample is deleted by age (owner decision); a configurable, off-by-default deletion mechanism for all analytics history is a later issue. Nothing reads this history yet; it starts accumulating now so a later read API can report observed slot use honestly.

Acceptance examples:

1. An ephemeral worker is sampled and then deleted (`DeleteEphemeralWorkerForRun`). Its samples remain, attributed to its owner.
2. A worker's cap is unknown (null). Its samples record unknown, never zero advertised slots.
3. A stale or offline worker is sampled as not online and contributes no online advertised capacity.
4. Deleting the owner removes that owner's samples.

## 3. Out of scope

- Any read API, DTO, page or command over these samples (a later issue).
- Provider usage-limit history (a separate issue).
- Exact schedulable capacity, CPU utilization or infrastructure monitoring.
- Any change to claim, lane admission, cross-check lanes, chat or worker deletion.
- A new maintenance loop or registry; workflow changes.
- Any deletion of samples other than the owner cascade (a later issue).

## 4. Modules and seams

### Samples

- One named pass on the existing `sweeper.Pass` (`api/internal/sweeper/sweeper.go`) writes at most one sample per worker per five-minute bucket, keyed by worker UUID and a SQL `date_bin` bucket (idempotent: a repeated tick inserts nothing new).
- Columns: observation time, owner UUID, opaque worker UUID by value, heartbeat freshness classification, nullable advertised ordinary cap, nullable cross-check slot cap, ordinary occupancy, and supporting-lane occupancy counted separately.
- Owner FK `ON DELETE CASCADE`; no FK to `workers`, so worker deletion keeps the samples.
- Anchor occupancy to the current worker-list predicates (`ListWorkersByUser` / `ListAllWorkers` in `queries/runtime.sql`): a run holds a slot only when it is bound to the worker and its status is one of `claimed`, `running`, `awaiting_approval`, `awaiting_input`, `awaiting_followup`. Ordinary occupancy is that set with `kind <> 'chat' AND NOT cross_check_lane` (the existing `active_runs`); cross-check-lane occupancy is the set with `cross_check_lane`; chat is counted separately. A cross-check that runs on an ordinary slot (no `cross_check_lane`) counts as ordinary, exactly as today. A `pool_wait` run or a worker affinity reference alone holds no slot. Isolated-lane workers are sampled like any other, with their `isolated_lane` flag recorded. Reuse the current heartbeat-staleness definition; introduce no new one.
- Store observed values only: never clamp an over-cap occupancy, never turn an unknown cap into zero.

### Owner attribution

Workers are owned by one user (`workers.user_id NOT NULL`). A sample's occupancy must count only runs whose owner equals the worker's owner. Before writing numeric occupancy, audit every lane that can place a run on a worker (ordinary claim, pool wait, cross-check, chat, isolated lane) and confirm owner equality holds in each. Any lane where a run's owner can differ from the worker's owner, or where attribution cannot be proven, is recorded as attribution unavailable for that sample, never as counted occupancy. Record the audit result in the PRD Decision Log. Do not broaden owner visibility or change claim behaviour to make the audit pass.

### Pass bounds

One sampling pass with its own session advisory lock constant registered in `store/migrate.go`, reusing `workersvc/checkpoint_retention.go`'s pinned-connection, try-lock and proven-unlock pattern; existing cadence (default 15 s, operator setting preserved), two-second deadline covering acquire, try-lock, batches, unlock and close, at most four batches of 1,000 workers, each batch one autocommitted statement. Each batch selects, in worker-id order, up to 1,000 workers with no sample at or after the current bucket, then inserts with `ON CONFLICT DO NOTHING`. A worker not reached within a five-minute bucket is a gap, not a sample; a failed batch may be retried on a later tick of the same bucket without duplicating samples. There is no stored cursor and no expiry. One pass failing must not block liveness or other sweeper work.

Measure with a stated synthetic workload (worker count, runs per worker): bytes per row (table and indexes), write-ahead log per batch, rows the selection examines when most workers are already sampled, pass wall time (the delay before the liveness sweep), owner-delete cost at one year of rows, and growth per always-on worker-year (105,120 rows) and at 50 and 500 workers. The run needs no live fleet access; any real fleet rate comes from aggregates the maintainer supplies at review. If the bounds cannot keep up, revise before merge.

### Docs and specs

Document what is sampled (columns, freshness, lanes, attribution rule, gaps, no age-based expiry, owner cascade, measured growth) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store`, executing the real sampling SQL on PostgreSQL 17: stale, offline and null-cap workers; ordinary, chat, cross-check, isolated-lane and pool-wait occupancy; over-cap values; ephemeral worker deletion preserving samples; owner deletion cascading them; repeated ticks idempotent; a worker missed in a bucket leaves a gap.
- Attribution: a fixture per audited lane proving occupancy counts only same-owner runs, and a forced cross-owner case recorded as attribution unavailable.
- Occupancy: `pool_wait` and affinity-only rows hold no slot; cross-check on an ordinary slot counts as ordinary; chat counted separately; isolated-lane flag recorded.
- Maintenance: a fleet larger than one batch is fully sampled across batches and ticks within one bucket; bounded batches and deadlines, the pass ending at the first failed batch, a failed then retried insert, a committed insert whose reply is lost, session-lock cleanup, using injected clocks, not wall-time sleeps.
- Observe each regression-like mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. Sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Five-minute worker capacity samples (no age-based expiry)

**Blocked by:** none.

Deliver the sample migration, the sampling pass, the owner-attribution audit, docs/specs/tests.

Acceptance: examples 1 to 4 hold; attribution is proven per lane or recorded as unavailable; worker deletion preserves samples; no clamping and no unknown-as-zero; no claim, admission or deletion behaviour changes. Attach the measured bytes per row, write-ahead log per batch, pass wall time and owner-delete cost under the stated synthetic workload, and the projected growth.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Capture now, read later | Samples not taken when they happen are lost; reject waiting for a read API before recording. |
| Worker id by value, owner FK only | Ephemeral deletion must not erase owner-scoped history. |
| Observed values, unknown kept unknown | Honest later percentages; reject reconstructing past capacity from today's workers or clamping. |
| Attribution unavailable over guessed counts | Never leak or misattribute another owner's runs; reject broadening visibility. |
| Existing sweeper pass | No new loop or scheduler. |
| No record deleted by default (owner decision, 2026-11-25) | Samples have no age-based expiry and still cascade with their owner. Rejected: 400-day retention, an expiry lane and lane alternation, for about 22 MiB per worker-year per registered worker (measured below). A configurable, off-by-default deletion mechanism for all analytics history is a later issue. |
| At most one sample per worker per bucket, not at most one attempt | A primary key on (worker, bucket) plus an anti-join on samples at or after the current bucket gives idempotent retries and refuses an earlier bucket after a backward clock step. Rejected: a stored pagination cursor and a state table. |
| Fail-closed attribution in SQL | A sample is `owner` only when every slot-holding run on the worker has the worker's owner and an audited kind/lane shape (all ten run kinds, the cross-check lane only on `cross_check`) and an egress-profile binding that matches the worker's `isolated_lane` (ClaimRun's two-way clause); otherwise `unavailable` with no counts. A sync test keeps the kind list in step with `runkind`, so a new kind fails the build. |
| Pinned session, never pooled | The pass hijacks its connection, sets a 1.75 s statement timeout, 300 ms lock timeout, 5 s idle-session timeout and 250 ms client-connection check, and always closes it, so no lock or setting leaks into the pool. |

**Owner-attribution audit (implementation).** Every SQL writer that can put a run into a slot-holding status was enumerated (queries, migrations, triggers). Owner-gated claims: `ClaimRun` (`runtime.sql`, target `r.user_id = @user_id`, caller passes the worker's owner) and `ClaimChatRun` (`chat.sql`, `r.user_id = @user_id`). Slot-to-slot moves that keep `worker_id`: `SetRunRunning`, `SetRunAwaitingApproval`, `SetRunAwaitingInput`, `SettleRunClarificationToGate`, `AdmitRunFollowupFinalize`, `SetRunAwaitingFollowup`. Snapshot readoption (`ReadoptRunsFromSnapshot`, `FrozenReadoptRunsFromSnapshot`) is not slot-to-slot: it moves a `queued` run back to a slot status, but only when the run is still bound to the same worker (`r.worker_id = @worker_id`), the worker lists it live at the same claim generation (`worker_active_runs`), and no fresh resume is pending or claimed; that is, it reverses a stale requeue of a run that worker had claimed, not an affinity-only binding. Its provenance is not separately proven here, so the sample does not rely on it: the per-sample gate itself checks owner equality, kind × cross-check lane, and the two-way isolated-lane match (`(r.egress_profile_id IS NOT NULL) = w.isolated_lane`, mirroring `ClaimRun`) on every slot-holding run, and records `unavailable` on any mismatch. `RestoreFreshResumeTuple` restores the saved pre-prepare status (`recovery_wait`, no slot) and worker. Affinity-only writers leave runs in non-slot statuses until an owner-gated claim: `PrepareFreshResumeClaim` (`queued`, and the claim refuses an owner mismatch), `CreateChatContinueRun` (`queued`), the recovery-wait rebind and the pool-wait requeue. `runs.user_id` and `workers.user_id` have no updating writers. No path that puts a mismatched run on a worker was found; the per-sample guard still records `unavailable` if any slot-holding run's owner, lane shape or isolated-lane binding fails. Fixtures in `worker_capacity_samples_livedb_test.go` cover the resulting row shape of each path; chat continuation and fresh resume use the real create/prepare/restore queries, while the other shapes (including the chat claim, pool-wait and recovery-wait rows) are built directly. Because the gate reads only the row state, the fixtures prove classification of each shape, not each transition's own guards.

**Measurements (synthetic, PostgreSQL 17.12, `shared_buffers` 128 MB, 5,000 workers over 50 users, 3 slot-holding runs each, about 10.1 million sample rows).** Bytes per row 218 (heap 134, primary key 70, owner index 14), so about 22 MiB per worker-year (105,120 rows; every registered worker row is sampled, online or offline; the owner index dedups at 100 workers per owner, an unmeasured estimate of nearer 28 MiB per worker-year at one worker per owner), about 1.1 GiB a year at 50 workers and about 10.7 GiB at 500. Write-ahead log about 389 KB per 1,000-sample batch at steady state (7.4 MB right after a checkpoint, from full-page images). Pass wall time: 171 ms and 98 ms for the two passes that covered the fleet, about 40 ms (max 47 ms) for each of the 20 steady-state passes in the same bucket; over all 22 passes the nearest-rank p95 was 98 ms (derived from the per-pass figures above). With all but 100 workers sampled, the batch statement ran in 29 ms: one primary-key probe per worker (5,000 probes) and the runs probe through `runs_worker_nonterminal_idx` only for the 100 selected workers. Deleting an owner with one worker-year of samples (105,120 rows) took 129 ms and 19.9 MB of write-ahead log right after a checkpoint; it grows linearly with the owner's history in one transaction. Harness: `TestWorkerCapacitySampleLoadMeasurement` (opt-in, `UZI_CAPACITY_SAMPLE_MEASURE=1`).

**Accepted risks.** Storage grows without age-based expiry until the later deletion mechanism. An owner delete removes all that owner's samples in one transaction. While an owner's `users` row is locked (account reset, owner delete), any batch containing one of that owner's workers fails as a whole after the 300 ms lock timeout (other owners' workers in it included), and since that worker stays first-unsampled the same batch fails on every later tick, so every worker from that batch onward is a gap until the lock is released (the whole fleet at 1,000 workers or fewer); the next tick after release retries idempotently. Dropping `ON CONFLICT DO NOTHING` is caught only by `TestWorkerCapacitySampleConcurrentSamplersLiveDB`: in sequential retries the anti-join already excludes sampled workers, so the lost-reply and retry tests do not depend on that clause. A session that cannot be confirmed released makes other passes skip until its backend ends. One new database connection per tick.
