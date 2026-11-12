# PRD #4207: Worker hibernation: automatic hibernation of idle workers

**Epic**: #4204. **Status**: Draft. **Blocked by**: PRD #4205 and PRD #4206, both merged. The edge on #4206 is a release dependency, not a code one: automatic hibernation must not go live before the promised cleanup (user decision; see D5).

## Problem

With PRD #4205 an admin can hibernate workers by hand, but nobody watches which workers have been idle for weeks.

## Outcome

When the admin turns on automatic hibernation, uzi hibernates every persistent hosted worker that has had no run activity for the configured idle threshold, through PRD #4205's admission and PRD #4206's cleanup. The default threshold is 14 days. This includes a user's last or only worker. When something blocks hibernating an idle worker, Admin health says which worker and why.

**Recorded interpretation (user decision 2026-11-12).**

- One idle threshold applies to every persistent worker. There is no separate "last worker" rule.
- "Idle" means no run activity of any kind on that worker: an assigned run of any kind (issue, prompt, schedule-fired, sweep, chat, judge, cross-check, CLI task).
- Login is not a signal: schedules run without logins, and CLI users may never log in through the web.
- Users cannot opt out.

Acceptance examples:

1. **Hibernate after 14 days.** Automatic hibernation is on, threshold 14 days. A worker whose last run ended 15 days ago is hibernated on the next policy pass. A worker whose last run ended 13 days ago stays awake.
2. **Wake resets the clock.** A hibernated worker woken by a scheduled sweep runs it. Its idle clock restarts when that run ends.
3. **Blocked idle worker.** A worker idle for 20 days but holding a recovery custody hold is not hibernated. Admin health shows "worker <name> idle 20d, hibernation blocked: recovery custody hold", and the warning clears once the hold is released and the worker hibernates.

## Out of scope

- Capability-aware warm coverage, which keeps one usable worker awake per user. A later child of the epic.
- PVC retirement.
- Per-user overrides.

## Modules and seams

**Idle clock, derived and not stored.** This adds no write to the claim hot path and needs no backfill.

- **Definition.** A worker is idle when it has **no** non-terminal run with `worker_id` = this worker (queued, paused and waits included), and its idle anchor is at or before `now - threshold`.
- **Idle anchor:** `GREATEST(max(runs.finished_at) for this worker, workers.last_wake_settled_at, workers.created_at)`.
- **Index:** `idx_runs_worker` (`00020:56`); `finished_at` is at `00020:49`.
- **Concurrent runs.** One run ending while another is still assigned leaves the worker non-idle by definition.
- **Requeues.** A requeued run moves its `worker_id` to the new worker, so the predicate follows the current assignment.
- **Wake grace.** `last_wake_settled_at` is set by PRD #4205's wake settlement. A manually or demand-woken worker therefore gets a full threshold of grace and is not re-hibernated on the next pass.
- **Signals.** It comes from run state, never heartbeats (which continue throughout idle) and never login.

**Settings.** Pattern: `release_check_enabled` / `release_check_interval` (`api/internal/settings/settings_release_check.go`, `UpdatesSettingsCard.tsx`).

- `worker_hibernation_auto`: bool, default false. It is effective only while PRD #4205's `worker_hibernation_enabled` is on.
- `worker_hibernation_idle_after`: Go duration, default `336h`, validated to [1h, 2160h].
- Both are shown on the PRD #4205 settings card with their units.

**Policy pass.** An api sweeper pass, at most once per minute:

- It selects persistent hosted workers in state `none` that are idle by the derived definition above.
- It calls PRD #4205's hibernate admission for each, which re-checks the predicates under the row lock.
- Admission refusals are recorded per worker as `hibernation_blocked_reason`.
- It is bounded by the fleet size (≤512) and does at most N=10 admissions per pass, to pace scale-downs.
- Candidates are ordered deterministically by `idle anchor` ascending, then id. A candidate refused within the last hour moves to the back of the order, so repeatedly blocked workers cannot starve the rest of the fleet.

**Admin health.** A new **instance-scoped** `fleet.hibernation` check in `api/internal/healthsvc/checks.go`, with its `docs/admin-health.md` row and `apitypes/health.go` entry.

- **What it warns about:** each worker idle past the threshold whose admission was refused, with the reason (custody, cordon, maintenance, roll, or a cleanup still in progress).
- **Long-parked workers:** a worker whose only assigned run is parked (`recovery_wait`, `paused`, `limit_wait`) is non-idle by definition. Once the parked run is older than the threshold, it is listed as "blocked: assigned run parked (RUN, status)", so a long park is visible rather than silently keeping the worker awake.
- **Bounds:** evidence is bounded by `maxEvidenceBytes`, and the list is capped at 50 workers.

## Testing decisions

- **LiveDB.**
  - The derived idle predicate holds for every run kind, including chat and judge, and treats a queued, paused or waiting run as non-idle.
  - It is not set when one run ends while another stays assigned.
  - A settled wake resets it, so there is no immediate re-hibernation.
  - A requeued run counts against its new worker.
  - Blocked candidates do not starve the others.
  - A parked run older than the threshold is listed as a blocker.
  - The pass hibernates at threshold + 1 minute and not at threshold - 1 minute.
  - The pass is off when either setting is off.
  - A refusal is recorded and surfaced.
  - Pacing at 10 per pass.
- **Health.** Check unit tests following the existing `fleet.*` tests.
- **Web.** Vitest for the settings fields and validation errors.

## Milestones

**M1: Idle workers hibernate themselves.** Blocked by: none within this PRD.

Covers the clock, the `last_wake_settled_at` column (written by #4205 settlement), settings, policy pass, refusal recording and docs.

Acceptance: examples 1-2.

**M2: Admin health names blocked idle workers.** Blocked by: M1.

Covers `fleet.hibernation` and its docs.

Acceptance: example 3.

## Decision Log

- **D1. The idle clock comes from run transitions,** not heartbeats or login. Heartbeats never stop while idle. Login misses schedules and CLI use.
- **D2. One threshold, including the last worker** (user decision). Warm coverage is a later refinement.
- **D3. Live admin settings are the single source of truth.** No Helm value, so it is never unclear which value is in effect.
- **D4. The pass is paced at 10 admissions per pass,** so a freshly enabled policy does not scale down the whole fleet in one tick.
- **D5. Keep the edge on #4206.** The user asked for cleanup at hibernation. The architect review called the edge false as a code dependency, which is correct, but it is kept deliberately as a release dependency.
- **D6. Derive idleness rather than storing it.** This keeps the claim path free of a new write and avoids a backfill, at the cost of one indexed aggregate per policy pass.
