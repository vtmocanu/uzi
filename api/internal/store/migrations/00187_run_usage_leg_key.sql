-- +goose Up

-- PRD #1079: key run_usage per SDK query() LEG, not per (run, session, model).
--
-- Each delivered result frame's modelUsage is the cost of ONE query() call, not a
-- cumulative session total: the Agent SDK cost-tracking docs state "When using
-- sessions, the cost reported is limited to the individual query call rather than
-- the entire session" and "Because the SDK does not maintain a session-level total,
-- developers are responsible for manually accumulating these values." The worker runs
-- one query() per turn (planning, then each implement iteration), each resumed on the
-- prior session id, so every result frame is one leg reporting only that leg. Because
-- all legs share runs.session_id, they collapsed into ONE (run_id, session_id, model)
-- row and GREATEST kept only the largest leg — under-counting a run's cost 2x-3.8x.
--
-- The fix keys each leg by its lineage_epoch (added in 00176), now derived at fold
-- time as the position-absolute count of persisted `init` frames before the result
-- frame (a pure function of (run_id, seq), so re-delivery recomputes the same value).
-- Adding lineage_epoch to the PRIMARY KEY makes GREATEST de-dup only re-delivery of
-- the SAME leg; the run_usage_totals view (00177) already MAXes within
-- (run_id, model, lineage_epoch) then SUMs across, which is exactly the per-leg rule
-- once every leg carries its own epoch. The view is NOT touched here (PRD D3).
--
-- run_id stays leading in the PK so the per-run rollup scan keeps its index. Every
-- existing row is unique on the old (run_id, session_id, model) key, so it is
-- trivially unique on the new four-column key: no data rewrite.
ALTER TABLE run_usage DROP CONSTRAINT run_usage_pkey,
    ADD PRIMARY KEY (run_id, session_id, model, lineage_epoch);

-- Retire the mutable per-run counter (PRD D4). With the position-absolute epoch there
-- is nothing to bump: runs.lineage_epoch, BumpRunLineageEpoch and the
-- resume_lineage_break bump loop all go away. This DROP regenerates every
-- `SELECT *` / `RETURNING *` on runs (a broad but mechanical sqlc diff removing
-- LineageEpoch from the Run struct and its scans).
ALTER TABLE runs DROP COLUMN lineage_epoch;

-- M3's one-off history-refold marker (PRD D5). A row is "refolded" once its run_usage
-- rows are keyed per leg. New rows are born refolded (DEFAULT true), so the incremental
-- fold stays the only writer for live runs; every non-chat row that exists at migration
-- time is history to refold and is set false. Chat runs never fold usage (the fold skips
-- kind='chat'), so they stay true and are never selected by the refold.
ALTER TABLE runs ADD COLUMN usage_refolded boolean NOT NULL DEFAULT true;
UPDATE runs SET usage_refolded = false WHERE kind <> 'chat';

-- Make the per-frame epoch count O(legs), not O(messages): a partial index over just
-- the `init` status frames. payload is jsonb, so the ->>'event' predicate is indexable.
-- The name and predicate do not collide with #1078's idx_run_messages_tool_use_seq.
CREATE INDEX idx_run_messages_init ON run_messages (run_id, seq)
    WHERE kind = 'status' AND payload->>'event' = 'init';

-- +goose Down

-- A downgrade KNOWINGLY restores the per-iteration under-count: it collapses the
-- per-leg rows back to one MAX row per (run_id, session_id, model), which is the very
-- masking PRD #1079 fixes. It is a true inverse of the schema change, not of the data
-- loss — the summed-across-legs totals cannot be recovered once collapsed.
DROP INDEX idx_run_messages_init;

ALTER TABLE runs DROP COLUMN usage_refolded;

-- Restore the mutable counter default 0 (every row single-lineage, as before 00176 use).
ALTER TABLE runs ADD COLUMN lineage_epoch integer NOT NULL DEFAULT 0;

-- Collapse run_usage to the old key BEFORE restoring the old PK: the four-column key
-- can hold several epochs per (run_id, session_id, model), which the three-column PK
-- cannot, so MAX each column into a single epoch-0 row per old key first.
CREATE TEMP TABLE run_usage_collapsed AS
SELECT run_id, session_id, model,
       0 AS lineage_epoch,
       MAX(input_tokens)          AS input_tokens,
       MAX(cache_read_tokens)     AS cache_read_tokens,
       MAX(cache_creation_tokens) AS cache_creation_tokens,
       MAX(output_tokens)         AS output_tokens,
       MAX(cost_usd)              AS cost_usd,
       MAX(updated_at)            AS updated_at
FROM run_usage
GROUP BY run_id, session_id, model;
DELETE FROM run_usage;
INSERT INTO run_usage (
    run_id, session_id, model, lineage_epoch,
    input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at
)
SELECT run_id, session_id, model, lineage_epoch,
       input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at
FROM run_usage_collapsed;
DROP TABLE run_usage_collapsed;
ALTER TABLE run_usage DROP CONSTRAINT run_usage_pkey,
    ADD PRIMARY KEY (run_id, session_id, model);
