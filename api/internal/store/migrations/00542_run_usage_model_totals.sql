-- +goose Up

-- Issue #4684 (scoped usage Spend API): two read-only views the analytics statements
-- fold over. Purely additive: no table or column changes, no existing view touched.
--
-- NOTE (goose numbering): drafted as 00532, immediately after the live head 00531;
-- renumber above the live head at landing via `task migration:renumber` if another
-- migration lands first.
--
-- run_usage_model_totals: run_usage_totals (00244) keeps its per-model split instead of
-- collapsing it. The body is 00244's run_usage_totals verbatim except the outermost
-- level, which groups by (run_id, model) rather than run_id. The inner per_lineage
-- level already groups by (run_id, model, lineage_index), so the outer SUM runs over
-- lineage_index per (run_id, model) and the session-cumulative high-water fold stays
-- exactly 00244's. Summing this view's rows per run therefore reproduces
-- run_usage_totals, and the 00244 cost_status CASE applied over a run's per-model
-- statuses reproduces its cost_status (the CASE is idempotent across levels).
-- Nested derived tables, no WITH: sqlc's analyzer cannot resolve a view whose body
-- starts with a WITH (see the STRUCTURE note in 00244).
CREATE OR REPLACE VIEW run_usage_model_totals AS
SELECT run_id,
       model,
       SUM(input_tokens)::bigint          AS input_tokens,
       SUM(cache_read_tokens)::bigint     AS cache_read_tokens,
       SUM(cache_creation_tokens)::bigint AS cache_creation_tokens,
       SUM(output_tokens)::bigint         AS output_tokens,
       SUM(cost_usd)::numeric             AS cost_usd,
       CASE
           WHEN bool_or(cost_status = 'unreported') THEN 'unreported'
           WHEN bool_or(cost_status = 'metered') AND bool_or(cost_status = 'subscription') THEN 'unreported'
           WHEN bool_or(cost_status = 'subscription') THEN 'subscription'
           ELSE 'metered'
       END AS cost_status
FROM (
    SELECT run_id, model, lineage_index,
           SUM(CASE WHEN is_cumulative THEN 0          ELSE input_tokens END)
               + GREATEST(0,          MAX(CASE WHEN is_cumulative THEN input_tokens - s_input END))                   AS input_tokens,
           SUM(CASE WHEN is_cumulative THEN 0          ELSE cache_read_tokens END)
               + GREATEST(0,          MAX(CASE WHEN is_cumulative THEN cache_read_tokens - s_cache_read END))         AS cache_read_tokens,
           SUM(CASE WHEN is_cumulative THEN 0          ELSE cache_creation_tokens END)
               + GREATEST(0,          MAX(CASE WHEN is_cumulative THEN cache_creation_tokens - s_cache_creation END)) AS cache_creation_tokens,
           SUM(CASE WHEN is_cumulative THEN 0          ELSE output_tokens END)
               + GREATEST(0,          MAX(CASE WHEN is_cumulative THEN output_tokens - s_output END))                 AS output_tokens,
           SUM(CASE WHEN is_cumulative THEN 0::numeric ELSE cost_usd END)
               + GREATEST(0::numeric, MAX(CASE WHEN is_cumulative THEN cost_usd - s_cost END))                        AS cost_usd,
           CASE
               WHEN bool_or(cost_status = 'unreported') THEN 'unreported'
               WHEN bool_or(cost_status = 'metered') AND bool_or(cost_status = 'subscription') THEN 'unreported'
               WHEN bool_or(cost_status = 'subscription') THEN 'subscription'
               ELSE 'metered'
           END AS cost_status
    FROM (
        SELECT run_id, model, lineage_index, lineage_epoch, is_cumulative, cost_status,
               input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd,
               SUM(CASE WHEN is_cumulative THEN 0          ELSE input_tokens END)          OVER (PARTITION BY run_id, model, lineage_index ORDER BY lineage_epoch) AS s_input,
               SUM(CASE WHEN is_cumulative THEN 0          ELSE cache_read_tokens END)     OVER (PARTITION BY run_id, model, lineage_index ORDER BY lineage_epoch) AS s_cache_read,
               SUM(CASE WHEN is_cumulative THEN 0          ELSE cache_creation_tokens END) OVER (PARTITION BY run_id, model, lineage_index ORDER BY lineage_epoch) AS s_cache_creation,
               SUM(CASE WHEN is_cumulative THEN 0          ELSE output_tokens END)         OVER (PARTITION BY run_id, model, lineage_index ORDER BY lineage_epoch) AS s_output,
               SUM(CASE WHEN is_cumulative THEN 0::numeric ELSE cost_usd END)              OVER (PARTITION BY run_id, model, lineage_index ORDER BY lineage_epoch) AS s_cost
        FROM (
            SELECT run_id, model, lineage_epoch,
                   MAX(lineage_index)                          AS lineage_index,
                   bool_or(usage_basis = 'session_cumulative') AS is_cumulative,
                   MAX(input_tokens)          AS input_tokens,
                   MAX(cache_read_tokens)     AS cache_read_tokens,
                   MAX(cache_creation_tokens) AS cache_creation_tokens,
                   MAX(output_tokens)         AS output_tokens,
                   MAX(cost_usd)              AS cost_usd,
                   CASE
                       WHEN bool_or(cost_status = 'unreported') THEN 'unreported'
                       WHEN bool_or(cost_status = 'metered') AND bool_or(cost_status = 'subscription') THEN 'unreported'
                       WHEN bool_or(cost_status = 'subscription') THEN 'subscription'
                       ELSE 'metered'
                   END AS cost_status
            FROM run_usage
            GROUP BY run_id, model, lineage_epoch
        ) per_leg
    ) windowed
    GROUP BY run_id, model, lineage_index
) per_lineage
GROUP BY run_id, model;

-- run_outcome_classes: one row per outcome-counted run (every kind except chat, via
-- fn_outcome_kind from 00500) with its outcome class, so analytics counts finished,
-- completed, failed, cancelled, plan_rejected and base_diverged from one definition.
-- The CASE mirrors SelfRunOutcomes: plan_rejected / base_diverged are split out of
-- failed by fail_origin, any other failure (including NULL origin) is 'failed', and a
-- run still in flight has a NULL outcome (in no finished or completed denominator).
CREATE OR REPLACE VIEW run_outcome_classes AS
SELECT id AS run_id,
       CASE
           WHEN status = 'completed' THEN 'completed'
           WHEN status = 'cancelled' THEN 'cancelled'
           WHEN status = 'failed' AND fail_origin = 'plan_rejected' THEN 'plan_rejected'
           WHEN status = 'failed' AND fail_origin = 'base_diverged' THEN 'base_diverged'
           WHEN status = 'failed' THEN 'failed'
       END::text AS outcome
FROM runs
WHERE fn_outcome_kind(kind);

-- +goose Down

DROP VIEW IF EXISTS run_outcome_classes;
DROP VIEW IF EXISTS run_usage_model_totals;
