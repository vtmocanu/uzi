-- +goose Up

-- PRD #1226 M2 (D4/D5): the structural completion interlock's ATTEMPT LOG and PERMIT
-- storage, plus two additive counters on `runs`. Everything here is ADDITIVE — two ADD
-- COLUMN (a constant-default int → PG fast-default, no table rewrite; a nullable jsonb with
-- no default) and two brand-new tables — so an N-1 worker/api reading `runs` during a
-- rolling release is unaffected, there is NO CHECK on a live table, and check-migration-
-- additive does not need the NOT VALID/VALIDATE two-step 00204/00205 used.

-- runs.completion_attempts is the monotone attempt COUNTER (D4). Incremented by
-- RecordCompletionAttempt each time a gated completion is attempted and denied for missing
-- milestones (or a same-lead nudge is recorded). M3 READS it: the SweepRunningTimeout
-- carve-out excludes rows with completion_attempts > 0 (a run that has begun completing must
-- enter the recoverable hold, not be server-timed-out). Constant default 0 → PG stores it as
-- a fast-default and rewrites no existing row.
ALTER TABLE runs ADD COLUMN completion_attempts int NOT NULL DEFAULT 0;

-- runs.latest_completion_attempt is the most-recent attempt SUMMARY (D4), nullable jsonb:
--   {"unmet":[...],"head":<text|null>,"worktree_fingerprint":<text|null>,"at":<timestamptz>}
-- Overwritten on each RecordCompletionAttempt so the run view/DTO can show the current
-- unmet set + head without scanning the bounded attempt log below. NULL until the first
-- attempt.
ALTER TABLE runs ADD COLUMN latest_completion_attempt jsonb;

-- run_completion_attempts is the BOUNDED attempt log (D4): one row per gated completion
-- attempt. RecordCompletionAttempt inserts here, bumps runs.completion_attempts, sets
-- runs.latest_completion_attempt, then PRUNES this table for the run to the most recent N
-- (see RecordCompletionAttempt's prune step) so the log can never grow unboundedly. unmet is
-- the server-recomputed missing-criteria id list (jsonb array, NOT NULL — an empty attempt
-- is `[]`, never NULL). head / worktree_fingerprint are nullable (the permit path records an
-- attempt with a head but no fingerprint; the same-lead nudge records both). contract_revision
-- is the run's revision AT the attempt, nullable for a pre-freeze edge.
CREATE TABLE run_completion_attempts (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id               uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    contract_revision    int,
    unmet                jsonb NOT NULL,
    head                 text,
    worktree_fingerprint text,
    created_at           timestamptz NOT NULL DEFAULT now()
);

-- The prune step and any per-run attempt read scan by (run_id, created_at DESC).
CREATE INDEX idx_run_completion_attempts_run ON run_completion_attempts (run_id, created_at);

-- run_completion_permits is the CLAIM-FENCED, IDEMPOTENT permit storage (D4/D5): the server
-- issues at most ONE live permit per (run_id, contract_revision, head) — that triple is the
-- IDEMPOTENCY KEY (the UNIQUE below), so repeating an unchanged permit request returns the
-- SAME row (UpsertCompletionPermit's ON CONFLICT DO UPDATE, which never resets consumed_at).
-- A permit is bound to the exact final head H and the frozen contract revision; the terminal
-- completion transaction (completeRunWithPermit) fetches the unconsumed permit for the
-- identity FOR UPDATE, stamps consumed_at, and writes `completed` atomically.
--   issued_by_worker_id  the claim fence: the worker that requested it (nullable — reserved
--                        for a future server-issued permit; structural always sets it).
--   audit                NULL for profile=structural (reserved for the semantic audit block,
--                        #1231).
--   finding_ids          empty '{}' for structural (reserved for structured findings, #1231).
CREATE TABLE run_completion_permits (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    contract_revision   int NOT NULL,
    branch              text NOT NULL,
    head                text NOT NULL,
    issued_by_worker_id uuid,
    audit               jsonb,
    finding_ids         text[] NOT NULL DEFAULT '{}',
    issued_at           timestamptz NOT NULL DEFAULT now(),
    consumed_at         timestamptz,
    UNIQUE (run_id, contract_revision, head)
);

-- +goose Down

-- Reverse add order. Down runs only on an explicit `goose down`, never during a forward
-- rolling release (store.Migrate only goes up), so these drops are out of
-- check-migration-additive's Up-only scope.
DROP TABLE run_completion_permits;
DROP TABLE run_completion_attempts;
ALTER TABLE runs DROP COLUMN latest_completion_attempt;
ALTER TABLE runs DROP COLUMN completion_attempts;
