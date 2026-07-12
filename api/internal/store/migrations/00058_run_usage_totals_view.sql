-- +goose Up

-- Per-run usage totals under M1's cumulative-across-resume verdict (PRD #40
-- Decision 3b), centralized in ONE place so every read (run list/detail, self,
-- admin) applies the identical rollup and it can never drift to a plain SUM over
-- run_usage rows. The rule: a run's total FOR A MODEL is the MAX (greatest-wins)
-- cumulative snapshot across that model's session rows — because the result frame
-- reports cumulative-to-date, so session rows are snapshots, not deltas — and the
-- run total is the SUM of those per-model maxima. A plain SUM over run_usage would
-- multiply the snapshots whenever session_id evolves across a run's turns.
--
-- A run appears here only if it has at least one run_usage row: readers LEFT JOIN
-- (or :one + no-rows) so a pre-feature run surfaces as absent usage, never a fake 0.
-- Chat runs never have rows (the fold skips kind='chat'), so they never appear.
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

-- +goose Down
DROP VIEW run_usage_totals;
