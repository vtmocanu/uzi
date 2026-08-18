# Worker pause (quiesce) — stop claiming new runs without killing in-flight work

Line references are against addffc31.

## Rationale

There is no graceful way to take a worker out of rotation: `DeleteWorker` 409s while any
non-terminal run points at the worker (`api/internal/handler/workers.go:641` — "worker has
active runs; cancel them before deleting it"), container restart is abort-then-drain
(`agent/src/main.ts` SIGTERM → `Runner.shutdown()`), and the workers table's only state is
liveness (`00020_workers_runs.sql:14`: `CHECK (status IN ('offline','online'))`). So upgrading
an external worker, rotating its credential, or retiring it means cancelling or aborting live
runs; a pause lets it finish what it holds while claiming nothing new.

## Sketch

- Migration: `workers.claims_paused_at timestamptz` (NULL = accepting; timestamp matches the
  `online_since` idiom). The register upsert must preserve it across reconnect — same CASE
  idiom `online_since` already uses — so a crash-looping worker cannot un-pause itself.
- Eligibility expressed ONCE, SQL-side, per ADR 0216: extend `fn_worker_can_claim` (or a
  sibling predicate) so `ClaimRun`'s own gate and the fleet-spread peer subquery
  (`runtime.sql:567-579`) agree — a Go-only gate would leave a paused worker a valid
  deferral target, parking runs for the spread grace window. The same gate applies to the
  chat lane (`ClaimChatRun`, `queries/chat.sql`): paused means no new work of any kind.
- Resume-affinity exemption: a paused worker may still claim runs already bound to it
  (`r.worker_id = @worker_id` — limit_wait promotions, requeues), so its own parked runs
  are not stranded and `docs/run-limit-wait.md`'s same-worker promise holds. "New work"
  means unaffiliated runs only.
- API: `PATCH /api/workers/{id}` gains optional `claims_paused` (absent key = untouched,
  the handler's existing rule); worker list DTOs expose `claims_paused_at` — `busy` and
  `active_runs` are already computed, so "paused — 1 run finishing" vs "paused — idle,
  safe to restart" costs no new backend work.
- Queued-run health: `queuedReason` / `CountOnlineWorkersWithFreeSlotForUser`
  (`workersvc/health.go:63`) learn a "workers are paused" reason — today a fully-paused
  fleet would read as idle capacity mysteriously not picking work up.
- CLI: `uzi worker pause|resume <id>`; PAUSED marker in `render.go`; `docs/cli.md` synopsis
  plus a pause → drain → restart procedure in `docs/worker-upgrades.md`.
- Web: pause/resume control + paused pill in `WorkersSettings.tsx`; mock-mode parity with
  two fixtures (paused-idle, paused-with-run-finishing) since those render differently.

## Caveats / out of scope

- Out of scope: auto-unpause, maintenance windows/schedules, admin pausing another user's
  worker, notification when a paused worker reaches idle.
- Hosted workers are the obvious first consumer (controller-driven upgrade rolls); whether
  the controller drives pause automatically is left to the PRD.
