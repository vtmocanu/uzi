-- +goose Up

DROP VIEW run_usage_totals;

-- Per-run usage totals, now lineage-epoch aware (PRD #632). A Postgres view cannot be
-- ALTERed to change its column set, so this DROP + CREATE replaces 00063's rollup. The
-- output columns are byte-identical in NAME and TYPE (input_tokens/cache_read_tokens/
-- cache_creation_tokens/output_tokens as ::bigint, cost_usd as ::numeric) so every
-- dependent read query still compiles unchanged.
--
-- The rule is now MAX within (run_id, model, lineage_epoch), then SUM across epochs and
-- models per run_id — a two-level refinement of 00063's MAX-per-(run_id, model)→SUM. WHY:
-- when a worker restart fails to resume, a fresh SDK session accumulates run_usage from 0
-- under a new (run_id, session_id, model) row, and the API bumps runs.lineage_epoch on the
-- resume_lineage_break status event (M2) so that fresh leg's rows are stamped a DISTINCT
-- lineage_epoch (M3). 00063's MAX-across-sessions silently masked the smaller leg; summing
-- across epochs now recovers it. MAX WITHIN one epoch still holds: inside a single lineage
-- the result frame reports cumulative-to-date, so session rows are snapshots of one
-- accumulator (session_id may evolve turn-to-turn within the lineage), and MAX-within-epoch
-- collapses them instead of double-counting.
--
-- A run whose rows are all epoch 0 — every single-lineage run, i.e. the vast majority and
-- every pre-#632 row (lineage_epoch defaults to 0) — has exactly one epoch group per model,
-- so the inner grouping collapses to (run_id, model) and the output is value-identical to
-- 00063: zero historical restatement.
--
-- A run appears here only if it has at least one run_usage row: readers LEFT JOIN (or :one +
-- no-rows) so a pre-feature run surfaces as absent usage, never a fake 0. Chat runs never
-- have rows (the fold skips kind='chat'), so they never appear.
CREATE VIEW run_usage_totals AS
SELECT run_id,
       SUM(input_tokens)::bigint          AS input_tokens,
       SUM(cache_read_tokens)::bigint     AS cache_read_tokens,
       SUM(cache_creation_tokens)::bigint AS cache_creation_tokens,
       SUM(output_tokens)::bigint         AS output_tokens,
       SUM(cost_usd)::numeric             AS cost_usd
FROM (
    SELECT run_id, model, lineage_epoch,
           MAX(input_tokens)          AS input_tokens,
           MAX(cache_read_tokens)      AS cache_read_tokens,
           MAX(cache_creation_tokens)  AS cache_creation_tokens,
           MAX(output_tokens)          AS output_tokens,
           MAX(cost_usd)               AS cost_usd
    FROM run_usage
    GROUP BY run_id, model, lineage_epoch
) per_model_epoch
GROUP BY run_id;

-- +goose Down
DROP VIEW run_usage_totals;

-- Restore the pre-#632 view verbatim from 00063_run_usage_totals_view.sql: MAX per
-- (run_id, model) across session rows, then SUM across models per run_id. This is a true
-- inverse — a downgrade recovers the original rollup that ignores lineage_epoch.
CREATE VIEW run_usage_totals AS
SELECT run_id,
       SUM(input_tokens)::bigint         AS input_tokens,
       SUM(cache_read_tokens)::bigint     AS cache_read_tokens,
       SUM(cache_creation_tokens)::bigint AS cache_creation_tokens,
       SUM(output_tokens)::bigint         AS output_tokens,
       SUM(cost_usd)::numeric             AS cost_usd
FROM (
    SELECT run_id, model,
           MAX(input_tokens)          AS input_tokens,
           MAX(cache_read_tokens)      AS cache_read_tokens,
           MAX(cache_creation_tokens)  AS cache_creation_tokens,
           MAX(output_tokens)          AS output_tokens,
           MAX(cost_usd)               AS cost_usd
    FROM run_usage
    GROUP BY run_id, model
) per_model
GROUP BY run_id;
