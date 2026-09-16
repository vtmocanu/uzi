-- +goose Up

-- PRD #1392 M1: a transient forge failure at CLONE parks the run on 'recovery_wait'
-- with a typed cause instead of failing it, under a forge-only lifetime cap, with
-- custody settled atomically in the park transaction and a fifth release-evidence class.
-- This migration is the schema half:
--
--   1. runs.recovery_wait_cause — the TYPED cause of a 'recovery_wait' park. NULL is the
--      LEGACY/untyped park (the empty-turn park writes NULL, D9), so the column is
--      nullable with a CHECK admitting only the three typed causes. Only 'forge_unreachable'
--      is written today (ParkRunForgeUnreachable); 'empty_turn'/'provider_outage' are
--      reserved (D9 — #1088 adopts 'provider_outage' when it lands).
--   2. runs.forge_park_count — the FORGE-ONLY lifetime park counter the cap decides on
--      (RUN_FORGE_UNREACHABLE_MAX_PARKS). Distinct from recovery_wait_count, which shapes
--      the backoff for EVERY cause and has no lifetime cap (fact 7 / D2): the empty-turn
--      park must keep its "no lifetime cap" contract, so it never touches this counter.
--      NOT NULL DEFAULT 0 so every existing row is 0 (never forge-parked).
--   3. recovery_custody_holds.release_evidence — the DISPOSITION that WARRANTED a hold's
--      release (fact 9 / D3). The holds table carried state/provenance/timestamps only, no
--      evidence column; this records WHY each release was allowed, under a CHECK of the five
--      classes: 'publication' (a completed run published its head), 'archive' (a ready
--      capture covers the source), 'forge_no_output' (a fresh-forge no-output proof),
--      'owner_discard' (an explicit owner discard), and the NEW 'no_adopted_source' (a
--      generation that never adopted a source — the pre-clone forge park, D3). Nullable:
--      every legacy released/open row predates the column and stays NULL.
--   4. runs_fail_origin_check widened with a THIRTEENTH value, 'forge_unreachable' — the
--      SERVER-DERIVED terminal origin the run gets when it exceeds the forge park cap. It is
--      NOT worker-reportable (workersvc/failorigin.go workerReportableFailOrigins): the
--      server stamps it directly. TestFailOriginVocabularyMatchesCheck parses THIS CHECK and
--      asserts it equals AllFailOrigins(), so adding a member on one side without the other
--      reddens at `go test` rather than raising 23514 on a user's failed run.
--
-- The fail_origin CHECK is `runs_fail_origin_check` (created inline-unnamed in 00126,
-- Postgres auto-named it <table>_<column>_check; widened by 00137, 00139 and 00186). Drop
-- and re-add it with the widened set — the twelve values carried verbatim from 00186's Up
-- plus 'forge_unreachable'. Immediate DROP+ADD (no NOT VALID), the 00186 template: the CHECK
-- validates against the existing rows on ADD, which is cheap for this small domain column.
--
-- Drafted as 00233; landed as 00232, the next free number above the live head
-- 00231_validate_run_branch_moved_stop_kind.sql at the landing rebase (#1386 landed
-- 00230/00231 first). Renumbered per the CLAUDE.md goose convention (no allow-missing;
-- strict goose refuses to boot on a version below an already-applied head — store/migrate.go).
ALTER TABLE runs ADD COLUMN recovery_wait_cause text;
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN (
        'forge_unreachable',
        'empty_turn',
        'provider_outage'
    ));

ALTER TABLE runs ADD COLUMN forge_park_count int NOT NULL DEFAULT 0;

ALTER TABLE recovery_custody_holds ADD COLUMN release_evidence text;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check
    CHECK (release_evidence IS NULL OR release_evidence IN (
        'publication',
        'archive',
        'forge_no_output',
        'owner_discard',
        'no_adopted_source'
    ));

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

-- +goose Down

-- Narrow runs_fail_origin_check back to the twelve-value set (00186's Up). Any
-- 'forge_unreachable' rows written while this migration was applied would violate the
-- narrower CHECK, so clear the now-forbidden value first. NULL it (fail_origin is nullable)
-- rather than DELETE the rows — a down-migration undoing this FEATURE must not destroy whole
-- run records; the human-readable failure_reason on those runs is untouched.
UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'forge_unreachable';
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

ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_release_evidence_check;
ALTER TABLE recovery_custody_holds DROP COLUMN release_evidence;

ALTER TABLE runs DROP COLUMN forge_park_count;

ALTER TABLE runs DROP CONSTRAINT runs_recovery_wait_cause_check;
ALTER TABLE runs DROP COLUMN recovery_wait_cause;
