-- +goose Up

-- Extend run_usage_totals with a per-run folded cost_status (PRD #1332 M5A / C1, D2/D5).
-- CREATE OR REPLACE VIEW keeps the existing five output columns FIRST, in the same NAME and
-- TYPE and order (input_tokens/cache_read_tokens/cache_creation_tokens/output_tokens as
-- ::bigint, cost_usd as ::numeric — CREATE OR REPLACE forbids reorder/retype/removal) so
-- every dependent read query (GetRunUsageTotal, ListRunsForUser's ru.* join,
-- GetJudgeRunUsageForTarget) still compiles unchanged, and APPENDS cost_status at the end.
--
-- cost_usd stays SUM(cost_usd): non-metered rows carry cost_usd=0 (00224's
-- run_usage_nonmetered_zero_check), so the sum already equals the metered-only dollar total.
--
-- The conservative fold (D2): unreported dominates; a run mixing metered and subscription
-- rows is unreported (a partial dollar sum cannot represent completeness); all-subscription
-- is subscription; all-metered is metered. The SAME CASE is applied at BOTH grouping levels.
-- The inner per-(run_id, model, lineage_epoch) level already MAXes the cumulative
-- token/cost snapshots (00177's reasoning), and folding cost_status there first then
-- re-folding at the outer per-run level is value-identical to folding across ALL the run's
-- rows at once: the fold's result depends only on which of {metered, subscription,
-- unreported} are present, and one level of folding preserves that three-way presence
-- (a metered+subscription group collapses to 'unreported', which re-triggers the outer
-- unreported; a pure group keeps its own status). run_usage rows for one run share the same
-- harness but MAY carry different cost_status per model (C4b), so the fold spans models.
CREATE OR REPLACE VIEW run_usage_totals AS
SELECT run_id,
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
    SELECT run_id, model, lineage_epoch,
           MAX(input_tokens)          AS input_tokens,
           MAX(cache_read_tokens)      AS cache_read_tokens,
           MAX(cache_creation_tokens)  AS cache_creation_tokens,
           MAX(output_tokens)          AS output_tokens,
           MAX(cost_usd)               AS cost_usd,
           CASE
               WHEN bool_or(cost_status = 'unreported') THEN 'unreported'
               WHEN bool_or(cost_status = 'metered') AND bool_or(cost_status = 'subscription') THEN 'unreported'
               WHEN bool_or(cost_status = 'subscription') THEN 'subscription'
               ELSE 'metered'
           END AS cost_status
    FROM run_usage
    GROUP BY run_id, model, lineage_epoch
) per_model_epoch
GROUP BY run_id;

-- +goose Down

-- CREATE OR REPLACE cannot REMOVE a column, so restore the pre-#1332 view (00177's Up
-- verbatim, five columns) via DROP + CREATE. A true inverse: the rollup that ignores
-- cost_status.
DROP VIEW run_usage_totals;
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
