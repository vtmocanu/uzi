-- +goose Up

-- Issue #1562 (ADR-1562, amending ADR-1079): a resumed session's result frame now
-- reports the RUNNING SESSION TOTAL, not only its own query() leg. From Claude Agent SDK
-- 0.3.277 a leg that continued a prior session emits its modelUsage as the session's
-- cumulative total; summing those legs (the ADR-1079 rule) over-counts a resumed run
-- several-fold (a 5-leg live run stored 42.725704 USD vs the true 10.4907258). Rather than
-- guess the reading from an SDK version, the worker now STAMPS which reading a frame
-- carries, and this migration teaches run_usage + the rollup view to honour it. Two
-- schema changes ride ONE migration because they are one feature: the per-row basis marker
-- and the lineage (session) index the view partitions on. 00245 validates the two CHECKs
-- added NOT VALID below.
--
-- NOTE (goose numbering): drafted as 00244, immediately after the live head 00243;
-- renumber the PAIR (00244 + 00245) above the live head together at landing via
-- `task migration:renumber` if another migration lands first.

-- (a) run_usage.usage_basis: the per-row reading marker (D). 'per_leg' is the ADR-1079
-- semantics every pre-#1562 row, every Codex frame and the stub executor emit — its value
-- is only this leg. 'session_cumulative' is the SDK-0.3.277 reading — the row's modelUsage
-- is the running total of the session this leg continued. NOT NULL DEFAULT 'per_leg' leaves
-- every existing row semantically unchanged (all historical rows are per_leg, lineage 0), so
-- the rewritten view returns byte-identical totals for them. The CHECK is added SEPARATELY
-- as NOT VALID and validated in 00245 (not inline on the ADD COLUMN): an inline CHECK is
-- created already-valid and, on a large live run_usage table, has PostgreSQL scan every row
-- under the ACCESS EXCLUSIVE lock the ALTER already holds. Mirrors 00226's two-step.
ALTER TABLE run_usage ADD COLUMN usage_basis text NOT NULL DEFAULT 'per_leg';
ALTER TABLE run_usage ADD CONSTRAINT run_usage_usage_basis_check
    CHECK (usage_basis IN ('per_leg', 'session_cumulative')) NOT VALID;

-- (b) run_usage.lineage_index: the SESSION index within the run (D). lineage_epoch already
-- counts the LEG (one query() call, one init frame); lineage_index counts the SESSION a leg
-- belongs to — 0 is the run's initial session, and each fresh session that starts after a
-- prior one opens 1, 2, … The fold derives it as the count of init frames flagged
-- fresh_session:true with a lower seq, EXCLUDING the run's first init (CountRunLineageRestartsBefore).
-- The view partitions the session_cumulative high-water fold by it so a fresh session
-- restarts the running total instead of being collapsed into the previous one. NOT NULL
-- DEFAULT 0 leaves every existing row in lineage 0 (its historical meaning). Its
-- nonnegativity CHECK is added NOT VALID and validated in 00245, same reasoning as (a).
ALTER TABLE run_usage ADD COLUMN lineage_index integer NOT NULL DEFAULT 0;
ALTER TABLE run_usage ADD CONSTRAINT run_usage_lineage_index_check
    CHECK (lineage_index >= 0) NOT VALID;

-- Rewrite run_usage_totals to fold session_cumulative legs by a per-session high-water mark
-- while keeping per_leg legs summed (ADR-1562). CREATE OR REPLACE keeps the SIX output
-- columns FIRST, in the SAME name, type and order as 00228 (input_tokens/cache_read_tokens/
-- cache_creation_tokens/output_tokens ::bigint, cost_usd ::numeric, cost_status) — CREATE OR
-- REPLACE forbids reorder/retype/removal, and every dependent read (GetRunUsageTotal,
-- ListRunsForUser's ru.* join, SelfUsage, AdminUsage*) must still compile unchanged.
--
-- The rule (ADR-1562), per (run_id, model, lineage_index), legs in lineage_epoch order, per
-- column, a running total R from 0: a per_leg leg does R += v; a session_cumulative leg does
-- R = max(R, v); the model total sums R over lineages; the run total sums over models. That
-- fold is equivalent, closed-form, to
--
--     R = (Σ per_leg v)  +  GREATEST(0, MAX over cumulative legs of (v − s_prefix))
--
-- where s_prefix is the running SUM of the PER_LEG values up to that leg (a cumulative leg's
-- own per_leg term is 0, so its s_prefix is the per_leg sum strictly before it). Proof by
-- induction on the legs in epoch order: a per_leg leg adds v to both R and the Σ term
-- (max term unchanged); a cumulative leg i has R_i = max(R_{i-1}, v_i) = max(S+M, v_i) =
-- S + max(M, v_i − S) with S = Σ per_leg up to i, which is exactly the new max term folded
-- in. GREATEST(0, MAX(...)) is a high-water mark, not "subtract the previous leg": a column
-- that goes down, or a model missing from a later leg, adds nothing; and GREATEST(0, NULL)=0
-- in PostgreSQL, so a lineage with no cumulative leg reduces to the plain per_leg SUM — which
-- is why every existing (all-per_leg, lineage-0) run yields values identical to 00228.
--
-- STRUCTURE. per_leg first MAXes each (run_id, model, lineage_epoch) exactly as 00228's inner
-- query, additionally aggregating bool_or(session_cumulative) and MAX(lineage_index) WITHOUT
-- grouping by them (a single leg can carry rows under two session_ids — a dropped-then-retried
-- resume — so the leg's basis/index must be reduced across those rows, not split). windowed
-- adds the per-(run_id, model, lineage_index) running per_leg sum (s_*). per_lineage applies
-- the closed form per column and folds cost_status over its legs; the outer SELECT SUMs per
-- run_id and re-folds cost_status with 00228's conservative CASE (idempotent across levels).
-- Written as NESTED derived tables (not CTEs) mirroring 00228's shape: sqlc's analyzer
-- resolves the view's output columns through subqueries but stumbles on a WITH inside a
-- CREATE VIEW. Each derived table is referenced ONCE, so PostgreSQL still inlines it and
-- run_id predicates push down.
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
GROUP BY run_id;

-- +goose Down

-- True inverse (goose downs are not run in this deployment; store.Migrate only goes up).
-- Restore 00228's view body FIRST — CREATE OR REPLACE cannot remove a column, and the
-- old body has no reference to the columns we drop next, so it must land before the drop —
-- then drop the two constraints/columns. Restoring the old view KNOWINGLY re-introduces the
-- session-cumulative over-count (it sums resumed legs again), a true inverse of the schema
-- change, not of any collapsed data.
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

ALTER TABLE run_usage DROP COLUMN IF EXISTS lineage_index;
ALTER TABLE run_usage DROP COLUMN IF EXISTS usage_basis;
