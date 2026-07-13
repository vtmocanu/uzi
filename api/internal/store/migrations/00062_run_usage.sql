-- +goose Up

-- Per-run token/cost accounting (PRD #40). One row per (run, SDK session, model),
-- fed from the terminal result frame's `modelUsage` (Decision 1). The API folds
-- every delivered result frame into this table (workersvc.AppendMessages, Decision
-- 2), so run/user/factory totals stay cheap aggregate queries; per-phase and
-- per-agent detail are derived client-side from the message stream, NOT from here
-- (Decisions 5 + 11).
--
-- M1 pinned the SDK result-frame semantics as CUMULATIVE-across-resume (Decision 3
-- verdict b): each phase's frame reports the session's running totals, not a
-- per-invocation delta. Three consequences are baked into this schema + its fold:
--   * the token/cost columns are monotonic non-decreasing per (run_id, session_id,
--     model), so the fold merges with GREATEST (UpsertRunUsage) — a crash-retry
--     that re-delivers an earlier frame can never regress a row below a later one;
--   * rollups take the LATEST/MAX per (run_id, model), NEVER a SUM across the
--     session rows (summing cumulative snapshots would multiply the totals);
--   * if a live run ever firms the verdict as per-invocation deltas instead, the
--     fold flips to plain latest-wins (EXCLUDED) and rollups to SUM — the shape
--     here survives either verdict, only the merge/rollup rule changes.
--
-- session_id is '' when the run had not yet reported one at fold time (the fold
-- sources it from runs.session_id); it is part of the PK, hence NOT NULL. The PK's
-- leading run_id column also serves the per-run rollup, so no extra index.
CREATE TABLE run_usage (
    run_id                uuid          NOT NULL REFERENCES runs ON DELETE CASCADE,
    session_id            text          NOT NULL DEFAULT '',
    model                 text          NOT NULL,
    input_tokens          bigint        NOT NULL DEFAULT 0,
    cache_read_tokens     bigint        NOT NULL DEFAULT 0,
    cache_creation_tokens bigint        NOT NULL DEFAULT 0,
    output_tokens         bigint        NOT NULL DEFAULT 0,
    cost_usd              numeric(12,6) NOT NULL DEFAULT 0,
    updated_at            timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, session_id, model)
);

-- +goose Down
DROP TABLE run_usage;
