-- +goose Up

-- worker cross-run recovery-clone conflict origin (issue #1308): widen the
-- runs.fail_origin domain with a thirteenth value, `runner_clone_conflict`. When a
-- runner reuses a recovery clone whose journal names a DIFFERENT owner run, the runner
-- self-heals by reclaiming the clone directory; if that owner run is still nonterminal /
-- active / unreachable the conflict survives self-heal at iteration 0, and the runner
-- fails typed with this origin instead of a raw clone error. It is a worker-reportable
-- (CoerceFailOrigin passes it through — see workerReportableFailOrigins) and a
-- pre-start-infra origin (the Judge is skipped for it at iteration 0 — see
-- preStartInfraFailOrigins), so a run that never got to do anything reviewable is not
-- sent to the most expensive per-run call.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126,
-- Postgres auto-named it <table>_<column>_check; widened by 00137 for
-- workflow_scope_missing, by 00139 for finalize_base_align_conflict, and by 00186 for
-- push_secret_blocked). Drop and re-add it with the widened set — the twelve values
-- carried verbatim from 00186's Up plus runner_clone_conflict.
-- TestFailOriginVocabularyMatchesCheck parses THIS migration's CHECK and asserts it
-- equals AllFailOrigins(), so adding a member on one side without the other reddens at
-- `go test` rather than raising 23514 on a user's failed run.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00221 against a live head of
-- 00220_validate_run_budget_extension.sql; renumber above the live head on the landing
-- rebase if another migration merged first (strict goose refuses to boot on a version
-- below an already-applied head — store/migrate.go), per the CLAUDE.md goose convention
-- (no allow-missing).
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
        'runner_clone_conflict'
    ));

-- +goose Down

-- Narrow back to the twelve-value set (00186's Up). Any `runner_clone_conflict` rows
-- written while this migration was applied would violate the narrower CHECK, so clear
-- the now-forbidden value first. NULL it (fail_origin is nullable) rather than DELETE the
-- rows — a down-migration undoing this FEATURE must not destroy whole run records; the
-- human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'runner_clone_conflict';
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
        'push_secret_blocked'
    ));
