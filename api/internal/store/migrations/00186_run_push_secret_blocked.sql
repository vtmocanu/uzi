-- +goose Up

-- worker pre-push secret scan + typed GH013 push-protection origin (issue #974):
-- widen the runs.fail_origin domain with a twelfth value, `push_secret_blocked`.
-- A GitHub run whose push carries a secret is rejected by GitHub Push Protection
-- (GH013 `Push cannot contain secrets`). The worker PREDICTS this with a pre-push
-- gitleaks range scan and also PARSES the remote reject, so instead of a raw
-- `remote rejected` the run ends `failed` with this typed origin and the pre-push
-- diff preserved in runs.preserved_patch (the column PRD #377 added in 00137, reused
-- here) so a human can recover the work — mirroring the workflow_scope_missing guard
-- in agent/src/ci-config-guard.ts, now for the secret-in-push class.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126,
-- Postgres auto-named it <table>_<column>_check; widened by 00137 for
-- workflow_scope_missing and by 00139 for finalize_base_align_conflict). Drop and
-- re-add it with the widened set — the eleven values carried verbatim from 00139's Up
-- plus push_secret_blocked. TestFailOriginVocabularyMatchesCheck parses THIS
-- migration's CHECK and asserts it equals AllFailOrigins(), so adding a member on one
-- side without the other reddens at `go test` rather than raising 23514 on a user's
-- failed run.
--
-- preserved_patch ALREADY EXISTS (added by 00137) and is reused unchanged — this
-- migration does NOT touch that column, in either direction (00137 owns it).
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00186 against a live head of
-- 00185_run_checkpoint_tip.sql; renumber above the live head on the landing rebase if
-- another migration merged first (strict goose refuses to boot on a version below an
-- already-applied head — store/migrate.go), per the CLAUDE.md goose convention (no
-- allow-missing).
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

-- +goose Down

-- Narrow back to the eleven-value set (00139's Up). Any `push_secret_blocked` rows
-- written while this migration was applied would violate the narrower CHECK, so clear
-- the now-forbidden value first. NULL it (fail_origin is nullable) rather than DELETE the
-- rows — a down-migration undoing this FEATURE must not destroy whole run records; the
-- human-readable failure_reason on those runs is untouched. Do NOT drop preserved_patch
-- here: 00137 owns that column and it stays in use for workflow_scope_missing.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'push_secret_blocked';
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
        'finalize_base_align_conflict'
    ));
