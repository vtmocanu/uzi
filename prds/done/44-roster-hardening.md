# PRD #44: Harden the repo-agent roster report

**GitLab Issue**: [#44](https://github.com/vtmocanu/uzi/-/issues/44)
**Status**: Complete (2026-07-16; MR !59 merged, issue #44 closed)
**Priority**: Low (follow-up; three minor robustness gaps from the MR !37 whole-diff review)
**Depends on**: nothing. PRD #37 (per-run agent selection) is merged.

## Problem

Three non-blocking robustness gaps clustered around the fire-and-forget
repo-agent roster report, found in the MR !37 review (2026-07-11) and
re-verified against current `main` on 2026-07-16 by three independent review
agents — all three CONFIRMED, none fixed since. None affects whether the
correct agents actually run; #2 can kill an approvable run, #1 and #3 lose
attribution/display data silently.

## Findings (verified 2026-07-16, current file:line)

### F1 — Autopilot repo-source attribution ordering dependency

- `agent/src/runner.ts:186` fires the `repo_agents` running report
  fire-and-forget (no retry); the autopilot selection report
  (`agent/src/runner.ts:446`) omits `repo_agents`, so
  `rosterFor` (`api/internal/workersvc/service.go:1104`) falls back to the
  persisted `run.RepoAgents` column.
- If the fire-and-forget report failed (transient error; it is never
  retried), the column is NULL → `validateSelection("repo", [])` errors
  ("this run detected no repo agents", `agent_selection.go:124-126`) → the
  selection persist is logged-and-dropped → `agent_source` stays NULL. The
  run still executes repo agents correctly (executor uses `ctx.repoAgents`);
  only the run view + MR repo-source marker are lost.
- Realistic trigger is a *failed* report, not tight timing (the plan turn
  outlasts a healthy local write).

**Fix**: include `repo_agents: repoAgentSummaries(repoAgents)` in the
autopilot selection report at `runner.ts:446`, **gated on
`repoAgents.length > 0`**. The guard is required: on detection failure
`repoAgents` is `[]` and the selection resolves to `own`; sending
`repo_agents: []` would flip the persisted NULL ("not reported") to `[]`
("detected none") and break that deliberate distinction (`runner.ts:184-185`,
`protocol.ts:623-625`). `rosterFor` already prefers the reported roster
(`service.go:1106-1112`), so the selection report becomes self-contained and
validates + persists atomically. No wire change (`protocol.ts:626/631`
already permits both fields), no size concern (`MaxRepoAgents=16`).

### F2 — Late `running` report regresses `awaiting_approval`

- `SetRunRunning` WHERE (`api/internal/store/queries/runtime.sql:290-291`)
  only excludes terminal statuses. Worker report retries back off up to ~31s
  (`agent/src/client.ts:44`), and two pre-gate fire-and-forget `running`
  reports exist (`runner.ts:186` roster, `runner.ts:214` onSessionId). A
  retry-delayed one landing after the (awaited) `awaiting_approval` report
  (`runner.ts:454`) flips the run back to `running`.
- Damage: `web/src/pages/RunView.tsx:350` renders PlanPanel only on
  `awaiting_approval`, so the plan gate disappears; **no self-heal** (worker
  is alive and heartbeating, sweeper won't act, worker never re-posts
  awaiting_approval) → the run eventually **fails on plan-approval timeout**.
  A human-approvable plan silently dies. Roster data itself is safe
  (COALESCE persists it regardless).
- Constraint: a naive "never running from awaiting_approval" guard breaks
  approval resume — approval does NOT change status
  (`CreateApprovePlanInput`, `runtime.sql:702-712`); the legit
  awaiting_approval→running transition IS a worker running-report sent after
  the worker consumes the input (`ConsumeRunInputs`, soft-consume via
  `consumed_at`).

**Fix** (API-only, no wire change): extend the `SetRunRunning` WHERE:
`AND (status <> 'awaiting_approval' OR EXISTS (SELECT 1 FROM run_user_inputs
WHERE run_id = @id AND kind = 'approve_plan' AND consumed_at IS NOT NULL))`.
A stale pre-gate report (no consumed approve_plan yet) cannot regress the
gate; the post-approval resume report (input consumed before it by
construction) passes. Must not block claimed→running or running→running
heartbeats; autopilot unaffected (never enters awaiting_approval). Known
accepted residual: a multi-round re-gate (Decision 8b) — a consumed round-1
input lets a stale round-2 pre-gate report through; rare, out of scope
(fully-robust ordinal scheme deliberately not taken — document in
specs/ai.md decision). Requires `sqlc generate` after editing the query.

### F3 — Description length cap: UTF-16 units vs bytes

- Worker: `agent/src/repoagents.ts:278` compares `description.length`
  (UTF-16 units) against `REPO_AGENT_MAX_DESCRIPTION_LEN` = 1024 (`:83`);
  over-cap agents are dropped as invalid.
- API: `api/internal/workersvc/agent_selection.go:94` compares Go `len()`
  (bytes) against `MaxAgentDescriptionLen` = 1024 (`:38`).
- UTF-8 bytes ≥ UTF-16 units always, so the divergence is one-directional:
  worker accepts / API 400s. Practical trigger: ~513 Romanian-diacritic
  chars (2-byte UTF-8), not just exotic 1024-char CJK.
- Aggravation: `validateRepoAgents` returns on the FIRST over-byte
  description, so ONE oversized description 400s the whole report → the
  fire-and-forget swallow (`runner.ts:186` catch → warn) drops the ENTIRE
  roster to NULL (repo card inert, run falls back to own templates).
  Silently.

**Fix**: standardize on **bytes** on both sides. Worker:
`Buffer.byteLength(description, "utf8") > REPO_AGENT_MAX_DESCRIPTION_LEN`
at `repoagents.ts:278`. API untouched (already bytes). Rationale: all
neighbouring caps are byte-based (`REPO_AGENT_MAX_BYTES` 64KB,
`MaxIssueDescriptionBytes` 256KB); bytes is the real payload bound. Also fix
the now-false "characters" wording: Go error msg `agent_selection.go:95` +
comments `:29,:36-38`; worker comment `repoagents.ts:82`. DB is jsonb, not
the binding constraint (`00052_run_agent_selection.sql:36`).

## Milestones

- [x] **M1 (F1)**: self-contained autopilot selection report with the
  `length > 0` guard. Tests: assert the repo-source selection report carries
  `repo_agents` and the own-source one does NOT (extend
  `agent/test/runner.test.ts` around :832/:876). — commit `d2aa213`.
- [x] **M2 (F2)**: `SetRunRunning` status guard + `sqlc generate`. Tests
  (Go): stale running report against `awaiting_approval` with no consumed
  approve_plan input → status unchanged; with consumed input → transition
  allowed; claimed→running and running→running unaffected. — commit
  `5e1f610` (new live-DB test `TestSetRunRunningAwaitingApprovalGuardLiveDB`).
- [x] **M3 (F3)**: byte-basis worker cap + wording fixes. Tests: multibyte
  boundary each side (e.g. 1024 3-byte CJK chars: worker now rejects/drops;
  API rejects — `agent/test/repoagents.test.ts`,
  `api/internal/workersvc/agent_selection_test.go:38` gains a multibyte
  case). — commit `e8d637c`.
- [x] **M4**: docs/specs sync (`specs/ai.md` decisions 261/262/263 incl. the
  accepted F2 residual; no CHANGELOG in repo). MR: created by the team lead.

## Success criteria

1. A failed fire-and-forget roster report no longer costs `agent_source`
   attribution on autopilot runs.
2. A retry-delayed pre-gate `running` report cannot hide the plan-approval
   gate; approval resume still works.
3. Worker and API enforce the same description cap basis; one oversized
   multibyte description no longer silently drops the whole roster.
4. All existing gates green: `cd api && go test ./...`,
   `cd agent && npm test && npm run typecheck`,
   `cd web && npm run typecheck` (web untouched unless needed).

## Out of scope

- Report ordinals / statusless roster PATCH (fully-robust F2 alternative).
- The multi-round re-gate residual hole (documented, accepted).
- Reviewer nits explicitly excluded by issue #44 (custom chip badge,
  `general-purpose` name collision).

## Work Log

- 2026-07-16: PRD created from issue #44 + three independent review-agent
  verdicts (all CONFIRMED against `main` @ `ee373af`).
- 2026-07-16: M1–M3 implemented on `feature/prd-44-roster-hardening`
  (based on `main` @ `ee373af`), one commit each:
  - `d2aa213` (F1): `agent/src/runner.ts` autopilot report carries
    `repo_agents` gated on `length > 0`; `agent/test/runner.test.ts`.
  - `5e1f610` (F2): `SetRunRunning` WHERE guard in
    `api/internal/store/queries/runtime.sql` + regenerated `runtime.sql.go`;
    new live-DB test `api/internal/store/run_running_guard_integration_test.go`.
  - `e8d637c` (F3): byte-basis worker cap in `agent/src/repoagents.ts` +
    "characters"→"bytes" wording in `api/internal/workersvc/agent_selection.go`;
    multibyte tests in `agent/test/repoagents.test.ts` +
    `api/internal/workersvc/agent_selection_test.go`.
  - M4 docs: `specs/ai.md` decisions 261 (F1), 262 (F2, incl. the accepted
    multi-round residual), 263 (F3).
  - Gates: `cd agent && npm test` (526 pass / 1 pre-existing skip) +
    `npm run typecheck` clean; `cd api && go test ./...` green (with the
    documented `UZI_SEED_SLACK_*` shell-leak vars unset); store live-DB IT
    (`./e2e/run-store-it.sh`) all pass. web/ untouched.
