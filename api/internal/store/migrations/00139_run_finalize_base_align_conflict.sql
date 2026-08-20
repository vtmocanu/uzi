-- +goose Up

-- PRD #456 M2: widen the runs.fail_origin domain with an eleventh value,
-- `finalize_base_align_conflict`. When a GitHub run is behind on `.github/workflows/**`
-- relative to the current default branch, the worker aligns the branch (merge-first,
-- rebase-fallback) before the finalize push. If that align CANNOT auto-resolve (the
-- merge AND the rebase both conflict), the worker aborts, ends the run `failed` with
-- this typed origin, and preserves the pre-align diff in runs.preserved_patch (the
-- column PRD #377 added in 00137) so a human can recover the work — the exact manual
-- recovery #422 got, now automatic and typed instead of a raw `remote rejected`.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126,
-- Postgres auto-named it <table>_<column>_check; widened once already by 00137 for
-- workflow_scope_missing). Drop and re-add it with the widened set — the ten values
-- carried verbatim from 00137's Up (00126's nine plus workflow_scope_missing) plus
-- finalize_base_align_conflict. TestFailOriginVocabularyMatchesCheck parses THIS
-- migration's CHECK and asserts it equals AllFailOrigins(), so adding a member on one
-- side without the other reddens at `go test` rather than raising 23514 on a user's
-- failed run.
--
-- preserved_patch ALREADY EXISTS (added by 00137) and is reused unchanged — this
-- migration does NOT touch that column, in either direction (00137 owns it).
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00139 against a live head of
-- 00138_worker_draining.sql; renumber above the live head on the landing rebase if
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
        'finalize_base_align_conflict'
    ));

-- +goose Down

-- Narrow back to the ten-value set (00137's Up). Any `finalize_base_align_conflict`
-- rows written while this migration was applied would violate the narrower CHECK, so
-- drop them first — a down-migration undoing this feature cannot keep values the
-- restored domain forbids. Do NOT drop preserved_patch here: 00137 owns that column
-- and it stays in use for workflow_scope_missing.
DELETE FROM runs WHERE fail_origin = 'finalize_base_align_conflict';
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
        'workflow_scope_missing'
    ));
