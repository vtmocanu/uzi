# PRD #3454: Instance maintenance mode (drain uzi before infrastructure work)

**Status**: Done (M1 and M2 delivered, 2026-10-26). Priority high. Reviewed by buddy and two architecture reviewers (2026-10-25). Live hosted acceptance is tracked separately in #3465.

## Problem

An admin who needs to drain Kubernetes nodes, upgrade the database or otherwise take the platform down has no way to stop uzi from starting new work while letting current work finish. The per-worker manual cordon (#3308, migrations `00355`/`00357`) closes intake on one persistent worker at a time, only for that worker's owner, and never for ephemeral workers. Today the admin has to cordon every worker by hand (impossible for workers they do not own), pause every user's schedules, and still watch the ephemeral provisioner start new pods for queued runs on the nodes being drained. There is also no single place that says "nothing is running or held; it is safe to proceed".

## Outcome

An admin turns on **maintenance mode** for the whole instance, with an optional reason. While it is on:

- **New work queues and does not start.** A run that has never been claimed (any kind: issue, prompt, sweep, schedule, chat, chat Continue, judge, ci_fix, mr_rework, task review and then-fix phases, `/api/v1` job, recovery successor, Slack-created runs and chats) is created as today and stays `queued`. No worker claims it and the ephemeral provisioner starts no pod for it.
- **Admitted work finishes.** A run already claimed at least once keeps running, resumes after approval, input, follow-up, limit, pool, recovery or wall waits, and may be requeued and re-claimed by any worker that accepts it, ephemeral replacements included. Cross-check children count as admitted (ClaimRun already requires their lead to be admitted and live).
- **Everyone can see it.** Every authenticated user sees a banner. A queued run that maintenance is holding back says so on its card, in `uzi run get`, and in `/api/v1` job responses. Creating a run during maintenance returns the normal success plus a notice.
- **The admin sees when it is clear.** A readout reports `active`, `held`, `custody` and `queued` counts with the runs behind them, each held or custody item tagged `progressing` or `needs_owner` with its owner. It reads **clear** only when active, held and open custody are all zero in its snapshot (D3: a claim already in flight at enable can still appear afterwards, so the admin re-checks before evicting).

Turning it off lets the queue start, bounded by existing worker slots. Nothing is lost or re-created.

"Clear" can require owner action: a run waiting for approval without auto-approve, a budget-exhausted park, an exhausted recovery requeue, or a custody hold that only its owner can discard (#2506, #2494, #2969, #2544, #3031 document long-lived holds). An admin cannot dispose of another user's holds; the readout names the owner to contact.

Acceptance examples:
1. An admin enables maintenance with reason "node upgrade". User A labels an issue `uzi`; the run is created `queued` and its card reads "waiting: uzi is in maintenance mode (node upgrade)". No worker claims it and no ephemeral pod is created for it. After the admin disables maintenance, it is claimed normally.
2. With maintenance on, a run already `running` finishes and opens its PR. A run at `awaiting_approval` is approved by its owner and continues to completion. The readout moves from `active 1, held 1` to `active 0, held 0, custody 0: clear`.
3. With maintenance on, an external product creates a `/api/v1` job. The response is the usual success with `status: queued` plus `queue_reason: "maintenance"`; the job's wall budget is not consumed while it waits (it starts at claim, `jobs.sql:305-329`).

## Out of scope

- **Rejecting new runs.** Considered and rejected (Decision Log D1).
- **Freezing the controller.** Controller rolls, DinD pressure maintenance, repair and reaping keep running (D5). An infrastructure freeze is a separate requirement.
- **An atomic enable boundary.** A claim already in flight when maintenance is enabled may still commit and then shows up as `active` (D3).
- **CLI writes.** `api/cmd/uzi/admin.go` is read-only by construction; the CLI gets status only (D6).
- **Scheduled maintenance windows, auto-disable timers, per-repo or per-user maintenance.**
- **User-initiated persistent hosted worker creation** (`api/internal/handler/hosted_workers.go:150`) still creates pods during maintenance. Such a pod holds no work (it cannot claim a new root) and is eviction-safe; the operator docs mention it.
- **Rolling-update ordering.** During an api rolling update, old replicas claim without the conjunct; the operator docs say to enable maintenance only after the rollout completes (D10).
- **`.github/workflows/**`.** Neither implementation nor validation may touch it.

## Modules and seams

### Intake-pause state (store)

Identifiers use **intake pause** in SQL and Go to avoid colliding with worker `maintenance_phase` / `maintenance_fenced`, DinD maintenance and recovery maintenance; the UI and docs say "maintenance mode".


- New singleton table `instance_intake_pause` (one row, `id = true`): `active boolean NOT NULL DEFAULT false`, `since timestamptz`, `by_user_id uuid` (FK `ON DELETE SET NULL`), `by_label text`, `reason text`, with a coherence check (`active` iff `since IS NOT NULL`). New append-only `instance_intake_pause_events` (who, when, on/off, reason), written in the same transaction as each transition, mirroring `worker_cordon_events`.
- New `STABLE` SQL function `fn_instance_intake_paused()` reading that row (no row lock). The migration seeds the row (`active = false`). A **missing row means paused** (`COALESCE(..., true)`): new roots wait, admitted continuations remain eligible. A **database read error fails the claim or provision attempt closed** and it retries through existing polling; already-running work continues. No stale or default-off fallback, and no exception-swallowing SQL. Migration draft number `00358` (renumbered at merge, see root `CLAUDE.md`). The Down refuses when any event history exists, like `00355`.
- Go: `store.InstanceIntakePause(ctx, q)` reads the row **uncached** inside the caller's transaction, for routes, readout and notices only; Go never gates a claim and never derives admission itself. It never goes through the settings TTL cache (`api/internal/settings/settings.go:110`, 5 s default).
- **Reason is admin input rendered to every user.** Bound it to 280 characters and sanitize it exactly as the manual cordon reason is (strip and cap through `termsafe.CellText(ansi.Strip(...))`, `api/internal/store/manual_cordon.go:93-96`); it is never rejected for content. Web renders it as text; the CLI renders it through the same untrusted-field path as other server strings.

### Admission rule (claims)

- **Admitted** means `runs.claim_generation > 0` or `kind = 'cross_check'`. A checker can only be claimed while its lead (locked via `runs.target_run_id`) is claimed or running at the matching generation, or, for a code check, is an interactive task lead at `awaiting_followup` (`runtime.sql:1047-1049`), so it never runs under an unadmitted lead. Everything else is a new root. Precedent for "claimed at least once" as the continuation test: ADR-1751's continuation exemption (`recovery.sql:680-708`).
- One SQL function, `fn_run_admitted_during_intake_pause(r runs)`, and **no Go mirror**. The claim guard is `NOT fn_instance_intake_paused() OR fn_run_admitted_during_intake_pause(r)`, added in exactly two places, which are the only statements that claim (`claim_generation + 1` and `status = 'claimed'`):
  - `ClaimRun`'s `target` CTE (`api/internal/store/queries/runtime.sql:1012-1020`), which also covers the leased ephemeral arm (`:1342`) and every lane (judge, job, checker: `claim_assembly.go:223`, `:233`; `service.go:3251`);
  - `ClaimChatRun`'s target subselect (`api/internal/store/queries/chat.sql:79-100`).
  - No Go-side claim gate (`service.go:3417` and `ephemeral_lease.go:168-198` are parameter assembly).
- **It is not folded into `fn_worker_accepts_new_intake`.** That predicate also feeds capacity, capability, health and controller projections (`runtime.sql:9992-11795`, `hosted_workers.sql:53`, `roll_pacing.go:1270`); a global false there would report every queued run as a capability gap, re-trigger ephemeral demand and change roll pacing (D2).
- Per-worker cordon, drain, maintenance fences and capability rules are unchanged and still apply to admitted work.
- **Test seam**: a live-DB table test that enumerates every run kind × admitted/not-admitted × maintenance on/off through `ClaimRun` and the chat claim, plus a guard test that fails if a new `UPDATE runs ... claim` query is added without the conjunct (regex over the generated query constants in `api/internal/store/*.sql.go`: `claim_generation\s*=\s*(runs\.)?claim_generation\s*\+\s*1` and the SET-list form `(?m)^\s*status\s*=\s*'claimed'\s*,` (not WHERE guards, which also mention `'claimed'`); assert the matched set is non-empty, exactly `{ClaimRun, ClaimChatRun}`, and each carries `fn_run_admitted_during_intake_pause`). Admission table rows include task review and then-fix runs.

### Ephemeral provisioning

- `EphemeralProvisioner` (`api/internal/hostedsvc/ephemeral.go:152`) re-reads the pause **in SQL inside the per-candidate transaction** (`:353`), before eviction, token generation or create, and skips a candidate whose run is not admitted.
- The same admission condition is applied **inside all three demand queries** (`runtime.sql:10420`, `:10624`, `:10899`; selected at `ephemeral.go:190`, `:207`, `:232`) **before** their batch limits, so blocked roots cannot fill the bounded batch and starve admitted replacements. No pass-level early exit on maintenance alone. Regression: more blocked roots than `ephemeralProvisionBatch` plus one admitted replacement still provisions the replacement.
- Admitted runs that need a replacement ephemeral worker are still provisioned.
- Reaping (`:337`) is unchanged. An ephemeral worker bound before enable to a run that is not admitted is **retained**: the reaper does not remove a registered worker whose bound run is still queued (`hosted_workers.sql:281`, `:297-298`). It cannot claim during maintenance, holds no work and is eviction-safe; it is reported informationally and claims its run after lift.

### Admin route and readout

- `GET /api/admin/maintenance` (admin session or admin read token): state plus the readout. `PUT /api/admin/maintenance` `{active, reason}`: **admin, cookie session + CSRF only**, matching settings PUT (`routes_admin.go:149-160`). Idempotent; an `active=true` PUT while active updates the reason only and keeps `since`.
- Readout, from one new aggregate query (none of the existing lists is complete: `runtime.sql:876` caps at 500 and skips chat/judge/checkers; `:9727` skips chat), each category deduplicated by run id:
  - `active`: status `claimed` or `running` (these imply `claim_generation > 0`).
  - `held`: status `awaiting_approval`, `awaiting_input`, `awaiting_followup`, `paused` (incl. wall park, `hold_reason = budget_exhausted`, `runtime.sql:3927`), `limit_wait`, `recovery_wait`, `pool_wait`, or `queued` and admitted. Tagged **`needs_owner`**: `awaiting_approval` without auto-approve, `awaiting_input`, `paused`, and `recovery_wait` with `recovery_wait_cause IN ('worker_requeue_exhausted', 'worker_memory_pressure')` (`recovery.sql:274-275`). Every other held state (`limit_wait`, `pool_wait`, `awaiting_followup` (idle-bounded), admitted `queued`, other `recovery_wait`, and `awaiting_approval` with auto-approve) is **`progressing`**.
  - `custody`: open recovery custody holds (`recovery.sql:286`, `:955`; `hosted_workers.sql:95`), including holds on terminal runs. **Every open hold blocks**: an available capture does not prove latest-work coverage (ADR-2445; `recovery.sql:266-271` keeps `inventory_guarded` holds and owner decisions open beside an older capture). Tagged `needs_owner` from `recovery_custody_hold_facts.decision_needed` / attention.
  - `queued`: status `queued` and not admitted, through `fn_run_admitted_during_intake_pause` (informational).
  - `idle_bound_workers`: ephemeral workers bound to a not-admitted run (informational).
  - Every non-terminal run falls in exactly one of active, held, queued; terminal runs count only through custody.
  - `clear = active == 0 && held == 0 && custody == 0` in one snapshot, returned with `as_of`; documented as "no blockers visible", not a race-free eviction authorization (D3). Each blocker shows how long it has been open.
  - Each category lists up to 50 runs (id, kind, owner label, repo, status); counts are exact.
- Web: a "Maintenance" card on `web/src/pages/AdminSettings.tsx` with the toggle, reason, readout and an explicit "clear: no running or held work (as of <time>)" / "not yet clear" line, with a note to refresh once more before evicting.

### Health detector (M1)

- `ListActiveRunsForHealth` (`runtime.sql:9727`) projects `intake_pause_held = fn_instance_intake_paused() AND status = 'queued' AND NOT fn_run_admitted_during_intake_pause(r)`, as `codex_account_gated` already rides that read (`health.go:302`). `queuedReason` (`api/internal/workersvc/health.go:590`) resolves a fixed `reasonMaintenance` **first** when that bool is true; otherwise every held root past `HealthQueuedSeconds` becomes `waiting_worker` ("waiting for a worker to pick up this run") and fires a per-run Slack nudge on the ok-to-flagged transition (`health.go:458-481`). The nudge for `reasonMaintenance` is suppressed exactly like `reasonHandoffSetup` and the custody-limit reason. On leaving `reasonMaintenance`, reset `health_since` (as the `leftPin` handling at `health.go:456` does), so "stuck for Xm" does not include the pause. Test: a not-admitted queued run past the threshold while paused gets `reasonMaintenance` and no nudge; after lift it reverts with a fresh `health_since`.

### Visibility

- `GET /api/maintenance` (any authenticated user): `{active, since, reason}` only. Web polls it with the existing app-shell cadence and shows an info banner in `AppShell.tsx:1394` for every user. `HealthDangerBanner` is admin-only and danger-only and is not reused.
- Queue reason is **computed in SQL at read time** from the same function, not through the threshold-based health pipeline (`runtime.sql:9898`): run DTOs gain an additive `queue_reason: "maintenance"` when the pause is active and the run is queued and not admitted. Rendered in `web/src/lib/runBadge.ts:488`, `RunProgressCell.tsx:64`, `RunView.tsx:358` and `api/cmd/uzi/run_render.go:159`.
- `/api/v1` jobs: additive optional `queue_reason` on the job DTO (`api/internal/apitypes/v1_jobs.go:43`) and in `api/openapi/v1.yaml`; the status enum (`:761`) is unchanged. `check:api-v1-compat` must pass. Fix the stale `V1JobDTO.WallSeconds` comment (creation freezes the default; it is not null).
- Run-create responses (web, CLI, chat) carry an additive `notice` when maintenance is active; the CLI prints it to stderr as a warning. Slack-created runs and chats (`api/internal/slacksvc`) get the same notice as a thread reply; a second Slack DM refused because a queued chat is waiting (`GetLiveChatForUser`, `chat.sql:62`) says maintenance is why.
- `uzi admin maintenance status [--json]`: read-only, renders the readout.
- Health: new `maintenance.active` check modelled on `checkSchedulesPaused` (`api/internal/healthsvc/checks.go:1172`): warning while active with since/reason/readout counts; unknown on read error. Liveness and readiness probes stay healthy.

## Testing decisions

- Live-DB tests (`.claude/rules/go.md` live-DB pattern) for the admission table, the provisioner's transactional skip, the readout categories and dedup, and transition audit atomicity.
- Mutation discipline: drop the conjunct from `ClaimRun` and watch the admission test fail; drop the provisioner recheck and watch its test fail.
- Handler tests for PUT authz (non-admin 403, admin read token refused, missing CSRF refused) and reason validation; follow `manual_cordon` handler tests.
- Web: Vitest for the banner, the admin card, and queue-reason rendering; mock mode gains a `maintenance` scenario.
- Agent: no change expected; the worker sees no claim, as on a cordoned worker.
- E2E: `git grep -F` claim/skip literals across `e2e/` per root `CLAUDE.md`; add no new phase unless one is cheap.

## Milestones

### M1: Maintenance holds back new work and tells the admin when it is clear

Blocked by: none.

- Migration, state, audit, admission conjunct in both claim statements, provisioner gate, health detector reason and nudge suppression, admin GET/PUT, readout query, admin web card, tests above.
- Acceptance: examples 1 (claim and provisioning half) and 2; the admission table test covers every run kind; the claim guard test matches exactly `{ClaimRun, ClaimChatRun}`; no health nudge for paused roots; PUT authz tests pass; `task gate:api` and `task gate:web` green.

### M2: Every user and caller can see maintenance

Blocked by: M1.

- `/api/maintenance`, all-user banner, derived `queue_reason` in run and v1 job DTOs + OpenAPI, create notices (web, CLI, chat, Slack), CLI warning and `uzi admin maintenance status`, health check, docs: a new operator page in `docs/` (when and how to use it, enable only after an api rollout completes, what "clear" means and that it can need owner action, the enable race) and a short user note; `task docs:sync`; `docs/cli.md`.
- Acceptance: example 1 (visibility half) and 3; `check:api-v1-compat` green; `check-docs:web` green.

The linked acceptance issue #3465 (owner: maintainer; blocks neither merge nor close) covers the live check: enable on the hosted k8s instance, confirm no pod is provisioned for a new run, drain a worker node once the readout says clear (re-checked), disable, confirm the queue starts.

## Decision Log

- **D1 Queue new work, do not reject it.** Gating at claim leaves runs queued with no change at the ~10 creation sites; rejection would add a refusal path, copy and tests at each, lose label-triggered intent, and make `/api/v1` callers retry. Schedules do not stack: `HasActiveRunForIssue` (`autopilot.sql:90-103`) and `HasActiveRunForSchedule` (`schedules.sql:370-378`) count queued runs, so a repeat fire skips `already_running`. Lift is bounded by worker slots. Rejected: 409/503 rejection.
- **D2 A separate conjunct at claim mutations, not inside `fn_worker_accepts_new_intake`.** Keeps capacity, capability, health and controller projections truthful during maintenance. Cost: a handful of claim sites, covered by an enumeration test and a guard test. Rejected: folding into the predicate (would mark every queued run a capability gap and trigger ephemeral demand).
- **D3 Snapshot visibility, no atomic boundary.** Claims read the flag per statement; a claim that started before enable may commit after. It appears as `active` once committed, but a readout taken in between can show zero: "clear" means no blockers visible in that snapshot, and the docs tell the admin to re-check before evicting. The same race applies to provisioning. Rejected: shared/exclusive fencing across intake and provisioning (new lock order for little gain; the user chose the simple form).
- **D4 Admission = claimed at least once (`claim_generation > 0`), plus cross-check children of an admitted lead.** "Finish all runs" means any run that started, including after a requeue to a different worker. Chat Continue, judge, ci_fix, task review and then-fix phases, recovery successors are new runs and wait (a `--review --then-fix` handoff stops between phases until lift). Cross-check children are admitted unconditionally because ClaimRun already binds them to a claimed lead. Rejected: the cordon's same-worker continuation rule, which would strand admitted runs requeued off a lost worker.
- **D5 Controller unchanged.** Workers stay effective-uncordoned, so roll pacing and DinD maintenance proceed; idle workers rolling is harmless during a drain. Rejected: a controller freeze (separate requirement).
- **D6 Web is the only writer; CLI status only.** Settings PUT is cookie+CSRF only and the admin CLI is read-only by construction; a CLI write needs its own auth design. Rejected: a CLI `on/off`.
- **D7 Dedicated singleton table, not `app_settings`.** `app_settings` values can be env-sourced (invisible to SQL), are strings, and are read through a TTL cache; the claim SQL needs an authoritative row and the transition needs an audit row. Rejected: an `app_settings` key.
- **D8 "Clear" requires zero active, zero held and zero open custody.** User decision (zero held): ephemeral workers lose their PVC on teardown, so held work on them is not eviction-safe; open custody keeps the existing protection. Long-lived holds (#3031, #2506, #2494, #2969, #2544) show as `needs_owner` with owner and age, so the admin knows whom to contact. Rejected: "0 active" alone; treating custody with an available capture as non-blocking (a capture does not prove latest-work coverage, ADR-2445); custody never blocking.
- **D9 Queue reason computed in SQL at read time; health detector knows the pause.** The health pipeline is threshold-based and would lag, so the DTO reason is derived; but the detector must still resolve `reasonMaintenance` first, or it mislabels and nudges every held root. One SQL function serves claims, demand, readout and queue reason, so there is no Go mirror to drift. Rejected: persisting a health reason; a Go mirror.
- **D10 Rolling update ordering is documented, not engineered.** Old replicas lack the conjunct during a rollout; the window is bounded by rollout duration and the docs say to enable after it completes. The reverse order (migration applied, pause off) is inert. Rejected: a version handshake.
- **D11 One M1, not split.** A guard-only milestone has no way to enable the pause and is not verifiable end to end. Rejected: splitting M1 into guards and routes.
- **D12 No post-lift nudge burst.** Leaving maintenance re-stamps `health_since` but does not nudge on that tick; a run still stuck after the lift is not nudged until it clears to ok and re-flags. Accepted to avoid a DM burst for the whole backlog at lift. Rejected: nudging every held root on the first post-lift tick.
- **D13 Delivery notes (#3454 run).** Migration 00359 adds a concurrent partial index `idx_runs_nonterminal_since` so the readout and health check stay cheap at large `runs` volumes. A test helper truncating `users` must re-seed the pause row (its `by_user_id` FK cascades on TRUNCATE; a missing row reads as paused). The readout sanitizes owner labels and repo names at render. Every `fn_instance_intake_paused()` call is wrapped as `(SELECT fn_instance_intake_paused())` for one evaluation per statement.
