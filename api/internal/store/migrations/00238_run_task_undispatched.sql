-- +goose Up

-- PRD #400 Decision 6 / issue #1367: a kind='task' (handoff) run is created status='queued'
-- with dispatched_at NULL and becomes claimable only once the CLI stamps dispatched_at
-- (DispatchTaskRun) after pushing local HEAD to the run's uzi/task/<id> branch. If that push
-- or the dispatch call never lands, the row sits queued+undispatched forever — ClaimRun never
-- offers it and no existing sweep touches it. The server-owned undispatched-handoff reaper
-- (SweepTaskNeverDispatched) terminalizes it past a dispatch grace window and stamps a NEW
-- fifteenth fail_origin, 'task_undispatched', for it. This migration is the schema half: it
-- widens runs_fail_origin_check with that fifteenth value.
--
-- 'task_undispatched' is SERVER-DERIVED, NOT worker-reportable: it is stamped directly inside
-- the reaper's conditional UPDATE, never by a worker report (workersvc/failorigin.go's
-- workerReportableFailOrigins excludes it, CoerceFailOrigin drops a worker forging it), and it
-- skips the judge (neverJudgeFailOrigins, workersvc/judge_enqueue.go) — an undispatched run has
-- no agent attempt to retrospect. TestFailOriginVocabularyMatchesCheck parses THIS CHECK and
-- asserts it equals AllFailOrigins(), so adding a member on one side without the other reddens
-- at `go test` rather than raising 23514 on a user's failed run.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126, Postgres
-- auto-named it <table>_<column>_check; widened by 00137, 00139, 00186, 00232 and 00235). Drop
-- and re-add it with the widened set — the fourteen values carried verbatim from 00235's Up plus
-- 'task_undispatched'. Immediate DROP+ADD (no NOT VALID), the 00186 template: the CHECK
-- validates against the existing rows on ADD, which is cheap for this small domain column.
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched'
    ));

-- +goose Down

-- Narrow runs_fail_origin_check back to the fourteen-value set (00235's Up). Any
-- 'task_undispatched' rows written while this migration was applied would violate the narrower
-- CHECK, so clear the now-forbidden value first. NULL it (fail_origin is nullable) rather than
-- DELETE the rows — a down-migration undoing this FEATURE must not destroy whole run records;
-- the human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'task_undispatched';
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten'
    ));
