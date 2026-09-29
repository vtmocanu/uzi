-- +goose Up

-- Issue #1888: the worker loads a run's selected skills into the Claude SDK session as a local
-- plugin. When the SDK reports plugin load errors at session start, the run would otherwise
-- proceed without the skills its owner selected and surface, if at all, only as a generic
-- 'agent_failure'. The worker now fails a run that HAS selected skills typed with a NEW
-- twentieth fail_origin, 'skills_plugin_load_failed', and a bounded, redacted failure_reason
-- naming the plugin, the error type, a trimmed message and the path. This migration is the
-- schema half: it widens runs_fail_origin_check with that twentieth value, for TWENTY values in
-- all.
--
-- 'skills_plugin_load_failed' is WORKER-REPORTABLE: the worker detects the load errors and
-- reports it on its `failed` state (workersvc/failorigin.go's workerReportableFailOrigins
-- includes it, so CoerceFailOrigin passes it through). It is a worker ENVIRONMENT condition, not
-- an agent defect, so it is NEVER JUDGED: it is a member of neverJudgeFailOrigins
-- (workersvc/judge_enqueue.go), which skips the judge regardless of iteration_count (it fires at
-- session start, which on a resumed run carries iteration_count > 0). It is not human-landable
-- (humanLandableFailOrigins excludes it): the run stops before the agent works, so there is no
-- committed work to land. TestFailOriginVocabularyMatchesCheck parses THIS CHECK (the latest
-- migration declaring one) and asserts it equals AllFailOrigins(), so adding a member on one side
-- without the other reddens at `go test` rather than raising 23514 on a user's failed run.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126, Postgres
-- auto-named it <table>_<column>_check; widened by 00137, 00139, 00186, 00232, 00235, 00238, 00250,
-- 00260, 00261 and 00263). Drop and re-add it with the widened set: the nineteen values carried
-- verbatim from 00263's Up plus 'skills_plugin_load_failed'. Immediate DROP+ADD (no NOT VALID),
-- the 00186 template: the CHECK validates against the existing rows on ADD, which is cheap for
-- this small domain column.
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
        'worker_residue_blocked',
        'skills_plugin_load_failed'
    ));

-- +goose Down

-- Narrow runs_fail_origin_check back to the nineteen-value set (00263's Up). Any
-- 'skills_plugin_load_failed' rows written while this migration was applied would violate the
-- narrower CHECK, so clear the now-forbidden value first. NULL it (fail_origin is nullable)
-- rather than DELETE the rows: a down-migration undoing this FEATURE must not destroy whole run
-- records; the human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'skills_plugin_load_failed';
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
