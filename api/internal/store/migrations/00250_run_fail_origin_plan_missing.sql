-- +goose Up

-- Issue #1593: a gated plan (or plan-revision) turn can end with prose only, calling neither
-- submit_plan nor ask_user. The worker nudges the model once; if the turn still produces
-- neither on an unattended run (or after owner guidance), the worker fails the run typed with
-- a NEW sixteenth fail_origin, 'plan_missing', and a fixed failure_reason, instead of the
-- generic 'agent_failure'. This migration is the schema half: it widens runs_fail_origin_check
-- with that sixteenth value.
--
-- 'plan_missing' is WORKER-REPORTABLE: the worker detects the prose-only plan turn and reports
-- it on its `failed` state (workersvc/failorigin.go's workerReportableFailOrigins includes it,
-- so CoerceFailOrigin passes it through). It is an agent defect (model noncompliance with the
-- plan contract), so it stays JUDGE-ELIGIBLE: it is deliberately absent from
-- preStartInfraFailOrigins, neverJudgeFailOrigins and envPublishFailOrigins
-- (workersvc/judge_enqueue.go). TestFailOriginVocabularyMatchesCheck parses THIS CHECK (the
-- latest migration declaring one) and asserts it equals AllFailOrigins(), so adding a member on
-- one side without the other reddens at `go test` rather than raising 23514 on a user's failed
-- run.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126, Postgres
-- auto-named it <table>_<column>_check; widened by 00137, 00139, 00186, 00232, 00235 and
-- 00238). Drop and re-add it with the widened set — the fifteen values carried verbatim from
-- 00238's Up plus 'plan_missing'. Immediate DROP+ADD (no NOT VALID), the 00186 template: the
-- CHECK validates against the existing rows on ADD, which is cheap for this small domain column.
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
        'plan_missing'
    ));

-- +goose Down

-- Narrow runs_fail_origin_check back to the fifteen-value set (00238's Up). Any 'plan_missing'
-- rows written while this migration was applied would violate the narrower CHECK, so clear the
-- now-forbidden value first. NULL it (fail_origin is nullable) rather than DELETE the rows — a
-- down-migration undoing this FEATURE must not destroy whole run records; the human-readable
-- failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'plan_missing';
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
