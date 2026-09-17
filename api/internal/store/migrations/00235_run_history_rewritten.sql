-- +goose Up

-- PRD #1416 M4: a run on a PUBLISHED branch whose history was rewritten below the published
-- floor P, where the ancestry bridge B (tree == H, both P and H ancestors) could not be built
-- or validated, fails typed 'history_rewritten' with a preserved_patch on both push paths
-- instead of finalize_base_align_conflict or the generic catch (SC3). This migration is the
-- schema half: it widens runs_fail_origin_check with a FOURTEENTH value, 'history_rewritten'.
--
-- Unlike forge_unreachable (00232), history_rewritten IS worker-reportable
-- (workersvc/failorigin.go workerReportableFailOrigins): the worker detects the un-bridgeable
-- divergence at finalize and reports it. It is an AGENT DEFECT, so it stays JUDGE-ELIGIBLE
-- (deliberately absent from both preStartInfraFailOrigins and neverJudgeFailOrigins in
-- workersvc/judge_enqueue.go). TestFailOriginVocabularyMatchesCheck parses THIS CHECK and
-- asserts it equals AllFailOrigins(), so adding a member on one side without the other reddens
-- at `go test` rather than raising 23514 on a user's failed run.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126, Postgres
-- auto-named it <table>_<column>_check; widened by 00137, 00139, 00186 and 00232). Drop and
-- re-add it with the widened set — the thirteen values carried verbatim from 00232's Up plus
-- 'history_rewritten'. Immediate DROP+ADD (no NOT VALID), the 00186 template: the CHECK
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
        'history_rewritten'
    ));

-- +goose Down

-- Narrow runs_fail_origin_check back to the thirteen-value set (00232's Up). Any
-- 'history_rewritten' rows written while this migration was applied would violate the narrower
-- CHECK, so clear the now-forbidden value first. NULL it (fail_origin is nullable) rather than
-- DELETE the rows — a down-migration undoing this FEATURE must not destroy whole run records;
-- the human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'history_rewritten';
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
        'forge_unreachable'
    ));
