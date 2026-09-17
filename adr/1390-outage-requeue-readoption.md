# ADR-1390: Api outage recovery — boot grace, heartbeat re-adoption, claim dedupe

**Status**: Accepted (PRD #1390 M1-M4 committed on this branch; M5 docs/ADR/spec and
M6 hosted acceptance are the maintainer's, out of band).
**Date**: 2026-09-17
**Issue**: [vtmocanu/uzi#1390](https://github.com/vtmocanu/uzi/issues/1390)
**PRD**: [prds/1390-outage-requeue-readoption.md](../prds/1390-outage-requeue-readoption.md) —
carries the full Decision Log (D1-D11), the milestone breakdown and the verified code
anchors; this ADR carries only the seams other code must respect and why they are shaped
as they are.

## Context

An api outage of ~50 minutes left two issue runs executing on a healthy hosted worker,
yet the api's picture of them went wrong the moment it returned and stayed wrong for
hours. Three independent failures, all downstream of one fact — worker ownership was
`worker_id` only, with no way for the worker to tell the api "these are still mine and
this is their state":

1. The boot sweep ran a full stale-worker pass immediately at api start, so every worker
   (stale only because the api had been gone) had its live runs flipped to `queued`,
   charged a re-queue, and — with `RUN_MAX_REQUEUES` defaulting to 1 — its whole re-queue
   budget spent by a single outage.
2. Nothing could put a re-queued-but-still-executing run back to `running`/its gate short
   of the attempt's next state report, which a lead deep in one long subagent turn does
   not send for hours; the board showed `queued`, health read the silence as `stalled`.
3. The affinity worker re-claimed the run it was already executing, accumulating custody
   holds each time `SweepClaimedNeverStarted` reset the parked second attempt.

The live fix was a hand-written `UPDATE runs SET status='running'` — the database
intervention #1349 M8 says an owner must never need. This ADR records the durable
seams that make that intervention unnecessary. All three api-side changes ride one shared
worker-reported snapshot (the wire contract shared with #1391); the run lane only (chat is
out, D10).

## The seams

### D1 — Boot grace is a listener-anchored delay, one knob, `0` = today

`SWEEPER_BOOT_GRACE` (`config.SweeperBootGrace`, default 60s = the stale window plus one
heartbeat interval; `0` reproduces today's immediate behaviour) gates exactly the three
stale-worker passes — `MarkStaleWorkersOffline`, `FailRunsOfStaleWorkersOverCap`,
`RequeueRunsOfStaleWorkers`. It is anchored at the moment the worker-facing **listener**
became ready (`Service.SetReadyAt`, stamped by `api/cmd/server/main.go` after all binds
succeed), **not** at process start: a process-start anchor would age while workers still
cannot connect. Every other sweep pass and every extra pass runs at boot as before. A
worker that does not heartbeat within the grace is genuinely dead and is swept on the
first post-grace tick. A live partition with the api up is the reconciliation's job (D4,
D9), not the grace's — the seam a future change must not break is that the grace covers
*only* the "api was gone, so every worker looks stale" case.

### D2 — Re-queue refund only with provenance (`runs.stale_requeue_generation`)

`RUN_MAX_REQUEUES` exists to stop a run that keeps killing its worker from re-running
forever; a re-adopted run was never re-run, so its charge must be refunded — but a
`queued` row alone does not record whether *this* transition was charged (the
claimed-never-started reset also writes `queued` without charging). The provenance column
`runs.stale_requeue_generation` records the `claim_generation` that
`RequeueRunsOfStaleWorkers` charged. The heartbeat restore refunds
(`requeue_count = GREATEST(requeue_count - 1, 0)`) **only** when
`stale_requeue_generation = claim_generation`, and both the restore and `ClaimRun` clear
it, so a legitimate earlier loss is never refunded and a double-refund is impossible. Any
future writer that decrements `requeue_count` on a worker's say-so must gate on this same
provenance.

### D3 — The active-run snapshot is a full-replacement table under a worker epoch and a register nonce

The worker reports `{snapshot_epoch, active: [{run_id, claim_generation, phase,
terminal_pending}], pending_overflow}`. The api stores the latest per worker in
`worker_active_runs` (migration `00235_worker_active_runs_snapshot.sql`), keyed
`(worker_id, run_id)`, carrying `claim_generation`, `phase`, the lease
(`terminal_pending`, `terminal_pending_until`), `snapshot_epoch` and `reported_at`. The
shape is forced by four requirements a lighter design cannot meet at once:

- **`ClaimRun` must read it in SQL across all workers by run** (D8) — so it is a table,
  not in-memory worker state, with an index on `(run_id, claim_generation)`.
- **An api restart must not forget it** — so the per-worker epoch (`workers.snapshot_epoch`)
  and the `workers.snapshot_register_nonce` are persisted; the nonce is what makes the
  epoch trustworthy across a worker restart *and* an api restart, which is the very moment
  it is checked.
- **The claim can beat the first post-outage heartbeat** — so the same snapshot rides the
  claim request (`ClaimRequest.active_snapshot`), not just the heartbeat.
- **Applied atomically as full replacement, ownership-validated.** `ReplaceWorkerActiveRuns`
  deletes every row of the worker and inserts the new set in one transaction, and only when
  the snapshot's epoch exceeds the stored one (a delayed older heartbeat arriving after a
  newer claim is discarded — the register nonce, not the epoch, is what rejects a
  previous-process snapshot after a restart). Full replacement is what removes ended
  attempts; only entries for runs the authenticated worker owns are ever persisted or
  locked (a worker can only describe its own runs), so a buggy or compromised worker cannot
  suppress a sibling's claim. An invalid / stale-epoch / wrong-nonce snapshot is **ignored**,
  never applied as empty (an empty snapshot would drop every lease); a bad **heartbeat**
  snapshot still refreshes liveness (never a 400), a bad **claim** snapshot fails the claim
  closed. Freshness for the claim exclusion (D8) is the snapshot's own `reported_at`, not
  the worker's heartbeat, so a worker whose snapshots fail validation cannot keep stale rows
  protected by its bare liveness.

### D4 — Both re-adoption directions fenced on generation + `claim_released_at`; the missing path fenced on `status_since` with margin

Reconciliation runs inside `Service.Heartbeat`, in one transaction after `HeartbeatWorker`
and the snapshot replacement, both directions carrying `kind <> 'chat'`:

- **`ReadoptRunsFromSnapshot`** restores a `queued` run-lane run owned by this worker whose
  listed generation equals `runs.claim_generation` and whose `claim_released_at IS NULL` to
  its listed phase.
- **`RequeueRunsMissingFromSnapshot`** requeues a `running` run-lane run owned by this
  worker that the snapshot does not list at its current generation, with no unexpired lease,
  `claim_released_at IS NULL`, and `status_since < now() - (WORKER_HEARTBEAT_STALE +
  WORKER_HEARTBEAT_INTERVAL)`.

Generation-equality plus `claim_released_at IS NULL` is #1247's rule for every
worker-driven write (its credential-switch release sets `claim_released_at`, cleared only
by `ClaimRun` while it advances the generation); honouring it here keeps re-adoption from
reviving a run #1247 deliberately released, and keeps a stale generation from masking an
absent newer one. The missing-run `status_since` margin (the stale window plus one
interval) is the 30s HTTP timeout plus the 15s cadence with slack. The consequence a
reader must keep straight: "within one worker heartbeat" is the **re-adoption** promise
(SC1); the **missing-run** requeue is the stale window plus up to two intervals (SC2), not
one heartbeat. Custody is untouched by re-adoption (a re-adopted run keeps the same
generation and its one live hold, #1349); the missing path follows the existing requeue
exactly (D6).

### D5 — Restore to the exact phase, bank the queued interval, never blanket `running`

The stale requeue converts held states too (`awaiting_approval`, `awaiting_input`,
`awaiting_followup`), and the worker keeps their `execute` promise and their gate content
columns alive across the requeue. Restoring a blanket `running` would hide a gate the
worker is still waiting on. So the restore sets `status` to the phase the worker reported
(the only party that knows it), banks the `queued` interval into `budget_paused_seconds`
when that phase is `awaiting_approval`/`awaiting_input` (as the requeue did for the park
interval before it), and leaves the held-state content columns alone — restoring the status
is enough to restore the gate. This is the heartbeat-side twin of #1247's claim-time
`resume_phase`: the same run can be recovered by whichever signal arrives first.

### D8 — Server-side, global, pre-claim dedupe is the mechanism; the worker's run-id set is a loud assertion only

A claimant-side check after the claim is too late (the claim has already advanced the
generation and opened a custody hold) and blind to a sibling claiming past the affinity
ceiling. So `ClaimRun` itself excludes, inside candidate selection and before the `hold`
CTE opens custody:

- any run a **fresh** snapshot (`reported_at >= now() - (WORKER_HEARTBEAT_STALE +
  WORKER_HEARTBEAT_INTERVAL)`) lists at the run's current generation, or that any snapshot
  lists under an unexpired terminal-pending lease (D11);
- any run in the claimant's **own request snapshot** (so the exclusion holds even when the
  claim beats the first heartbeat);
- any run owned by a worker under an unexpired `pending_overflow` closure (D11).

To make the request-snapshot exclusion race-safe, the claim transaction first locks its own
listed runs `FOR UPDATE` (only its own; a foreign entry is dropped, never locked) before
replacing the snapshot and selecting a candidate, so a sibling's concurrent `FOR UPDATE SKIP
LOCKED` claim skips those rows until the replacement commits. The worker's claim loop keeps a
run-id `Set` purely as a loud assertion — a returned id already live is logged at error and
not executed — never as the dedupe itself.

### D9 — Over-cap failure waits for a second stale window and serialises with the heartbeat

With `RUN_MAX_REQUEUES = 1`, a run requeued once and then hitting a 46s partition would be
**terminated** before any heartbeat could re-adopt it, and terminal cannot be undone. So
`FailRunsOfStaleWorkersOverCap` requires the worker to have been stale for two consecutive
stale windows (`last_heartbeat_at < now() - 2 * WORKER_HEARTBEAT_STALE`) — the requeue
itself still fires after one — and takes the worker row `FOR UPDATE` in its subquery,
re-checking staleness after any concurrent `HeartbeatWorker` commits, so a heartbeat that
lands between the staleness read and the terminal write wins. `RequeueRunsOfStaleWorkers`
takes the same lock. Net effect: a worker that is really gone is failed one stale window
(45s) later than before; a live worker gets that window to return
(`docs/configuration.md`'s `RUN_MAX_REQUEUES=0` sentence moves to this two-window rule).

### D11 — `terminal_pending` is a server-side, worker-scoped lease, plus a worker-level `pending_overflow` closure

#1391's guarantee is about an outcome already journaled to the worker's disk, which outlives
the worker's heartbeat; protecting it only while the heartbeat is fresh would let a dead
worker's pending completion be overtaken by a re-claim and then rejected by #1247's fence
when it finally replays. So a `terminal_pending` entry carries a server-side lease
(`terminal_pending_until = now() + TERMINAL_PENDING_LEASE`, default 1h, refreshed by every
snapshot that still lists it). **Every** path that could move the run out from under the
pending replay honours the lease: the missing-run requeue, both stale-worker passes,
register's orphan fail/requeue, `SweepRunningTimeout`, the persist-failure auto-stop,
`SweepClaimedNeverStarted`, and the claim; register preserves the leased rows themselves.

The lease predicate **as it landed is worker-scoped** — `NOT EXISTS (… WHERE a.run_id =
runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending AND terminal_pending_until >
now() AND a.claim_generation = runs.claim_generation)` (and `a.worker_id = r.worker_id` in
the claim). The `a.worker_id = runs.worker_id` conjunct is a hardening applied after the M2a
audit over the PRD's original unscoped predicate; it is sound because a run's snapshot row is
only ever its owner's, and it keeps every lease consistent with the sweep predicates. A
future edit to any of these paths must carry the worker-scoped conjunct, not the unscoped
form.

Because an outcome the worker could not fit in its snapshot has **no row of its own to
lease**, an overflow is closed at the worker level instead: `workers.pending_overflow` /
`pending_overflow_until` (same clock as the leases). While unexpired it closes every run
that worker owns to every claimant (sibling past the affinity ceiling included), refuses the
flagged worker any claim at all (including an unassigned run, via the claimant-side service
guard), and makes every D11 transition skip every run that worker owns; it is cleared only by
a valid unflagged snapshot or by expiry. The lease and overflow expiries share one clock and
are the single backstop, so a worker that never returns still frees its runs.

## Operational seams

- **`UZI_ACTIVE_SNAPSHOT_DISABLED` — the rollback lever.** An api started with this set omits
  `active_run_snapshot` from the register `protocol_features`, decodes heartbeats strictly as
  before, and stores no snapshots. It turns the whole snapshot machinery off in one env var
  (M1's boot grace is independent and stays on), and is the rollback the e2e phase's api-outage
  simulation exercises. Turning it on under a running fleet is safe: workers stop sending the
  field on the next register.

- **D7 — negotiated at register, degrades on a 400.** The worker sends the snapshot only after
  seeing `active_run_snapshot` in the register response's `protocol_features`. Because the
  heartbeat decoder rejects unknown fields (a 400), and register is one-shot per worker
  process, an api rolled back *under* a running worker would 400 every heartbeat forever — a
  lost heartbeat is worse than the bug. So a 400 to a heartbeat carrying the field triggers one
  retry with **every** negotiated heartbeat extension stripped, and on success the worker clears
  its entire cached server-feature set until its next process start (its next register). A lost
  heartbeat to the field is never acceptable; this seam is why an old or rolled-back api is safe
  under a new worker.

## Consequences

- An api restart of any length no longer disturbs a run on a still-live worker: it is restored
  to its exact status within one heartbeat, with `requeue_count` unchanged, park time banked,
  and no new custody hold. A worker that genuinely died still has its runs requeued after the
  stale window and failed only after a second window. This closes the "queued work resumes
  without database intervention" half of #1349 M8 (verified by M6).
- `runs.stale_requeue_generation` is now the only sanctioned provenance for refunding a
  re-queue charge; the schema also gains `worker_active_runs`, `workers.snapshot_epoch`,
  `workers.snapshot_register_nonce`, `workers.pending_overflow` and
  `workers.pending_overflow_until`. Any future stale/timeout/auto-stop/claimed-never-started
  writer, and `ClaimRun`, must honour the worker-scoped terminal-pending lease and the
  worker-level overflow closure — they are not optional add-ons but invariants of the recovery
  model.
- The active-run snapshot is a two-party wire contract shared with #1391: #1390 owns the api
  side (storage, reconciliation, dedupe, leases); #1391 owns the worker-side liveness of an
  attempt and the pending-outcome replay under the lease. Whichever landed first added the
  register `protocol_features` mechanism; the other adds its token.
- Chat is deliberately out (D10): its claim carries no generation, so it cannot join an
  exact-generation contract without one. Chat keeps the boot grace and today's behaviour; a
  follow-up adds a generation to the chat claim before admitting it to the snapshot.
- The operator surface now shows, per worker, what it reports it is executing and in which
  phase (`uzi worker list` / `uzi admin workers`, M2c), so a split-brain is visible rather than
  inferred from pod logs.
