# PRD #4693: Record worker capacity samples

**Issue:** #4693.
**Status:** Ready.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Additive sample migration, one sampling and expiry pass on the existing sweeper, docs/specs/tests. No new API route, DTO, user interface, command or service; no change to claim, admission or worker deletion behaviour. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [ ] M1 five-minute worker capacity samples with bounded retention

## 1. Problem

A worker's advertised slots (`workers.max_concurrent_runs`, `workers.max_cross_check_slots`) and the runs it holds are only known as of now. When an ephemeral worker is deleted, or a cap changes, the past is gone. Nothing records how much capacity existed and how much was in use over time, and a worker that was not observed cannot be told apart from an idle one. Samples not taken when they happen cannot be reconstructed later.

## 2. Outcome

Each worker is sampled at most once per five-minute bucket (a worker the pass does not reach in a bucket is a gap): observed advertised caps, ordinary occupancy and separately classified supporting lanes, with its owner and freshness. Samples survive worker deletion and are removed with their owner. Nothing reads this history yet; it starts accumulating now so a later read API can report observed slot use honestly. Bounded retention keeps storage finite.

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

## 4. Modules and seams

### Samples

- One named pass on the existing `sweeper.Pass` (`api/internal/sweeper/sweeper.go`) attempts to sample each worker at most once per five-minute bucket, keyed by worker UUID and a SQL `date_bin` bucket (idempotent: a repeated tick inserts nothing new).
- Columns: observation time, owner UUID, opaque worker UUID by value, heartbeat freshness classification, nullable advertised ordinary cap, nullable cross-check slot cap, ordinary occupancy, and supporting-lane occupancy counted separately.
- Owner FK `ON DELETE CASCADE`; no FK to `workers`, so worker deletion keeps the samples.
- Anchor occupancy to the current worker-list predicates (`ListWorkersByUser` / `ListAllWorkers` in `queries/runtime.sql`): a run holds a slot only when it is bound to the worker and its status is one of `claimed`, `running`, `awaiting_approval`, `awaiting_input`, `awaiting_followup`. Ordinary occupancy is that set with `kind <> 'chat' AND NOT cross_check_lane` (the existing `active_runs`); cross-check-lane occupancy is the set with `cross_check_lane`; chat is counted separately. A cross-check that runs on an ordinary slot (no `cross_check_lane`) counts as ordinary, exactly as today. A `pool_wait` run or a worker affinity reference alone holds no slot. Isolated-lane workers are sampled like any other, with their `isolated_lane` flag recorded. Reuse the current heartbeat-staleness definition; introduce no new one.
- Store observed values only: never clamp an over-cap occupancy, never turn an unknown cap into zero.

### Owner attribution

Workers are owned by one user (`workers.user_id NOT NULL`). A sample's occupancy must count only runs whose owner equals the worker's owner. Before writing numeric occupancy, audit every lane that can place a run on a worker (ordinary claim, pool wait, cross-check, chat, isolated lane) and confirm owner equality holds in each. Any lane where a run's owner can differ from the worker's owner, or where attribution cannot be proven, is recorded as attribution unavailable for that sample, never as counted occupancy. Record the audit result in the PRD Decision Log. Do not broaden owner visibility or change claim behaviour to make the audit pass.

### Retention and pass bounds

Retain samples 400 days (at most 115,200 rows per continuously present worker). Sampling and expiry share one named pass with its own session advisory lock constant registered in `store/migrate.go`, reusing `workersvc/checkpoint_retention.go`'s pinned-connection, try-lock and proven-unlock pattern; existing cadence (default 15 s, operator setting preserved), two-second deadline, at most four batches of 1,000 work units, one committed transaction per batch, deterministic pagination. A worker not reached within a five-minute bucket is a gap, not a sample. Both lanes share the pass's two-second deadline and four-batch budget, so a batch count alone does not reserve time: the pass alternates which lane runs first on successive ticks, so each lane gets the opportunity to run first at least every other tick; on a tick where the other lane runs first, the second lane runs only if time and batches remain. Sampling resumes from a stored pagination cursor. Barring database failures, each lane therefore gets a turn at least every other tick (report scheduling fairness and measured drain separately), and a fleet larger than one batch is covered over successive ticks rather than starved. One pass failing must not block liveness or other sweeper work.

Measure with a stated synthetic workload (worker count, churn rate, runs per worker): sample inflow, projected rows and bytes, collector write load, effective cadence, added delay before the liveness sweep and expiry drain. The run needs no live fleet access; any real fleet rate comes from aggregates the maintainer supplies at review. If the bounds cannot keep up, revise before merge.

### Docs and specs

Document what is sampled (columns, freshness, lanes, attribution rule, gaps, retention) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- LiveDB tests only in `store`, executing the real sampling and expiry SQL on PostgreSQL 17: stale, offline and null-cap workers; ordinary, chat, cross-check, isolated-lane and pool-wait occupancy; over-cap values; ephemeral worker deletion preserving samples; owner deletion cascading them; repeated ticks idempotent; a worker missed in a bucket leaves a gap.
- Attribution: a fixture per audited lane proving occupancy counts only same-owner runs, and a forced cross-owner case recorded as attribution unavailable.
- Occupancy: `pool_wait` and affinity-only rows hold no slot; cross-check on an ordinary slot counts as ordinary; chat counted separately; isolated-lane flag recorded.
- Maintenance: a fleet larger than one batch is fully sampled across successive ticks while expiry still progresses, including when the lane that runs first exhausts the two-second deadline (the other lane runs first on the next tick); bounded batches and deadlines, independent failure, session-lock cleanup, drain, using injected clocks, not wall-time sleeps.
- Observe each regression-like mutation fail and the restored code pass, per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. Sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run affected library/fake targets and name unrun phases.

## 6. Milestones

### M1: Five-minute worker capacity samples with bounded retention

**Blocked by:** none.

Deliver the sample migration, the sampling and expiry pass, the owner-attribution audit, docs/specs/tests.

Acceptance: examples 1 to 4 hold; attribution is proven per lane or recorded as unavailable; worker deletion preserves samples; no clamping and no unknown-as-zero; no claim, admission or deletion behaviour changes. Attach measured inflow under the stated synthetic churn, projected storage, collector write load and pass drain.

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

**UNVERIFIED before implementation:** owner attribution across every lane, storage under real churn, write load and drain. All are local code or PostgreSQL experiments; no external lookup is left to the worker.
