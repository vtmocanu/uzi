-- +goose Up

-- Issue #1783: a run-owned execution (a tool shell or a Docker invocation the run started) can
-- outlive the run's park and recreate the run's clone path with foreign ownership, so the next
-- reseed of that path fails with EACCES and surfaced only as the generic 'agent_failure'. The
-- worker now refuses to push, capture or reseed when it cannot prove the run's execution stopped
-- (surviving or unverifiable run-owned processes) or cannot clear or quarantine residue at the
-- run's clone path, and fails the run typed with a NEW nineteenth fail_origin,
-- 'worker_residue_blocked', instead of guessing. This migration is the schema half: it widens
-- runs_fail_origin_check with that nineteenth value, for NINETEEN values in all.
--
-- 'worker_residue_blocked' is WORKER-REPORTABLE: the worker detects the unproven quiescence or
-- the unclearable residue and reports it on its `failed` state (workersvc/failorigin.go's
-- workerReportableFailOrigins includes it, so CoerceFailOrigin passes it through). It is a
-- worker INFRASTRUCTURE condition, not an agent defect, so it is NEVER JUDGED: it is a member
-- of neverJudgeFailOrigins (workersvc/judge_enqueue.go), which skips the judge regardless of
-- iteration_count (it can fire pre-start at reseed or at finalize on a resumed run). It is not
-- human-landable (humanLandableFailOrigins excludes it): the worker refused to push or capture
-- rather than publish from an unproven state. TestFailOriginVocabularyMatchesCheck parses THIS CHECK
-- (the latest migration declaring one) and asserts it equals AllFailOrigins(), so adding a
-- member on one side without the other reddens at `go test` rather than raising 23514 on a
-- user's failed run.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126, Postgres
-- auto-named it <table>_<column>_check; widened by 00137, 00139, 00186, 00232, 00235, 00238, 00250,
-- 00260 and 00261). Drop and re-add it with the widened set: the eighteen values carried verbatim
-- from 00261's Up plus 'worker_residue_blocked'. Immediate DROP+ADD (no NOT VALID), the 00186
-- template: the CHECK validates against the existing rows on ADD, which is cheap for this small
-- domain column.
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
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full',
        'worker_residue_blocked'
    ));

-- +goose Down

-- Narrow runs_fail_origin_check back to the eighteen-value set (00261's Up). Any
-- 'worker_residue_blocked' rows written while this migration was applied would violate the
-- narrower CHECK, so clear the now-forbidden value first. NULL it (fail_origin is nullable)
-- rather than DELETE the rows: a down-migration undoing this FEATURE must not destroy whole run
-- records; the human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'worker_residue_blocked';
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
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full'
    ));
