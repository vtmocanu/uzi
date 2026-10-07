-- Run with psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 -f scripts/run-budget-measurement.sql
-- Read-only baseline for #2279: latest 200 runs of ALL kinds, then filter issue runs.
-- Re-run with a read-only database role. No identifiers or issue contents are returned.
-- budget_used_seconds matches runs_dto.go: end - started_at - banked pause time,
-- clamped at zero; paused freezes at status_since, terminal at finished_at.
-- Running and cancelled observations are censored, never included in completed percentiles.
-- Current budget_exhausted parks are counted separately from extensions. Historical parks
-- are UNAVAILABLE: hold_reason clears on resume and no durable park event is stored.
-- Extensions include pre-emptive grants and are not evidence of exhaustion.
-- Known-wall overrun excludes extensions/finalize allowance from the original frozen wall;
-- NULL-wall runs cannot be reconstructed after config changes and are counted as unknown.
-- The original non-default-instance baseline (2026-09-26 through 2026-10-05):
-- 158 issue runs / 200, 120 completed; used p50 ~4800s, p90 ~15360s, max ~47280s;
-- 19 extended, 11 using 57600s. Completed milestone buckets 0/1/2/3/4/5+: 34/9/23/32/10/12.
-- A later database snapshot naturally differs. No claim that 24h is measured p95 coverage.
BEGIN READ ONLY;
WITH sample AS (
 SELECT * FROM runs ORDER BY created_at DESC, id DESC LIMIT 200
), observed AS (
 SELECT r.*,
        CASE WHEN jsonb_array_length(COALESCE(milestones_frozen, '[]'::jsonb)) >= 5
             THEN '5+' ELSE jsonb_array_length(COALESCE(milestones_frozen, '[]'::jsonb))::text END AS milestone_bucket,
        CASE WHEN started_at IS NULL THEN NULL ELSE
         GREATEST(0, trunc(extract(epoch FROM (
          CASE WHEN status = 'paused' THEN COALESCE(status_since, now())
               WHEN status IN ('completed','failed','cancelled') THEN COALESCE(finished_at, now())
               ELSE now() END - started_at))) - budget_paused_seconds) END AS used_seconds
 FROM sample r WHERE kind = 'issue'
)
SELECT CASE WHEN GROUPING(milestone_bucket,harness,size_class,model)=15 THEN 'all' ELSE 'group' END AS scope,
       milestone_bucket, harness, size_class, model,
       count(*) AS issue_runs,
       count(*) FILTER (WHERE status='completed') AS completed,
       count(*) FILTER (WHERE status='running') AS running_censored,
       count(*) FILTER (WHERE status='cancelled') AS cancelled_censored,
       count(*) FILTER (WHERE status NOT IN ('completed','failed','cancelled','running')) AS other_incomplete,
       count(*) FILTER (WHERE status='failed') AS failed,
       count(*) FILTER (WHERE status='paused' AND hold_reason='budget_exhausted') AS current_exhausted_parks,
       'unavailable' AS historical_exhausted_parks,
       count(*) FILTER (WHERE budget_extension_seconds>0) AS extended_runs,
       count(*) FILTER (WHERE budget_extension_seconds=57600) AS full_16h_extensions,
       percentile_cont(ARRAY[0.5,0.9,0.95]) WITHIN GROUP (ORDER BY used_seconds)
         FILTER (WHERE status='completed') AS completed_used_percentiles_seconds,
       max(used_seconds) FILTER (WHERE status='completed') AS completed_max_used_seconds,
       count(*) FILTER (WHERE budget_wall_seconds IS NULL) AS original_wall_unknown,
       count(*) FILTER (WHERE used_seconds>budget_wall_seconds) AS known_wall_overrun_runs,
       max(GREATEST(0, used_seconds-budget_wall_seconds)) FILTER (WHERE budget_wall_seconds IS NOT NULL) AS max_known_wall_overrun_seconds,
       min(created_at) AS sample_start, max(created_at) AS sample_end
FROM observed
GROUP BY GROUPING SETS ((), (milestone_bucket,harness,size_class,model))
ORDER BY scope, milestone_bucket, harness, size_class, model;
ROLLBACK;
