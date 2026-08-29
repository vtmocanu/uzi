-- +goose Up

-- PRD #632: per-leg lineage marker to fix the broken-resume undercount. When a worker
-- restart fails to resume, a fresh SDK session accumulates run_usage from 0 under a new
-- (run_id, session_id, model) row; the run_usage_totals view's MAX-per-(run_id, model)
-- silently masked the smaller leg. lineage_epoch stamps each independent leg so the view
-- can MAX within (run_id, model, lineage_epoch) then SUM across epochs (migration 00176).
--
-- runs.lineage_epoch is the per-run counter, bumped server-side once per ingested
-- resume_lineage_break status event (a column, not a per-fold subquery, to keep the
-- fold hot path cheap). run_usage.lineage_epoch is the stamped snapshot the fold writes.
-- Both default 0, so every existing row and every single-lineage run is byte-identical
-- to today: zero historical restatement.
ALTER TABLE runs      ADD COLUMN lineage_epoch integer NOT NULL DEFAULT 0;
ALTER TABLE run_usage ADD COLUMN lineage_epoch integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE run_usage DROP COLUMN lineage_epoch;
ALTER TABLE runs      DROP COLUMN lineage_epoch;
