# PRD #216 — Spread runs across idle workers automatically

**Issue**: [#216](https://gitlab.example.com/vtmocanu/uzi/-/issues/216) · **Label**: PRD · **Priority**: High
**Area**: `api/internal/store/queries/runtime.sql` (`ClaimRun` — the whole feature) · `api/internal/workersvc/service.go:824-830` (the sole caller, gains params) · one migration (the partial index, D11) · `adr/0216-fleet-aware-claim.md` (M0) · `deploy/chart/` + `docs/` (M7-M8).
**Line references** are against `d367653b`.
**Status**: not started.
**Related**: **PRD #84 (capability-aware scheduling) adds a predicate to this same statement** — see D5. **ADR-42** (worker run concurrency) is amended by this PRD — see M0.
**Reviewed** 2026-08-03, one architectural pass. Five blocking findings, all applied. Two were established against a real `postgres:17` rather than reasoned about, and one of those (D7) refutes the design's most natural implementation.

**Correction record**: the first draft's central mechanism argument was **wrong**, and its risk section would have steered testing away from the bugs that actually exist. Both are preserved as D2 and R1 with the correction stated, rather than quietly replaced, because "the design livelocks for reason X" and "the design livelocks for reasons Y and Z, which X's tests cannot see" have very different consequences for M2.

## Problem

Adding a second worker does not spread work across it. **This is a mechanism
argument, not an observation — see the correction immediately below, which is
recorded here rather than in a footnote because the first draft got it wrong.**

> **⟨corrected⟩ The two runs this PRD was written from CANNOT demonstrate a
> placement failure, because the fleet had one worker when they were claimed.**
> The first draft's opening evidence read "Two runs were queued and both landed
> on `base.l-da4a`, which sat at 2/2 while `base.l-d066` sat at 0/2". Measured:
> `base.l-d066` `created_at` = **2026-08-03T14:30:27Z**; run #209 `claimed_at` =
> **12:45:37**, run #78 created **12:30:50**. The second worker's row did not
> exist until **1h45m after** #209 was claimed. The runs were also created 15
> minutes apart, not queued together. So no scheduler chose `da4a` over an idle
> peer; there was no peer.
>
> The one placement that did occur with `d066` alive is #78's re-claim at
> **14:41:12**, and its feed shows a `limit_wait` park at 14:38:06 followed by
> "resuming an already-approved plan" — a **resume**, which D3 deliberately
> returns to its prior worker. Even the shipped fix places it identically.
>
> The three mechanisms below are confirmed from code and the concern is real.
> What does not exist yet is an observation of the bug. **M5 must produce one**
> (a fresh pair queued against a genuinely idle two-worker fleet) rather than
> re-citing this pair.

Three mechanisms, each individually sensible, compose into the behaviour:

**1. The claim is pull-based with no fleet awareness.** `ClaimRun`
(`runtime.sql:432-493`) selects `LIMIT 1` ordered by
`COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC` — resume
affinity, then oldest-first. Nothing in the predicate or the ordering knows how
many runs the claiming worker already holds, or that a peer is idle.

**2. The advertised cap is not enforced server-side.** From `runtime.sql:77-78`:
"max_concurrent_runs (the advertised cap, NULL when unadvertised) rides on
`w.*`; it is **observability only and never enforced**." Capacity lives entirely
in the worker (`agent/src/worker.ts:140-152`).

**3. A worker that claims does not yield.** `agent/src/worker.ts:174-176` — the
sleep is skipped **only on success**, so the first worker to poll an idle fleet
claims run 1 and is back asking for run 2 within one API round-trip, while its
peer is still inside its 3s `WORKER_POLL_INTERVAL` (`agent/src/config.ts:264`).

### The workaround is a REVERT, and it is still worse than the problem

`workers.maxConcurrentRuns: 1` is the **chart default**
(`deploy/chart/values.yaml:371-376`, "Default 1 (safe): a hosted worker takes ONE
run at a time regardless of size"). This deployment *raised* it, so "set it to 1"
is a revert, not an addition — the values edit already happened, in the other
direction. That matters for framing: the knob is not missing, it was
deliberately opened for burst capacity.

Reverting does force one run per worker, but it is a **fleet-wide, static**
answer to a **per-claim, dynamic** question. It gives back the burst capacity
that was the reason to raise it, needs a controller round-trip per fleet change,
and does not survive growth: three workers and four runs still needs a placement
decision.

### What this costs, stated at the precision the evidence supports

Two runs on one `l` worker share a **4-CPU limit against a 1-CPU request**
(`controller/internal/preset/preset.go:119-125`: `CPURequest: 1`,
`CPULimit: 4`, `MemoryRequest: 4Gi`, `MemoryLimit: 8Gi`) — so under node pressure
the *guaranteed* share is 1 CPU, which strengthens the argument rather than
weakening it.

Measured across the two runs: **405-452 seconds** (two independent
classifications) where both ran gate/test commands simultaneously. Gate commands
averaged 43.1s overlapping vs 16.3s alone (47.6s vs 12.2s on the independent
pass). **That comparison is confounded** — a longer command is mechanically more
likely to intersect another interval, and the "alone" set is dominated by ~108
short commands — so it establishes direction only.

**The clean signal is the same command at a CONSTANT suite size. ⟨corrected⟩**
The first draft cited `npm test` at 34s / 46s / 68s "on an unchanged tree"; the
tree was **not** unchanged (those runs reported 1407, 1455 and 1522 tests, 70
minutes apart with coders editing between them), so suite growth is an
uncontrolled explanation. Two blocks in the same feed have a genuinely constant
tally and show the effect more strongly:

- `tests=1474`: **36.1 / 43.7 / 43.9 / 52.6 / 73.3 / 79.8 s** — 2.2x spread
- `tests=1541`: **33.8 / 34.2 / 34.4 / 43.4 / 50.4 / 69.4 / 78.1 / 89.6 s** — 2.65x

`CLAUDE.md:40` independently records CPU contention as a measured flake source,
and `Taskfile.yml:54-60` runs the component gates serially *because* of it.

## Solution

Make the claim fleet-aware, decided **inside the existing claim statement**.

A worker holding `n` active runs defers a queued run to an eligible, live peer
that has a free slot and is strictly less loaded. Resume affinity outranks the
spread. A run that has waited past a grace window is exempt from the spread
entirely. When no peer qualifies, behaviour is exactly what it is today.

## Decision log

**D1 — Server-side, not worker-side.** The rejected alternative was making
`worker.ts:174-176` sleep a poll after a successful claim. It is timing luck,
not placement, and it *slows* backfill on a single-worker fleet, which D3 names
as the common case. The server is the only place that can see the fleet.

**D2 — The predicate rides the claim's snapshot. ⟨corrected⟩** The first draft
argued that a service-layer read-then-decide would livelock because "two racing
workers both observe an idle peer and both defer". **That is false**: under a
strict comparison the minimum-loaded set is never empty, and a minimum-loaded
worker never defers. The real reasons to put the predicate in the statement are
narrower and still sufficient: no read-then-act window, no extra round-trip, and
the counts come from one consistent snapshot.

What it does **not** buy is mutual exclusion across workers. Verified by
`EXPLAIN (ANALYZE)`: the peer-load aggregates evaluate in `InitPlan`/`SubPlan`
nodes **before** `LockRows`, so two concurrent claims evaluate peers
independently. That is safe in the only direction that matters — an uncommitted
peer claim makes the peer look *emptier*, which can make *me* defer but can
never double-claim, since the run row's `FOR UPDATE SKIP LOCKED` is unchanged.

The mechanism is legal and was verified, not assumed: an `UPDATE … WHERE id =
(SELECT … AND NOT EXISTS (… aggregates …) ORDER BY … FOR UPDATE SKIP LOCKED
LIMIT 1)` parses, plans as a nested-loop anti-join, and returns the right
answer. `FOR UPDATE`'s aggregate restriction binds the locking query level only.

**D3 — Affinity wins, and the predicate is PER-ROW.** The spread applies to the
non-affinity path only: `(r.worker_id = @worker_id OR <spread allows>)`. Written
as a worker-level gate that does not reference `r`, it would filter **every**
candidate row including this worker's own affinity-held resumes, and a resumed
run would not return to its warm clone. Silent when wrong; SC3 is the only thing
that catches it.

Second-order and deliberate: a per-row predicate can skip an older run and take
a younger one (the older deferred to a peer eligible for it, the younger not).
This is a change to the FIFO ordering guarantee and is accepted.

**D4 — Fail-open, bounded in TIME, not just by condition. ⟨corrected⟩** The
first draft enumerated fail-open *conditions*. No condition list covers the case
where the state persistently and **correctly** says "an idle, live, eligible peer
exists" and that peer never claims — a wedged claim loop, a worker mid-roll, a
worker whose every claim dies in `assembleClaim`. The only existing backstop is
the queued health detector (`health_queued_seconds`, default 600s,
`api/internal/settings/settings.go:129`), which flags and does not fix.

So the spread carries a **grace**, mirroring `@affinity_cutoff`: a queued run
older than `@spread_cutoff` (~2-3 poll intervals) is exempt from the spread
predicate entirely. It is self-limiting — under load nothing is idle, so the
predicate does not fire. **This is what makes SC2 a testable invariant**
("every queued run is claimable within `spread_grace`") instead of "not stranded
in the scenarios we thought of".

**D5 — Eligibility is ONE predicate, used twice. Cross-references PRD #84.**
To answer "could peer P claim run R?", the spread must apply the same filter the
candidate scan applies to the claiming worker. Two hand-written copies drift:
one way defers to a worker that cannot claim (strand → D4); the other silently
stops spreading, with nothing red.

**PRD #84 adds `required_capabilities ⊆ worker_caps` to this same statement**
(#84 Solution §2; `api/internal/handler/worker_protocol.go:112-120` already
accepts-and-ignores `Capabilities` for it), and #84's pre-claim "no eligible
worker" state is indistinguishable from this PRD's deferral. **Decision (user,
2026-08-03): #216 lands first and writes eligibility as a single reusable
expression that #84 extends rather than duplicates.** Both PRDs carry a mutual
reference. Note a third copy already exists: `active_runs` is hand-written at
`runtime.sql:98` (`ListWorkersByUser`) and `:422` (`ListAllWorkers`), and those
drive the UI's "N/M runs" — if they drift from the placement predicate, the UI
explains a placement that did not happen.

**D6 — Liveness via `last_heartbeat_at`, not `workers.status`.** Compare against
`WORKER_HEARTBEAT_STALE` (default 45s, `api/internal/config/config.go:668`),
passed as a param mirroring `@affinity_cutoff`. `status` only moves when the
sweeper runs and lags by up to one sweep. No corpse window exists: a stale
worker's runs are requeued by the same test, so it stops being a target and
loses its `active_runs` in the same sweep.

**D7 — Structure the predicate as `NOT EXISTS`, never as scalar occupancy.
⟨measured — this refutes the natural implementation⟩** `workers.max_concurrent_runs`
is nullable by design (`00055_worker_max_concurrent_runs.sql:10`, "NULL when the
worker advertises no cap — an older image") and `worker_protocol.go:150-155`
drops out-of-band reports to NULL. A scalar occupancy comparison propagates that
NULL into three-valued logic and filters the row out. Measured on `postgres:17`:

| scenario | scalar occupancy | `NOT EXISTS` raw-count |
|---|---|---|
| claimer 1/2, peer 5 active + NULL cap (correct answer: CLAIM) | **0 rows, stranded** | claimed |
| **single-worker fleet, no peers at all** | **0 rows, forever** | claimed |

The second is total: `min()` over an empty set is NULL, and `0.5 <= NULL` is
NULL, not false. D3 names the single-worker fleet as the common case, so **the
shape that breaks first breaks completely**. `EXISTS` is two-valued by
construction, so fail-open becomes structural rather than something every
`COALESCE` must remember.

**D8 — Read `max_concurrent_runs` for TARGETING; still do not ENFORCE it.**
Deferring to a peer without reading its cap defers to a peer already at its own
cap: A (cap 4, holds 2) defers to B (cap 1, holds 1) because B holds fewer; B's
own semaphore refuses; repeat forever. On a heterogeneous fleet that is a steady
state, not a window. The peer filter therefore needs
`peer.active < peer.max_concurrent_runs`, and a **NULL cap makes a worker not a
deferral target** (never defer to capacity you cannot establish).

This is not what ADR-42 rejected. ADR-42 rejected the server *enforcing* a cap on
the claiming worker; reading a peer's advertised cap to decide whether it is a
plausible target is a different claim. M0 amends ADR-42 so it does not read as
contradicted.

**D9 — We spread SLOTS, and the motivation measures CPU. Say so.** `active_runs`
is slot occupancy. It diverges from load in three live ways: a run parked at
`awaiting_approval`/`awaiting_input` holds a slot and burns no CPU; a
`kind='judge'` run holds a slot and is a slim model call; and **chat is excluded
entirely (D10), so a worker running N chat sessions reports `active_runs = 0`
and is the most attractive deferral target in the fleet while genuinely busy.**
Slots remain the right proxy (already computed, matches `max_concurrent_runs`, a
load-based scheduler is scope creep). `workers.stats_cpu_pct` is explicitly
rejected as an input: it is DISPLAY-ONLY per PRD #49 Decision 5.

**D10 — Chat stays out.** `ClaimRun` already excludes `kind='chat'`; the chat
lane has its own budget via `ClaimChatRun`. The spread counts the run lane only.
Whether the chat lane wants the same treatment is out of scope — but see D9 for
the load consequence of that exclusion.

**D11 — Ship the index with the predicate, not after it.** No index serves
"active runs of worker X": `idx_runs_worker` is `(worker_id)` alone and
`idx_runs_claimable` is `(user_id, status, created_at)`, so the planner scans
every row in those statuses instance-wide and filters. A partial index on
`runs (worker_id) WHERE status IN (…) AND kind <> 'chat'` retires R2 in one
migration line. Number is a draft (live head is `00094`); renumber above the
head at landing per `CLAUDE.md`.

**D12 — Incidental doc fix. ⟨narrowed⟩** The comment at `runtime.sql:70-71`
describes `active_runs` as "claimed/running/awaiting_approval" while the SQL at
`:96` also includes `'awaiting_input'`. The first draft also cited `:91` as
evidence; that is **over-reach** — `:91` is the `busy` EXISTS, which the comment
describes separately at `:74-76` and for which it enumerates no statuses. Only
`:96` contradicts. Fixed in M1 per the fix-the-doc rule (the in-the-same-commit
obligation is stated in the user's global `CLAUDE.md`; the repo's own file scopes
fix-the-doc at `:554` to present- vs past-tense claims). `ListAllWorkers`
(`:404-422`) duplicates the subquery without prose; it is D5's third copy.

## Milestones

- [ ] **M0 — ADR + the #84 seam.** `adr/0216-fleet-aware-claim.md` (numbered by
      originating issue per repo convention). Records the eligibility predicate
      as a seam other code must respect, the fail-open/NULL discipline (D7), and
      amends ADR-42's "no server-side cap enforcement" so D8 does not read as a
      contradiction. Adds the mutual reference to PRD #84.
- [ ] **M1 — Fleet-aware claim predicate.** `ClaimRun` gains the spread,
      honouring D3 (per-row, affinity first), D4 (`@spread_cutoff`), D5 (one
      eligibility expression), D6 (heartbeat param), D7 (`NOT EXISTS`), D8 (peer
      free slot, NULL cap not a target). Plus D11's index and D12's comment fix.
      `ClaimRunParams` and its sole caller (`service.go:824-830`) change with it.
- [ ] **M2 — Live-DB tests, written for the failures that exist.** Three
      distinct classes, and the first is the one the original draft would have
      missed: **(a) steady-state starvation, single-threaded** — heterogeneous
      caps (D8's A/B livelock), NULL cap (D7), single-worker fleet (D7),
      past-grace exemption (D4); **(b) the differential eligibility pin (D5)** —
      for a matrix of (worker, run) pairs, `peer-eligible(w,r)` must hold exactly
      when `w`'s own `ClaimRun` returns `r`, with cases authored to
      discriminate (docker worker × allowlisted repo, × non-allowlisted repo, ×
      repo-less judge run, non-docker × non-allowlisted); **(c) concurrency** —
      N workers claiming M runs simultaneously, no run unclaimed, none twice.
      Via `./e2e/run-store-it.sh`, which picks a random port and a PID-unique
      container (so concurrent sweeps do not collide) and **already hardcodes
      `-p 1`** at `e2e/run-store-it.sh:72` — it is not an argument to pass.
      **Note (b) cannot be exercised on the current fleet**: both workers are
      `docker: true` and `DockerRepoAllowlist` (`service.go:817`) takes no worker
      or user argument — it is one global settings read — so per-run eligibility
      only diverges on a mixed docker/non-docker fleet. The matrix must be
      synthetic, or it will "confirm" D5 against a fleet that cannot exhibit it.
- [ ] **M3 — Worker-side confirmation.** Expected answer is **no change needed**:
      a deferral returns 204 and `worker.ts:176` already sleeps a poll on a
      non-claim. Stated as an expected finding so the milestone is falsifiable.
- [ ] **M4 — Make a deferral distinguishable from an idle queue.** Today it is
      not: `Service.Claim` turns `pgx.ErrNoRows` into `nil, nil` → 204
      (`service.go:833-838`), so the worker, the API log and the operator all
      see "empty queue". Worse, the health detector then **mis-labels** it —
      a past-threshold queued run is flagged `waiting_worker`, "waiting for a
      worker to pick up this run" (`workersvc/health.go:48,63-64,203-210`),
      which is actively wrong when a worker asked and the server refused. Needs
      a mechanism, not a log line: either a diagnostic query on the `ErrNoRows`
      path when the user has queued runs, or a reason column returned by the
      claim statement. Both change the claim path's query count, so the choice
      is a Decision-Log addition.
- [ ] **M5 — End-to-end on the real fleet, INCLUDING a pre-change control.**
      Two workers, two runs queued together, one each. Then saturation: three
      runs on two cap-2 workers place 2+1 or 1+2, never 3+0, never a strand.
      **M5 owes the observation the Problem section does not have**: run the
      two-runs-two-idle-workers case *before* M1 lands and confirm both go to one
      worker. Without that control this PRD fixes a bug nobody has watched
      happen. **M5 cannot settle the raw-vs-occupancy question** (R3) — under
      equal caps the two give identical answers on every input, so that belongs
      in M2's synthetic heterogeneous fixtures.
- [ ] **M6 — Retire the workaround.** Confirm `workers.maxConcurrentRuns` no
      longer needs lowering, and update the chart docs. This is the milestone
      that delivers the user-visible ask: no values edit.
- [ ] **M7 — Docs.** Worker/run-lifecycle docs describe placement, following the
      existing frontmatter rules.

## Success criteria

1. Two runs queued simultaneously against two idle workers land one per worker,
   repeatably.
2. **Every queued run is claimable within `spread_grace`** — a bounded invariant
   (D4), provable by M2(a), not an unbounded "never stranded".
3. A resumed run still returns to its prior worker within the affinity grace
   (D3), unchanged.
4. `peer-eligible(w,r)` agrees with `w`'s own claim eligibility on every case in
   M2(b)'s discriminating matrix (D5).
5. A single-worker fleet and a NULL-cap peer both still claim (D7).
6. A heterogeneous fleet reaches a stable placement with no A/B livelock (D8).
7. `workers.maxConcurrentRuns` no longer has to be 1 to get spreading, and burst
   capacity on a single-worker fleet is unchanged.
8. An operator can tell a deferral from an idle queue (M4), and a deferred run is
   not reported as `waiting_worker`.

## Risks

- **R1 — Starvation is the failure mode, and it is STEADY-STATE, not a race.
  ⟨corrected⟩** The first draft said "a single-threaded test will pass against a
  livelocking implementation". **That is backwards for the bugs that exist**: D7's
  NULL/empty-fleet strand and D8's A/B livelock both reproduce instantly in a
  single-threaded test written for them, and would have been missed by a test
  suite aimed only at concurrency. M2 splits (a) steady-state from (c)
  concurrency for exactly this reason.
- **R2 — Claim-path cost, and it is SMALLER than the first draft implied.**
  Measured: with 2003 queued runs the per-worker aggregate hoists into a
  `SubPlan` under `Materialize` at `loops=1` while the anti-join runs
  `loops=2003`, so cost is O(peers), not O(peers × queued). That is a plan
  choice, not a guarantee — if the count ever becomes correlated with `r` (e.g.
  "peer's active runs on this repo"), the hoist is lost. D11's index addresses
  the real driver. The first draft said "`ClaimRun` runs every 3s per worker";
  that **overstates the hot path** — a worker at capacity takes `worker.ts:140`'s
  branch and never calls `claimRun()` at all, so the cost lands on *idle*
  workers, which is the cheap case for a subquery.
- **R3 — Over-fitting to a homogeneous fleet.** "Fewer active runs" is a proxy
  for "less loaded" and is wrong when caps differ: a cap-8 worker at 2/8 should
  take work before a cap-2 worker at 1/2, and raw count gets that backwards.
  Once D8's free-slot filter is in, this is only a **tie-break**; compute it by
  integer cross-multiplication (`peer.active * mine.cap < mine.active * peer.cap`)
  — exact, no float ties, no division. `00055` has no CHECK, so do not rely on
  the handler's clamp from SQL.
- **R4 — Deferring to a peer with a broken credential converts imbalance into a
  DEAD RUN.** A peer pinned to a deleted or undecryptable Anthropic credential
  does not merely fail to claim; it claims and fails the run terminally
  (`errCredentialUnavailable` → `MarkRunFailedByID`, `service.go:857-866`). This
  is the one eligibility dimension where deferring wrongly is worse than not
  spreading. Treat it as part of D5's predicate.
- **R5 — Double deferral windows on requeue.** `RequeueRunsOfStaleWorkers` keeps
  `worker_id` and bumps `updated_at`, restarting the affinity grace
  (`WORKER_AFFINITY_GRACE`, 2m). A requeued run can then serve an affinity hold
  *and* a spread deferral back to back. D4's time bound is what keeps the total
  finite.
- **R6 — Roll/upgrade state is not an eligibility input.** A worker mid-roll
  heartbeats until its pod goes, so D6 does not exclude it. Accepted as a
  non-goal (self-corrects on the next poll) rather than omitted silently. The
  vault gate is per-user and does not discriminate between peers.
- **R7 — Interaction with PRD #215**, which raises concurrency *within* a run
  and so raises per-run CPU demand. Validate the two together.

## Open questions

- M4's mechanism: diagnostic query on the empty path, or a reason column on the
  claim statement? The second is cheaper per call and changes the statement's
  shape, which #84 also wants to change.
- Does the chat lane (`ClaimChatRun`) want the same treatment? Out of scope
  (D10), but D9 shows chat load is invisible to the run-lane predicate.
