-- +goose Up

-- PRD #377 M1: widen the runs.fail_origin domain with a tenth value,
-- `workflow_scope_missing`, and add a nullable column to preserve the agent's diff.
-- When a GitHub run's branch touches `.github/workflows/**`, the bot's `repo`-only PAT
-- cannot push it (a structurally-guaranteed rejection, by design — the CI-integrity
-- boundary in privcheck forbids the `workflow` scope). The worker now detects this at
-- finalize BEFORE the doomed push and reports a typed `failed` outcome carrying this
-- origin plus the preserved patch, instead of face-planting into a raw `remote rejected`
-- error and discarding the agent's work.
--
-- The fail_origin CHECK was created inline-unnamed in 00126, so Postgres auto-named it
-- `runs_fail_origin_check` (single-column CHECK → <table>_<column>_check); it has never
-- been altered since (verified: no prior ALTER touches it). Drop and re-add it with the
-- widened set — the nine existing values carried verbatim from 00126 plus
-- `workflow_scope_missing`. TestFailOriginVocabularyMatchesCheck parses THIS migration's
-- CHECK and asserts it equals AllFailOrigins(), so adding a member on one side without
-- the other reddens at `go test` rather than raising 23514 on a user's failed run.
--
-- preserved_patch is NULLABLE with NO DEFAULT: only a `failed` report on the
-- workflow-scope path sets it, and a dedicated column (rather than reusing report_md)
-- keeps the report-only semantics untouched — see PRD D3. It carries the agent's
-- secret-scrubbed, size-capped branch diff so a human can apply it without re-deriving
-- it from the transcript.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00137 against a live head of 00136_then_fix.sql;
-- renumber above the live head on the landing rebase if another migration merged first
-- (strict goose refuses to boot on a version below an already-applied head —
-- store/migrate.go), per the CLAUDE.md goose convention (no allow-missing).
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
ALTER TABLE runs ADD COLUMN preserved_patch text;

-- +goose Down

-- Narrow back to the original nine-value set (00126). Any `workflow_scope_missing` rows
-- written while this migration was applied would violate the narrower CHECK, so drop them
-- first — a down-migration undoing this feature cannot keep values the restored domain
-- forbids.
DELETE FROM runs WHERE fail_origin = 'workflow_scope_missing';
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
        'auto_stopped'
    ));
ALTER TABLE runs DROP COLUMN preserved_patch;
