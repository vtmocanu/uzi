# Run queue priority — background runs yield to interactive work, with a manual expedite

## Rationale

Every finished issue/ci_fix run auto-enqueues a judge run (`maybeEnqueueJudge`,
`api/internal/workersvc/judge_enqueue.go:44`), and self-improve/scheduled prompt runs
join the same lane — the claim query's only kind carve-out is `AND r.kind <> 'chat'`
(`api/internal/store/queries/runtime.sql:511`). The queue has no priority *term*:
`ClaimRun` orders by `COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC`
(`runtime.sql:561`), so on a busy factory a human pressing Start run waits behind
accumulated background retrospection on a worker whose `WORKER_MAX_CONCURRENT_RUNS`
defaults to 1. A kind-derived priority (judge/self_improve demoted) plus a per-run
manual expedite fixes this, with an age-based fail-open so background work can never
starve.

## Sketch

- Migration: nullable `runs.priority SMALLINT` (manual override only) + an
  `IMMUTABLE fn_run_priority(run_kind, priority, is_stale)` SQL function — the same
  reused-expression idiom as `fn_worker_can_claim` (migration
  `00113_fleet_aware_claim.sql`), so claim and UI can never disagree.
- Kind defaults live in the function (judge/self_improve demoted, everything else
  normal), so none of the 7 `INSERT INTO runs` sites change and no backfill is needed.
- `ClaimRun`: add the priority term to ORDER BY *between* affinity and `created_at`
  (`affinity DESC, fn_run_priority(...) DESC, r.created_at ASC`). WHERE untouched —
  eligibility and fleet-spread predicates unchanged.
- Starvation guard: `is_stale = r.created_at < @background_grace_cutoff`, a new
  `RUN_BACKGROUND_GRACE` knob (mirroring `@spread_cutoff` / `WorkerSpreadGrace`,
  default ~15m) collapses the demotion to normal priority.
- Expedite verb: `PATCH /api/runs/{id}/priority` (owner-scoped, `queued` only) in
  `api/internal/handler/runs.go`; CLI `uzi run expedite` in `api/cmd/uzi` (+ SKILL.md
  and `docs/cli.md`); web: Expedite action + priority pill on queued rows in
  `web/src/pages/RunsList.tsx` / `RunView.tsx`.
- `queuedReason` (`api/internal/workersvc/health.go`) learns to say a demoted judge
  run is *deprioritized*, not stuck — today it would read as a fault.

## Caveats

- The queue today has no priority **term**, but it is not strictly FIFO either:
  eligibility predicates already let a worker skip an older run
  (`prds/216-worker-load-balancing.md:144` records this as accepted).
- Adjacency: open PRDs #216 (fleet spread) and #84 (capability-aware scheduling) both
  own edits to the same `ClaimRun` statement; a priority term should coordinate with
  in-flight work there rather than land blind.
- When reasoning about the lane, cite the `kind <> 'chat'` predicate — the prose
  comments describing the run lane as "issue/ci_fix" are under-inclusive (judge,
  self_improve and prompt runs ride it too).
