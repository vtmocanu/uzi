-- +goose Up

-- PRD #69 M7a Pass A (the SEAM): a TRUSTED, structured failure ORIGIN for each
-- failed run. Until now the only record of WHY a run failed lived in the free-text
-- failure_reason column, which is deliberately never parsed (it carries worker
-- prose, server-composed sentences, and an auto-stop decoration that MUST NOT be
-- read — see FailRunAutoStop). The judge needs a machine-checkable class instead,
-- so this adds a closed-enum column stamped at every terminal-failure write site,
-- modelled EXACTLY on the rate_limit_type closed-enum precedent (00091 / the
-- CoerceRateLimitType allowlist in workersvc/limitwait.go).
--
-- fail_origin is NULLABLE with NO DEFAULT, and the two facts a NULL encodes are
-- both legitimate:
--   * a run that COMPLETED or is still non-terminal (never failed), and
--   * a pre-feature or otherwise UNCLASSIFIED failed run (a row that failed before
--     this migration, or through a path a later change forgot to stamp).
-- A DEFAULT would fabricate a class for both, so there is none; every CURRENT
-- failed writer is taught to set a non-NULL origin in this same PRD.
--
-- The vocabulary is CLOSED in this same migration (the CHECK below), for the same
-- reason 00091 closes rate_limit_type in the migration that adds it: the
-- authoritative copy of the set lives on this side of the wire, and
-- CoerceFailOrigin maps any worker-reported value onto it before it can reach the
-- DB. TestFailOriginVocabularyMatchesCheck parses this CHECK and asserts it equals
-- AllFailOrigins(), so adding a member on one side without the other reddens at
-- `go test` rather than raising 23514 on a user's failed run.
--
-- plan_rejected and auto_stopped OVERLAP stop_kind DELIBERATELY: those two failed
-- transitions already stamp a stop_kind, and this column re-states the same fact in
-- the fail_origin vocabulary so that EVERY failed writer sets a non-NULL origin and
-- a consumer never has to join two columns to classify a failure.
--
-- NUMBER ASSIGNED AT LANDING. Drafted as 00126 against a live head of
-- 00125_user_judge_model.sql; renumber above the live head on the landing rebase if
-- another migration merged first (strict goose refuses to boot on a version below an
-- already-applied head — store/migrate.go).
--
-- The CHECK admits NULL implicitly: `NULL IN (...)` evaluates to NULL, and a CHECK
-- passes unless it is FALSE, so no `fail_origin IS NULL OR` disjunct is needed (and
-- one would still be legal). This mirrors 00091's note that a CHECK rejecting NULL
-- would fail against the existing table at migration time.
ALTER TABLE runs ADD COLUMN fail_origin text
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

-- +goose Down
ALTER TABLE runs DROP COLUMN fail_origin;
