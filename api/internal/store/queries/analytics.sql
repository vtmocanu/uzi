-- Issue #4684: scoped run usage (Spend) API, options and authority statements.
--
-- ADR-1184 D1: each statement is separate and an owner-shaped statement carries an
-- unconditional user predicate; never merge an owner and an all-scope statement behind a flag.
--
-- NO statement in this section may reference run_usage, run_usage_totals or
-- run_usage_model_totals: option lists, point checks and history start are authority
-- reads and must stay cheap and independent of the usage fold. A Go text assertion
-- (TestAnalyticsSQLOptionsBlocksAvoidUsageTables) enforces this for every Options, Accessible, Exists and
-- HistoryStart block.

-- name: AnalyticsTimezoneNames :many
SELECT name::text AS name FROM pg_catalog.pg_timezone_names ORDER BY name COLLATE "C";

-- name: AnalyticsOwnerRepoOptions :many
-- Repos on the caller's connections. LIMIT 201 lets the caller serve 200 and flag truncation.
SELECT r.id, r.path_with_namespace
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE c.user_id = @user_id::uuid
ORDER BY r.path_with_namespace COLLATE "C", r.id
LIMIT 201;

-- name: AnalyticsAllRepoOptions :many
SELECT r.id, r.path_with_namespace
FROM repos r
ORDER BY r.path_with_namespace COLLATE "C", r.id
LIMIT 201;

-- name: AnalyticsUserOptions :many
SELECT u.id, u.email
FROM users u
ORDER BY u.email COLLATE "C", u.id
LIMIT 201;

-- name: AnalyticsOwnerRepoAccessible :one
SELECT EXISTS (
    SELECT 1
    FROM repos r
    JOIN forge_connections c ON c.id = r.connection_id
    WHERE r.id = @repo_id::uuid AND c.user_id = @user_id::uuid
)::boolean AS accessible;

-- name: AnalyticsRepoExists :one
SELECT EXISTS (SELECT 1 FROM repos WHERE id = @repo_id::uuid)::boolean AS present;

-- name: AnalyticsUserExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = @user_id::uuid)::boolean AS present;

-- name: AnalyticsOwnerHistoryStart :one
-- Earliest created_at of the caller's runs in scope; NULL when the scope is empty.
SELECT MIN(r.created_at)::timestamptz AS history_start
FROM runs r
WHERE r.user_id = @user_id::uuid
  AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
  AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
  AND r.kind = ANY(@kinds::text[]);

-- name: AnalyticsAllHistoryStart :one
SELECT MIN(r.created_at)::timestamptz AS history_start
FROM runs r
WHERE (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
  AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
  AND r.kind = ANY(@kinds::text[]);

-- name: AnalyticsOwnerSpend :one
-- Issue #4684: the Spend document, one jsonb value built over ONE materialized cohort and fold.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- Every range comes from the parameters (half-open: start <= created_at < end); there is no
-- calendar arithmetic here. Dollars are emitted as numeric strings, ratios as JSON numbers,
-- timestamps as UTC strings so the document does not depend on the session TimeZone.
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
rp AS (
    SELECT t.ord::int AS idx, t.s AS r_start, t.e AS r_end
    FROM ROWS FROM (unnest(@roll_starts::timestamptz[]), unnest(@roll_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
bnd AS (
    SELECT COALESCE(array_agg(d.x ORDER BY d.x), '{}'::timestamptz[]) AS arr
    FROM (SELECT r_start AS x FROM rp UNION SELECT r_end FROM rp) d
),
cohort AS MATERIALIZED (
    SELECT r.id, r.user_id, u.email, r.repo_id, rpo.path_with_namespace AS repo_path,
           (@reveal_repos::boolean IS TRUE OR fc.id IS NOT NULL) AS repo_shown,  -- owner-only
           r.kind, r.harness, r.status, r.created_at, r.finished_at, r.status_since, r.mr_iid
    FROM runs r
    JOIN users u ON u.id = r.user_id
    LEFT JOIN repos rpo ON rpo.id = r.repo_id
    LEFT JOIN forge_connections fc ON fc.id = rpo.connection_id AND fc.user_id = @user_id::uuid  -- owner-only
    WHERE r.created_at >= @fold_from::timestamptz AND r.created_at < @now::timestamptz
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
),
model_fold AS MATERIALIZED (
    SELECT c.id AS run_id, m.model, m.input_tokens, m.cache_read_tokens, m.cache_creation_tokens,
           m.output_tokens, m.cost_usd, m.cost_status
    FROM cohort c
    LEFT JOIN LATERAL (
        SELECT t.model, t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens,
               t.output_tokens, t.cost_usd, t.cost_status
        FROM run_usage_model_totals t
        WHERE t.run_id = c.id
        OFFSET 0
    ) m ON true
    WHERE c.kind <> 'chat'
),
merged_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
      AND w.user_id = @user_id::uuid  -- owner-only
),
run_fold AS MATERIALIZED (
    SELECT c.id AS run_id, c.user_id, c.email, c.repo_id, c.kind, c.harness, c.status AS run_status,
           c.created_at, c.finished_at, c.status_since, c.mr_iid,
           CASE WHEN c.repo_id IS NULL THEN 'no_repo'
                WHEN c.repo_shown THEN c.repo_id::text
                ELSE 'inaccessible_repo' END AS repo_key,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_id END AS repo_id_out,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_path END AS repo_path_out,
           (c.kind = ANY(@delivery_kinds::text[])) AS is_delivery,
           CASE WHEN c.created_at >= sqlc.arg('from')::timestamptz THEN 'cur'
                WHEN sqlc.narg('previous_from')::timestamptz IS NOT NULL AND c.created_at >= sqlc.narg('previous_from')::timestamptz AND c.created_at < sqlc.arg('from')::timestamptz THEN 'prev'
           END AS win,
           f.input_tokens, f.cache_read_tokens, f.cache_creation_tokens, f.output_tokens, f.cost,
           f.usage_status, f.primary_model,
           o.outcome,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL
                AND (c.repo_id, c.mr_iid) IN (SELECT mw.repo_id, mw.mr_iid FROM merged_witness mw)) AS witnessed,
           width_bucket(c.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM cohort c
    JOIN (
        SELECT mf.run_id,
               COALESCE(sum(mf.input_tokens), 0) AS input_tokens,
               COALESCE(sum(mf.cache_read_tokens), 0) AS cache_read_tokens,
               COALESCE(sum(mf.cache_creation_tokens), 0) AS cache_creation_tokens,
               COALESCE(sum(mf.output_tokens), 0) AS output_tokens,
               COALESCE(sum(mf.cost_usd), 0) AS cost,
               CASE WHEN count(mf.model) = 0 THEN 'missing'
                    WHEN bool_or(mf.cost_status = 'unreported') THEN 'unreported'
                    WHEN bool_or(mf.cost_status = 'metered') AND bool_or(mf.cost_status = 'subscription') THEN 'unreported'
                    WHEN bool_or(mf.cost_status = 'subscription') THEN 'subscription'
                    ELSE 'metered' END AS usage_status,
               COALESCE((array_agg(mf.model ORDER BY mf.output_tokens DESC, mf.model COLLATE "C" ASC)
                         FILTER (WHERE mf.model IS NOT NULL))[1], 'unknown') AS primary_model
        FROM model_fold mf
        GROUP BY mf.run_id
    ) f ON f.run_id = c.id
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0
    ) o ON true
    WHERE c.kind <> 'chat'
),
rfb AS MATERIALIZED (
    SELECT rf.*,
           CASE WHEN rf.win = 'cur' AND rf.wb >= 1 AND rf.created_at < (@bucket_ends::timestamptz[])[rf.wb] THEN rf.wb END AS bidx
    FROM run_fold rf
),
tot_cur AS (SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM rfb x WHERE x.win = 'cur'),
tot_prev AS (SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM rfb x WHERE x.win = 'prev'),
tot_cost AS (SELECT COALESCE(sum(x.cost), 0) AS cost, count(*) AS runs FROM rfb x WHERE x.win = 'cur'),
overall_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j
    FROM bk LEFT JOIN rfb x ON x.bidx = bk.idx
    GROUP BY bk.idx
),
split_items AS MATERIALIZED (
    SELECT x.run_id, x.usage_status, x.bidx, x.cost, x.input_tokens, x.cache_read_tokens,
           x.cache_creation_tokens, x.output_tokens,
           CASE @split::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @split::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @split::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur' AND @split::text <> 'usage_model'
    UNION ALL
    SELECT mf.run_id, x.usage_status, x.bidx, COALESCE(mf.cost_usd, 0), COALESCE(mf.input_tokens, 0),
           COALESCE(mf.cache_read_tokens, 0), COALESCE(mf.cache_creation_tokens, 0), COALESCE(mf.output_tokens, 0),
           COALESCE(mf.model, 'unknown'), NULL::text, NULL::uuid
    FROM model_fold mf
    JOIN rfb x ON x.run_id = mf.run_id
    WHERE x.win = 'cur' AND @split::text = 'usage_model'
),
split_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gsum DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, sum(i.cost) OVER (PARTITION BY i.gkey) AS gsum FROM split_items i) y
),
split_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM split_ranked r
    GROUP BY r.gkey
),
split_slotted AS MATERIALIZED (
    SELECT r.*, LEAST(r.rnk, 6) AS slot
    FROM split_ranked r
),
split_tot AS (
    SELECT x.slot, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x GROUP BY x.slot
),
split_cell_agg AS (
    SELECT x.slot, x.bidx, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x GROUP BY x.slot, x.bidx
),
split_cell_empty AS (
    SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x WHERE false
),
split_cells AS MATERIALIZED (
    SELECT s.slot, bk.idx, COALESCE(a.j, (SELECT e.j FROM split_cell_empty e)) AS j
    FROM (SELECT DISTINCT slot FROM split_slotted) s
    CROSS JOIN bk
    LEFT JOIN split_cell_agg a ON a.slot = s.slot AND a.bidx = bk.idx
),
cache_cur AS (
    SELECT COALESCE(sum(x.cache_read_tokens), 0)::bigint AS read_tokens,
           COALESCE(sum(x.input_tokens), 0)::bigint AS input_tokens,
           COALESCE(sum(x.cache_creation_tokens), 0)::bigint AS creation_tokens,
           jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0) AS disclosure
    FROM rfb x WHERE x.win = 'cur'
),
top_runs AS (
    SELECT x.* FROM rfb x WHERE x.win = 'cur'
    ORDER BY x.cost DESC, x.created_at DESC, x.run_id
    LIMIT 10
),
pr_agg AS (
    SELECT rf.repo_id, rf.mr_iid, bool_or(rf.witnessed) AS wit,
           min(rf.created_at) FILTER (WHERE rf.win = 'cur') AS t_cur,
               COALESCE(sum(rf.cost) FILTER (WHERE rf.win = 'cur'), 0) AS num_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'metered'))::int AS m_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'subscription'))::int AS s_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'unreported'))::int AS u_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'missing'))::int AS x_cur,
               min(rf.created_at) FILTER (WHERE rf.win = 'prev') AS t_prev,
               COALESCE(sum(rf.cost) FILTER (WHERE rf.win = 'prev'), 0) AS num_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'metered'))::int AS m_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'subscription'))::int AS s_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'unreported'))::int AS u_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'missing'))::int AS x_prev,
               min(rf.created_at) AS t_fold,
               COALESCE(sum(rf.cost), 0) AS num_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'metered'))::int AS m_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'subscription'))::int AS s_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'unreported'))::int AS u_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'missing'))::int AS x_fold
    FROM run_fold rf
    WHERE rf.is_delivery AND rf.mr_iid IS NOT NULL AND rf.repo_id IS NOT NULL
    GROUP BY rf.repo_id, rf.mr_iid
),
ev AS (
    SELECT 'cpr' AS tag, rf.created_at AS t, rf.cost AS num,
           (rf.is_delivery AND rf.outcome = 'completed')::int AS den,
           (rf.usage_status = 'metered')::int AS m, (rf.usage_status = 'subscription')::int AS s,
           (rf.usage_status = 'unreported')::int AS u, (rf.usage_status = 'missing')::int AS x
    FROM run_fold rf
    UNION ALL
    SELECT 'pr_cur' AS tag, p.t_cur AS t, p.num_cur AS num, p.wit::int AS den,
           p.m_cur AS m, p.s_cur AS s, p.u_cur AS u, p.x_cur AS x
    FROM pr_agg p WHERE p.t_cur IS NOT NULL
    UNION ALL
    SELECT 'pr_prev' AS tag, p.t_prev AS t, p.num_prev AS num, p.wit::int AS den,
           p.m_prev AS m, p.s_prev AS s, p.u_prev AS u, p.x_prev AS x
    FROM pr_agg p WHERE p.t_prev IS NOT NULL
    UNION ALL
    SELECT 'pr_fold' AS tag, p.t_fold AS t, p.num_fold AS num, p.wit::int AS den,
           p.m_fold AS m, p.s_fold AS s, p.u_fold AS u, p.x_fold AS x
    FROM pr_agg p WHERE p.t_fold IS NOT NULL
),
ev_b AS (
    SELECT e.tag, e.num, e.den, e.m, e.s, e.u, e.x,
           CASE WHEN e.wb >= 1 AND e.t < (@bucket_ends::timestamptz[])[e.wb] THEN e.wb END AS bidx
    FROM (SELECT ev.*, width_bucket(ev.t, @bucket_starts::timestamptz[]) AS wb FROM ev WHERE ev.tag IN ('cpr', 'pr_cur')) e
),
ev_seg AS (
    SELECT e.tag, width_bucket(e.t, (SELECT arr FROM bnd)) AS seg,
           sum(e.num) AS num, sum(e.den) AS den, sum(e.m) AS m, sum(e.s) AS s, sum(e.u) AS u, sum(e.x) AS x
    FROM ev e
    WHERE e.tag IN ('cpr', 'pr_fold')
    GROUP BY e.tag, width_bucket(e.t, (SELECT arr FROM bnd))
),
cpr_cur AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'cpr' AND e.t >= sqlc.arg('from')::timestamptz),
cpr_prev AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'cpr' AND sqlc.narg('previous_from')::timestamptz IS NOT NULL AND e.t >= sqlc.narg('previous_from')::timestamptz AND e.t < sqlc.arg('from')::timestamptz),
cpr_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j
    FROM bk
    LEFT JOIN ev_b e ON e.tag = 'cpr' AND e.bidx = bk.idx
    GROUP BY bk.idx
),
cpr_roll AS (
    SELECT rp.idx, jsonb_build_object(
            'numerator', COALESCE(sum(g.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(g.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN NULL ELSE round(sum(g.num) / sum(g.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(g.m), 0)::bigint, 'subscription', COALESCE(sum(g.s), 0)::bigint, 'unreported', COALESCE(sum(g.u), 0)::bigint, 'missing', COALESCE(sum(g.x), 0)::bigint, 'incomplete', COALESCE(sum(g.s + g.u + g.x), 0) > 0)) AS j
    FROM rp
    LEFT JOIN ev_seg g ON g.tag = 'cpr'
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY rp.idx
),
pr_cur AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'pr_cur'),
pr_prev AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'pr_prev'),
pr_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j
    FROM bk
    LEFT JOIN ev_b e ON e.tag = 'pr_cur' AND e.bidx = bk.idx
    GROUP BY bk.idx
),
pr_roll AS (
    SELECT rp.idx, jsonb_build_object(
            'numerator', COALESCE(sum(g.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(g.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN NULL ELSE round(sum(g.num) / sum(g.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(g.m), 0)::bigint, 'subscription', COALESCE(sum(g.s), 0)::bigint, 'unreported', COALESCE(sum(g.u), 0)::bigint, 'missing', COALESCE(sum(g.x), 0)::bigint, 'incomplete', COALESCE(sum(g.s + g.u + g.x), 0) > 0)) AS j
    FROM rp
    LEFT JOIN ev_seg g ON g.tag = 'pr_fold'
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY rp.idx
),
bd_items AS MATERIALIZED (
    SELECT x.*,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @breakdown::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur'
),
bd_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gsum DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, sum(i.cost) OVER (PARTITION BY i.gkey) AS gsum FROM bd_items i) y
),
bd_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM bd_ranked r
    GROUP BY r.gkey
),
bd_rows AS (
    SELECT LEAST(x.rnk, 51) AS slot,
           jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END))
           || jsonb_build_object(
               'finished', count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL),
               'outcomes', jsonb_build_object(
                   'completed', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'completed'),
                   'cancelled', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'cancelled'),
                   'plan_rejected', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'plan_rejected'),
                   'base_diverged', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'base_diverged'),
                   'failed', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'failed')),
               'failure_rate', jsonb_build_object('value', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'failed')::numeric / count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL), 6) END, 'reason', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'completion_ratio', jsonb_build_object('value', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'completed')::numeric / count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL), 6) END, 'reason', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'cost_share', jsonb_build_object('value', CASE WHEN (SELECT tc.cost FROM tot_cost tc) = 0 THEN NULL ELSE round(COALESCE(sum(x.cost), 0) / (SELECT tc.cost FROM tot_cost tc), 6) END, 'reason', CASE WHEN (SELECT tc.cost FROM tot_cost tc) = 0 THEN 'not_applicable' END),
               'elapsed_p50_seconds', jsonb_build_object('value', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (COALESCE(x.finished_at, x.status_since) - x.created_at))) FILTER (WHERE x.outcome IS NOT NULL) END, 'reason', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'elapsed_p90_seconds', jsonb_build_object('value', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (COALESCE(x.finished_at, x.status_since) - x.created_at))) FILTER (WHERE x.outcome IS NOT NULL) END, 'reason', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'merged_prs', count(DISTINCT (x.repo_id, x.mr_iid)) FILTER (WHERE x.is_delivery AND x.witnessed)) AS j
    FROM bd_ranked x
    GROUP BY LEAST(x.rnk, 51)
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'untracked_chat_runs', (SELECT count(*) FROM cohort c WHERE c.kind = 'chat' AND c.created_at >= sqlc.arg('from')::timestamptz),
    'totals', jsonb_build_object('current', (SELECT j FROM tot_cur))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM tot_prev)) END,
    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM overall_cells c), '[]'::jsonb),
    'split', jsonb_build_object(
        'dimension', @split::text,
        'non_additive', @split::text = 'usage_model',
        'groups', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', st.slot,
                    'is_other', st.slot = 6,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid,
                    'totals', st.j,
                    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', sc.idx - 1) || sc.j ORDER BY sc.idx)
                                         FROM split_cells sc WHERE sc.slot = st.slot), '[]'::jsonb))
                ORDER BY st.slot)
            FROM split_tot st
            LEFT JOIN split_groups g ON g.rnk = st.slot AND st.slot < 6), '[]'::jsonb)),
    'cost_per_completed_run', jsonb_build_object(
            'current', (SELECT j FROM cpr_cur),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cpr_cells c), '[]'::jsonb),
            'rolling', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cpr_roll c), '[]'::jsonb))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM cpr_prev)) END,
    'cost_per_merged_pr', jsonb_build_object(
            'current', (SELECT j FROM pr_cur),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM pr_cells c), '[]'::jsonb),
            'rolling', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM pr_roll c), '[]'::jsonb))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM pr_prev)) END,
    'cache_read', (
        SELECT jsonb_build_object(
            'cache_read_tokens', cc.read_tokens,
            'input_tokens', cc.input_tokens,
            'cache_creation_tokens', cc.creation_tokens,
            'denominator', cc.read_tokens + cc.input_tokens + cc.creation_tokens,
            'value', CASE WHEN cc.read_tokens + cc.input_tokens + cc.creation_tokens = 0 THEN NULL
                          ELSE round(cc.read_tokens::numeric / (cc.read_tokens + cc.input_tokens + cc.creation_tokens), 6) END,
            'reason', CASE WHEN cc.read_tokens + cc.input_tokens + cc.creation_tokens = 0 THEN 'no_input_tokens' END,
            'disclosure', cc.disclosure)
        FROM cache_cur cc),
    'top_runs', COALESCE((
        SELECT jsonb_agg(jsonb_build_object(
            'run_id', t.run_id, 'kind', t.kind, 'harness', t.harness, 'run_status', t.run_status,
            'usage_status', t.usage_status, 'primary_model', t.primary_model,
            'created_at', to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'cost', t.cost::numeric(24,6)::text,
            'input_tokens', t.input_tokens::bigint, 'cache_read_tokens', t.cache_read_tokens::bigint,
            'cache_creation_tokens', t.cache_creation_tokens::bigint, 'output_tokens', t.output_tokens::bigint,
            'repo_key', t.repo_key, 'repo_id', t.repo_id_out, 'repo_path', t.repo_path_out,
            'user_id', t.user_id, 'user_email', t.email)
            ORDER BY t.cost DESC, t.created_at DESC, t.run_id)
        FROM top_runs t), '[]'::jsonb),
    'breakdown', jsonb_build_object(
        'dimension', @breakdown::text,
        'rows', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', r.slot,
                    'is_other', r.slot = 51,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid)
                || r.j
                ORDER BY r.slot)
            FROM bd_rows r
            LEFT JOIN bd_groups g ON g.rnk = r.slot AND r.slot < 51), '[]'::jsonb))
)::jsonb AS doc;

-- name: AnalyticsAllSpend :one
-- Issue #4684: the Spend document, one jsonb value built over ONE materialized cohort and fold.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- Every range comes from the parameters (half-open: start <= created_at < end); there is no
-- calendar arithmetic here. Dollars are emitted as numeric strings, ratios as JSON numbers,
-- timestamps as UTC strings so the document does not depend on the session TimeZone.
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
rp AS (
    SELECT t.ord::int AS idx, t.s AS r_start, t.e AS r_end
    FROM ROWS FROM (unnest(@roll_starts::timestamptz[]), unnest(@roll_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
bnd AS (
    SELECT COALESCE(array_agg(d.x ORDER BY d.x), '{}'::timestamptz[]) AS arr
    FROM (SELECT r_start AS x FROM rp UNION SELECT r_end FROM rp) d
),
cohort AS MATERIALIZED (
    SELECT r.id, r.user_id, u.email, r.repo_id, rpo.path_with_namespace AS repo_path,
           true AS repo_shown,  -- all-only
           r.kind, r.harness, r.status, r.created_at, r.finished_at, r.status_since, r.mr_iid
    FROM runs r
    JOIN users u ON u.id = r.user_id
    LEFT JOIN repos rpo ON rpo.id = r.repo_id
    WHERE r.created_at >= @fold_from::timestamptz AND r.created_at < @now::timestamptz
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
),
model_fold AS MATERIALIZED (
    SELECT c.id AS run_id, m.model, m.input_tokens, m.cache_read_tokens, m.cache_creation_tokens,
           m.output_tokens, m.cost_usd, m.cost_status
    FROM cohort c
    LEFT JOIN LATERAL (
        SELECT t.model, t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens,
               t.output_tokens, t.cost_usd, t.cost_status
        FROM run_usage_model_totals t
        WHERE t.run_id = c.id
        OFFSET 0
    ) m ON true
    WHERE c.kind <> 'chat'
),
merged_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
),
run_fold AS MATERIALIZED (
    SELECT c.id AS run_id, c.user_id, c.email, c.repo_id, c.kind, c.harness, c.status AS run_status,
           c.created_at, c.finished_at, c.status_since, c.mr_iid,
           CASE WHEN c.repo_id IS NULL THEN 'no_repo'
                WHEN c.repo_shown THEN c.repo_id::text
                ELSE 'inaccessible_repo' END AS repo_key,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_id END AS repo_id_out,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_path END AS repo_path_out,
           (c.kind = ANY(@delivery_kinds::text[])) AS is_delivery,
           CASE WHEN c.created_at >= sqlc.arg('from')::timestamptz THEN 'cur'
                WHEN sqlc.narg('previous_from')::timestamptz IS NOT NULL AND c.created_at >= sqlc.narg('previous_from')::timestamptz AND c.created_at < sqlc.arg('from')::timestamptz THEN 'prev'
           END AS win,
           f.input_tokens, f.cache_read_tokens, f.cache_creation_tokens, f.output_tokens, f.cost,
           f.usage_status, f.primary_model,
           o.outcome,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL
                AND (c.repo_id, c.mr_iid) IN (SELECT mw.repo_id, mw.mr_iid FROM merged_witness mw)) AS witnessed,
           width_bucket(c.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM cohort c
    JOIN (
        SELECT mf.run_id,
               COALESCE(sum(mf.input_tokens), 0) AS input_tokens,
               COALESCE(sum(mf.cache_read_tokens), 0) AS cache_read_tokens,
               COALESCE(sum(mf.cache_creation_tokens), 0) AS cache_creation_tokens,
               COALESCE(sum(mf.output_tokens), 0) AS output_tokens,
               COALESCE(sum(mf.cost_usd), 0) AS cost,
               CASE WHEN count(mf.model) = 0 THEN 'missing'
                    WHEN bool_or(mf.cost_status = 'unreported') THEN 'unreported'
                    WHEN bool_or(mf.cost_status = 'metered') AND bool_or(mf.cost_status = 'subscription') THEN 'unreported'
                    WHEN bool_or(mf.cost_status = 'subscription') THEN 'subscription'
                    ELSE 'metered' END AS usage_status,
               COALESCE((array_agg(mf.model ORDER BY mf.output_tokens DESC, mf.model COLLATE "C" ASC)
                         FILTER (WHERE mf.model IS NOT NULL))[1], 'unknown') AS primary_model
        FROM model_fold mf
        GROUP BY mf.run_id
    ) f ON f.run_id = c.id
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0
    ) o ON true
    WHERE c.kind <> 'chat'
),
rfb AS MATERIALIZED (
    SELECT rf.*,
           CASE WHEN rf.win = 'cur' AND rf.wb >= 1 AND rf.created_at < (@bucket_ends::timestamptz[])[rf.wb] THEN rf.wb END AS bidx
    FROM run_fold rf
),
tot_cur AS (SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM rfb x WHERE x.win = 'cur'),
tot_prev AS (SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM rfb x WHERE x.win = 'prev'),
tot_cost AS (SELECT COALESCE(sum(x.cost), 0) AS cost, count(*) AS runs FROM rfb x WHERE x.win = 'cur'),
overall_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j
    FROM bk LEFT JOIN rfb x ON x.bidx = bk.idx
    GROUP BY bk.idx
),
split_items AS MATERIALIZED (
    SELECT x.run_id, x.usage_status, x.bidx, x.cost, x.input_tokens, x.cache_read_tokens,
           x.cache_creation_tokens, x.output_tokens,
           CASE @split::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @split::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @split::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur' AND @split::text <> 'usage_model'
    UNION ALL
    SELECT mf.run_id, x.usage_status, x.bidx, COALESCE(mf.cost_usd, 0), COALESCE(mf.input_tokens, 0),
           COALESCE(mf.cache_read_tokens, 0), COALESCE(mf.cache_creation_tokens, 0), COALESCE(mf.output_tokens, 0),
           COALESCE(mf.model, 'unknown'), NULL::text, NULL::uuid
    FROM model_fold mf
    JOIN rfb x ON x.run_id = mf.run_id
    WHERE x.win = 'cur' AND @split::text = 'usage_model'
),
split_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gsum DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, sum(i.cost) OVER (PARTITION BY i.gkey) AS gsum FROM split_items i) y
),
split_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM split_ranked r
    GROUP BY r.gkey
),
split_slotted AS MATERIALIZED (
    SELECT r.*, LEAST(r.rnk, 6) AS slot
    FROM split_ranked r
),
split_tot AS (
    SELECT x.slot, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x GROUP BY x.slot
),
split_cell_agg AS (
    SELECT x.slot, x.bidx, jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x GROUP BY x.slot, x.bidx
),
split_cell_empty AS (
    SELECT jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END)) AS j FROM split_slotted x WHERE false
),
split_cells AS MATERIALIZED (
    SELECT s.slot, bk.idx, COALESCE(a.j, (SELECT e.j FROM split_cell_empty e)) AS j
    FROM (SELECT DISTINCT slot FROM split_slotted) s
    CROSS JOIN bk
    LEFT JOIN split_cell_agg a ON a.slot = s.slot AND a.bidx = bk.idx
),
cache_cur AS (
    SELECT COALESCE(sum(x.cache_read_tokens), 0)::bigint AS read_tokens,
           COALESCE(sum(x.input_tokens), 0)::bigint AS input_tokens,
           COALESCE(sum(x.cache_creation_tokens), 0)::bigint AS creation_tokens,
           jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0) AS disclosure
    FROM rfb x WHERE x.win = 'cur'
),
top_runs AS (
    SELECT x.* FROM rfb x WHERE x.win = 'cur'
    ORDER BY x.cost DESC, x.created_at DESC, x.run_id
    LIMIT 10
),
pr_agg AS (
    SELECT rf.repo_id, rf.mr_iid, bool_or(rf.witnessed) AS wit,
           min(rf.created_at) FILTER (WHERE rf.win = 'cur') AS t_cur,
               COALESCE(sum(rf.cost) FILTER (WHERE rf.win = 'cur'), 0) AS num_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'metered'))::int AS m_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'subscription'))::int AS s_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'unreported'))::int AS u_cur,
               (count(*) FILTER (WHERE rf.win = 'cur' AND rf.usage_status = 'missing'))::int AS x_cur,
               min(rf.created_at) FILTER (WHERE rf.win = 'prev') AS t_prev,
               COALESCE(sum(rf.cost) FILTER (WHERE rf.win = 'prev'), 0) AS num_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'metered'))::int AS m_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'subscription'))::int AS s_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'unreported'))::int AS u_prev,
               (count(*) FILTER (WHERE rf.win = 'prev' AND rf.usage_status = 'missing'))::int AS x_prev,
               min(rf.created_at) AS t_fold,
               COALESCE(sum(rf.cost), 0) AS num_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'metered'))::int AS m_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'subscription'))::int AS s_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'unreported'))::int AS u_fold,
               (count(*) FILTER (WHERE rf.usage_status = 'missing'))::int AS x_fold
    FROM run_fold rf
    WHERE rf.is_delivery AND rf.mr_iid IS NOT NULL AND rf.repo_id IS NOT NULL
    GROUP BY rf.repo_id, rf.mr_iid
),
ev AS (
    SELECT 'cpr' AS tag, rf.created_at AS t, rf.cost AS num,
           (rf.is_delivery AND rf.outcome = 'completed')::int AS den,
           (rf.usage_status = 'metered')::int AS m, (rf.usage_status = 'subscription')::int AS s,
           (rf.usage_status = 'unreported')::int AS u, (rf.usage_status = 'missing')::int AS x
    FROM run_fold rf
    UNION ALL
    SELECT 'pr_cur' AS tag, p.t_cur AS t, p.num_cur AS num, p.wit::int AS den,
           p.m_cur AS m, p.s_cur AS s, p.u_cur AS u, p.x_cur AS x
    FROM pr_agg p WHERE p.t_cur IS NOT NULL
    UNION ALL
    SELECT 'pr_prev' AS tag, p.t_prev AS t, p.num_prev AS num, p.wit::int AS den,
           p.m_prev AS m, p.s_prev AS s, p.u_prev AS u, p.x_prev AS x
    FROM pr_agg p WHERE p.t_prev IS NOT NULL
    UNION ALL
    SELECT 'pr_fold' AS tag, p.t_fold AS t, p.num_fold AS num, p.wit::int AS den,
           p.m_fold AS m, p.s_fold AS s, p.u_fold AS u, p.x_fold AS x
    FROM pr_agg p WHERE p.t_fold IS NOT NULL
),
ev_b AS (
    SELECT e.tag, e.num, e.den, e.m, e.s, e.u, e.x,
           CASE WHEN e.wb >= 1 AND e.t < (@bucket_ends::timestamptz[])[e.wb] THEN e.wb END AS bidx
    FROM (SELECT ev.*, width_bucket(ev.t, @bucket_starts::timestamptz[]) AS wb FROM ev WHERE ev.tag IN ('cpr', 'pr_cur')) e
),
ev_seg AS (
    SELECT e.tag, width_bucket(e.t, (SELECT arr FROM bnd)) AS seg,
           sum(e.num) AS num, sum(e.den) AS den, sum(e.m) AS m, sum(e.s) AS s, sum(e.u) AS u, sum(e.x) AS x
    FROM ev e
    WHERE e.tag IN ('cpr', 'pr_fold')
    GROUP BY e.tag, width_bucket(e.t, (SELECT arr FROM bnd))
),
cpr_cur AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'cpr' AND e.t >= sqlc.arg('from')::timestamptz),
cpr_prev AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'cpr' AND sqlc.narg('previous_from')::timestamptz IS NOT NULL AND e.t >= sqlc.narg('previous_from')::timestamptz AND e.t < sqlc.arg('from')::timestamptz),
cpr_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j
    FROM bk
    LEFT JOIN ev_b e ON e.tag = 'cpr' AND e.bidx = bk.idx
    GROUP BY bk.idx
),
cpr_roll AS (
    SELECT rp.idx, jsonb_build_object(
            'numerator', COALESCE(sum(g.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(g.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN NULL ELSE round(sum(g.num) / sum(g.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN 'no_completed_delivery_runs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(g.m), 0)::bigint, 'subscription', COALESCE(sum(g.s), 0)::bigint, 'unreported', COALESCE(sum(g.u), 0)::bigint, 'missing', COALESCE(sum(g.x), 0)::bigint, 'incomplete', COALESCE(sum(g.s + g.u + g.x), 0) > 0)) AS j
    FROM rp
    LEFT JOIN ev_seg g ON g.tag = 'cpr'
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY rp.idx
),
pr_cur AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'pr_cur'),
pr_prev AS (SELECT jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j FROM ev e WHERE e.tag = 'pr_prev'),
pr_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'numerator', COALESCE(sum(e.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(e.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN NULL ELSE round(sum(e.num) / sum(e.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(e.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(e.m), 0)::bigint, 'subscription', COALESCE(sum(e.s), 0)::bigint, 'unreported', COALESCE(sum(e.u), 0)::bigint, 'missing', COALESCE(sum(e.x), 0)::bigint, 'incomplete', COALESCE(sum(e.s + e.u + e.x), 0) > 0)) AS j
    FROM bk
    LEFT JOIN ev_b e ON e.tag = 'pr_cur' AND e.bidx = bk.idx
    GROUP BY bk.idx
),
pr_roll AS (
    SELECT rp.idx, jsonb_build_object(
            'numerator', COALESCE(sum(g.num), 0)::numeric(24,6)::text,
            'denominator', COALESCE(sum(g.den), 0)::bigint,
            'value', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN NULL ELSE round(sum(g.num) / sum(g.den), 6)::text END,
            'reason', CASE WHEN COALESCE(sum(g.den), 0) = 0 THEN 'no_merged_prs' END,
            'disclosure', jsonb_build_object('metered', COALESCE(sum(g.m), 0)::bigint, 'subscription', COALESCE(sum(g.s), 0)::bigint, 'unreported', COALESCE(sum(g.u), 0)::bigint, 'missing', COALESCE(sum(g.x), 0)::bigint, 'incomplete', COALESCE(sum(g.s + g.u + g.x), 0) > 0)) AS j
    FROM rp
    LEFT JOIN ev_seg g ON g.tag = 'pr_fold'
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY rp.idx
),
bd_items AS MATERIALIZED (
    SELECT x.*,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @breakdown::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur'
),
bd_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gsum DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, sum(i.cost) OVER (PARTITION BY i.gkey) AS gsum FROM bd_items i) y
),
bd_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM bd_ranked r
    GROUP BY r.gkey
),
bd_rows AS (
    SELECT LEAST(x.rnk, 51) AS slot,
           jsonb_build_object(
            'cost', COALESCE(sum(x.cost), 0)::numeric(24,6)::text,
            'input_tokens', COALESCE(sum(x.input_tokens), 0)::bigint,
            'cache_read_tokens', COALESCE(sum(x.cache_read_tokens), 0)::bigint,
            'cache_creation_tokens', COALESCE(sum(x.cache_creation_tokens), 0)::bigint,
            'output_tokens', COALESCE(sum(x.output_tokens), 0)::bigint,
            'runs', count(DISTINCT x.run_id),
            'disclosure', jsonb_build_object('metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'), 'subscription', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'subscription'), 'unreported', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'unreported'), 'missing', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'missing'), 'incomplete', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status <> 'metered') > 0),
            'coverage', jsonb_build_object(
                'metered', count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered'),
                'applicable', count(DISTINCT x.run_id),
                'value', CASE WHEN count(DISTINCT x.run_id) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.usage_status = 'metered')::numeric / count(DISTINCT x.run_id), 6) END,
                'reason', CASE WHEN count(DISTINCT x.run_id) = 0 THEN 'no_applicable_runs' END))
           || jsonb_build_object(
               'finished', count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL),
               'outcomes', jsonb_build_object(
                   'completed', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'completed'),
                   'cancelled', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'cancelled'),
                   'plan_rejected', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'plan_rejected'),
                   'base_diverged', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'base_diverged'),
                   'failed', count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'failed')),
               'failure_rate', jsonb_build_object('value', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'failed')::numeric / count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL), 6) END, 'reason', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'completion_ratio', jsonb_build_object('value', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE round(count(DISTINCT x.run_id) FILTER (WHERE x.outcome = 'completed')::numeric / count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL), 6) END, 'reason', CASE WHEN count(DISTINCT x.run_id) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'cost_share', jsonb_build_object('value', CASE WHEN (SELECT tc.cost FROM tot_cost tc) = 0 THEN NULL ELSE round(COALESCE(sum(x.cost), 0) / (SELECT tc.cost FROM tot_cost tc), 6) END, 'reason', CASE WHEN (SELECT tc.cost FROM tot_cost tc) = 0 THEN 'not_applicable' END),
               'elapsed_p50_seconds', jsonb_build_object('value', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (COALESCE(x.finished_at, x.status_since) - x.created_at))) FILTER (WHERE x.outcome IS NOT NULL) END, 'reason', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'elapsed_p90_seconds', jsonb_build_object('value', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN NULL ELSE percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (COALESCE(x.finished_at, x.status_since) - x.created_at))) FILTER (WHERE x.outcome IS NOT NULL) END, 'reason', CASE WHEN count(*) FILTER (WHERE x.outcome IS NOT NULL) = 0 THEN 'no_finished_runs' END),
               'merged_prs', count(DISTINCT (x.repo_id, x.mr_iid)) FILTER (WHERE x.is_delivery AND x.witnessed)) AS j
    FROM bd_ranked x
    GROUP BY LEAST(x.rnk, 51)
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'untracked_chat_runs', (SELECT count(*) FROM cohort c WHERE c.kind = 'chat' AND c.created_at >= sqlc.arg('from')::timestamptz),
    'totals', jsonb_build_object('current', (SELECT j FROM tot_cur))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM tot_prev)) END,
    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM overall_cells c), '[]'::jsonb),
    'split', jsonb_build_object(
        'dimension', @split::text,
        'non_additive', @split::text = 'usage_model',
        'groups', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', st.slot,
                    'is_other', st.slot = 6,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid,
                    'totals', st.j,
                    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', sc.idx - 1) || sc.j ORDER BY sc.idx)
                                         FROM split_cells sc WHERE sc.slot = st.slot), '[]'::jsonb))
                ORDER BY st.slot)
            FROM split_tot st
            LEFT JOIN split_groups g ON g.rnk = st.slot AND st.slot < 6), '[]'::jsonb)),
    'cost_per_completed_run', jsonb_build_object(
            'current', (SELECT j FROM cpr_cur),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cpr_cells c), '[]'::jsonb),
            'rolling', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cpr_roll c), '[]'::jsonb))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM cpr_prev)) END,
    'cost_per_merged_pr', jsonb_build_object(
            'current', (SELECT j FROM pr_cur),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM pr_cells c), '[]'::jsonb),
            'rolling', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM pr_roll c), '[]'::jsonb))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT j FROM pr_prev)) END,
    'cache_read', (
        SELECT jsonb_build_object(
            'cache_read_tokens', cc.read_tokens,
            'input_tokens', cc.input_tokens,
            'cache_creation_tokens', cc.creation_tokens,
            'denominator', cc.read_tokens + cc.input_tokens + cc.creation_tokens,
            'value', CASE WHEN cc.read_tokens + cc.input_tokens + cc.creation_tokens = 0 THEN NULL
                          ELSE round(cc.read_tokens::numeric / (cc.read_tokens + cc.input_tokens + cc.creation_tokens), 6) END,
            'reason', CASE WHEN cc.read_tokens + cc.input_tokens + cc.creation_tokens = 0 THEN 'no_input_tokens' END,
            'disclosure', cc.disclosure)
        FROM cache_cur cc),
    'top_runs', COALESCE((
        SELECT jsonb_agg(jsonb_build_object(
            'run_id', t.run_id, 'kind', t.kind, 'harness', t.harness, 'run_status', t.run_status,
            'usage_status', t.usage_status, 'primary_model', t.primary_model,
            'created_at', to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'cost', t.cost::numeric(24,6)::text,
            'input_tokens', t.input_tokens::bigint, 'cache_read_tokens', t.cache_read_tokens::bigint,
            'cache_creation_tokens', t.cache_creation_tokens::bigint, 'output_tokens', t.output_tokens::bigint,
            'repo_key', t.repo_key, 'repo_id', t.repo_id_out, 'repo_path', t.repo_path_out,
            'user_id', t.user_id, 'user_email', t.email)
            ORDER BY t.cost DESC, t.created_at DESC, t.run_id)
        FROM top_runs t), '[]'::jsonb),
    'breakdown', jsonb_build_object(
        'dimension', @breakdown::text,
        'rows', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', r.slot,
                    'is_other', r.slot = 51,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid)
                || r.j
                ORDER BY r.slot)
            FROM bd_rows r
            LEFT JOIN bd_groups g ON g.rnk = r.slot AND r.slot < 51), '[]'::jsonb))
)::jsonb AS doc;

-- name: AnalyticsOwnerReliability :one
-- Issue #4685: the Reliability document, one jsonb value built over ONE materialized cohort and
-- ONE per-run fold (exactly one row per run, so every count is a plain count(*)). Reuses the
-- Spend statements' window, bucket, rolling-segment, masking and Primary-model rules.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- The caller passes the effective Delivery kinds in @kinds (chat, judge and cross_check never
-- reach this statement). Every range comes from the parameters (half-open); no calendar
-- arithmetic here. Outcome classes come from run_outcome_classes; needs_landing mirrors
-- SelfRunOutcomes (the Go-owned @landable_origins plus an owner-scoped available capture or a
-- preserved patch).
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
rp AS (
    SELECT t.ord::int AS idx, t.s AS r_start, t.e AS r_end
    FROM ROWS FROM (unnest(@roll_starts::timestamptz[]), unnest(@roll_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
bnd AS (
    SELECT COALESCE(array_agg(d.x ORDER BY d.x), '{}'::timestamptz[]) AS arr
    FROM (SELECT r_start AS x FROM rp UNION SELECT r_end FROM rp) d
),
roll_series AS (
    SELECT NULL::text AS hs, 0 AS ord
    UNION ALL
    SELECT t.h, t.ord::int FROM unnest(@roll_harnesses::text[]) WITH ORDINALITY AS t(h, ord)
),
cohort AS MATERIALIZED (
    SELECT r.id, r.user_id, u.email, r.repo_id, rpo.path_with_namespace AS repo_path,
           (@reveal_repos::boolean IS TRUE OR fc.id IS NOT NULL) AS repo_shown,  -- owner-only
           r.kind, r.harness, r.fail_origin, (r.requeue_count > 0) AS requeued, r.created_at,
           CASE WHEN r.status = 'failed' AND r.fail_origin = ANY(@landable_origins::text[])
                THEN (EXISTS (SELECT 1 FROM recovery_captures rc
                              WHERE rc.run_id = r.id AND rc.user_id = r.user_id AND rc.state = 'available')
                      OR r.preserved_patch IS NOT NULL)
                ELSE false END AS needs_landing
    FROM runs r
    JOIN users u ON u.id = r.user_id
    LEFT JOIN repos rpo ON rpo.id = r.repo_id
    LEFT JOIN forge_connections fc ON fc.id = rpo.connection_id AND fc.user_id = @user_id::uuid  -- owner-only
    WHERE r.created_at >= @fold_from::timestamptz AND r.created_at < @now::timestamptz
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
),
run_fold AS MATERIALIZED (
    SELECT c.id AS run_id, c.user_id, c.email, c.repo_id, c.kind, c.harness, c.fail_origin, c.requeued,
           c.needs_landing, c.created_at,
           CASE WHEN c.repo_id IS NULL THEN 'no_repo'
                WHEN c.repo_shown THEN c.repo_id::text
                ELSE 'inaccessible_repo' END AS repo_key,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_id END AS repo_id_out,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_path END AS repo_path_out,
           CASE WHEN c.created_at >= sqlc.arg('from')::timestamptz THEN 'cur'
                WHEN sqlc.narg('previous_from')::timestamptz IS NOT NULL AND c.created_at >= sqlc.narg('previous_from')::timestamptz AND c.created_at < sqlc.arg('from')::timestamptz THEN 'prev'
           END AS win,
           o.outcome,
           pm.primary_model,
           width_bucket(c.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM cohort c
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0
    ) o ON true
    LEFT JOIN LATERAL (
        SELECT
               COALESCE((array_agg(mf.model ORDER BY mf.output_tokens DESC, mf.model COLLATE "C" ASC)
                         FILTER (WHERE mf.model IS NOT NULL))[1], 'unknown') AS primary_model
        FROM run_usage_model_totals mf
        WHERE mf.run_id = c.id AND @breakdown::text = 'primary_model'
          AND c.created_at >= sqlc.arg('from')::timestamptz  -- only current-window rows read primary_model
        OFFSET 0
    ) pm ON true
),
rfb AS MATERIALIZED (
    SELECT rf.*,
           CASE WHEN rf.win = 'cur' AND rf.wb >= 1 AND rf.created_at < (@bucket_ends::timestamptz[])[rf.wb] THEN rf.wb END AS bidx
    FROM run_fold rf
),
bd_items AS MATERIALIZED (
    SELECT x.*,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @breakdown::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur'
),
bd_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gfin DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, count(*) FILTER (WHERE i.outcome IS NOT NULL) OVER (PARTITION BY i.gkey) AS gfin FROM bd_items i) y
),
bd_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM bd_ranked r
    GROUP BY r.gkey
),
fan AS (
    SELECT 'tot_cur'::text AS kind, 0 AS idx, x.outcome, x.requeued FROM rfb x WHERE x.win = 'cur'
    UNION ALL SELECT 'tot_prev', 0, x.outcome, x.requeued FROM rfb x WHERE x.win = 'prev'
    UNION ALL SELECT 'bucket', x.bidx, x.outcome, x.requeued FROM rfb x WHERE x.bidx IS NOT NULL
    UNION ALL SELECT 'bd', LEAST(x.rnk, 51), x.outcome, x.requeued FROM bd_ranked x
    UNION ALL SELECT 'tot_cur', 0, NULL::text, false
    UNION ALL SELECT 'tot_prev', 0, NULL::text, false
    UNION ALL SELECT 'bucket', bk.idx, NULL::text, false FROM bk
),
cnt AS (
    SELECT f.kind, f.idx,
           (CASE WHEN f.kind = 'bd' THEN 5 ELSE 1 END) AS min_n,
           count(*) FILTER (WHERE f.outcome IS NOT NULL) AS fin,
           count(*) FILTER (WHERE f.outcome = 'completed') AS n_completed,
           count(*) FILTER (WHERE f.outcome = 'cancelled') AS n_cancelled,
           count(*) FILTER (WHERE f.outcome = 'plan_rejected') AS n_plan_rejected,
           count(*) FILTER (WHERE f.outcome = 'base_diverged') AS n_base_diverged,
           count(*) FILTER (WHERE f.outcome = 'failed') AS n_failed,
           count(*) FILTER (WHERE f.requeued) AS n_requeued
    FROM fan f
    GROUP BY f.kind, f.idx
),
cnt_j AS (
    SELECT c.kind, c.idx, c.fin, c.n_requeued,
           jsonb_build_object('completed', c.n_completed, 'cancelled', c.n_cancelled, 'plan_rejected', c.n_plan_rejected,
                              'base_diverged', c.n_base_diverged, 'failed', c.n_failed) AS outcomes,
           jsonb_build_object('value', CASE WHEN c.fin < c.min_n THEN NULL ELSE round(c.n_failed::numeric / c.fin, 6) END,
                              'reason', CASE WHEN c.fin >= c.min_n THEN NULL
                                             WHEN c.kind = 'bd' THEN 'insufficient_runs'
                                             ELSE 'no_finished_runs' END) AS rate
    FROM cnt c
),
cause_g AS (
    SELECT COALESCE(x.fail_origin, 'unknown') AS origin, count(*) AS n
    FROM rfb x
    WHERE x.win = 'cur' AND x.outcome = 'failed'
    GROUP BY 1
),
cause_r AS (
    SELECT g.origin, g.n, row_number() OVER (ORDER BY g.n DESC, g.origin COLLATE "C")::int AS rnk
    FROM cause_g g
),
cause_s AS (
    SELECT LEAST(r.rnk, 6) AS slot, sum(r.n)::bigint AS n, (array_agg(r.origin ORDER BY r.rnk) FILTER (WHERE r.rnk <= 5))[1] AS origin
    FROM cause_r r
    GROUP BY LEAST(r.rnk, 6)
),
ev_seg AS (
    SELECT NULL::text AS hs, width_bucket(x.created_at, (SELECT arr FROM bnd)) AS seg,
           count(*) FILTER (WHERE x.outcome IS NOT NULL) AS fin, count(*) FILTER (WHERE x.outcome = 'failed') AS failed
    FROM run_fold x
    GROUP BY width_bucket(x.created_at, (SELECT arr FROM bnd))
    UNION ALL
    SELECT x.harness, width_bucket(x.created_at, (SELECT arr FROM bnd)),
           count(*) FILTER (WHERE x.outcome IS NOT NULL), count(*) FILTER (WHERE x.outcome = 'failed')
    FROM run_fold x
    GROUP BY x.harness, width_bucket(x.created_at, (SELECT arr FROM bnd))
),
roll_pts AS (
    SELECT s.hs, s.ord, rp.idx, COALESCE(sum(g.fin), 0)::bigint AS fin, COALESCE(sum(g.failed), 0)::bigint AS failed
    FROM roll_series s
    CROSS JOIN rp
    LEFT JOIN ev_seg g ON g.hs IS NOT DISTINCT FROM s.hs
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY s.hs, s.ord, rp.idx
),
roll_j AS (
    SELECT p.hs, p.idx,
           jsonb_build_object('index', p.idx - 1, 'finished', p.fin, 'failed', p.failed,
                              'value', CASE WHEN p.fin < 5 THEN NULL ELSE round(p.failed::numeric / p.fin, 6) END,
                              'reason', CASE WHEN p.fin < 5 THEN 'insufficient_runs' END) AS j
    FROM roll_pts p
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'totals', jsonb_build_object('current', (SELECT jsonb_build_object('finished', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued) FROM cnt_j c WHERE c.kind = 'tot_cur'))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT jsonb_build_object('finished', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued) FROM cnt_j c WHERE c.kind = 'tot_prev')) END,
    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'finished', c.fin, 'outcomes', c.outcomes) ORDER BY c.idx) FROM cnt_j c WHERE c.kind = 'bucket'), '[]'::jsonb),
    'causes', jsonb_build_object(
        'failed', (SELECT COALESCE(sum(g.n), 0)::bigint FROM cause_g g),
        'items', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('rank', s.slot, 'is_other', s.slot = 6, 'origin', s.origin, 'runs', s.n,
                                                'share', round(s.n::numeric / (SELECT sum(g.n) FROM cause_g g), 6)) ORDER BY s.slot)
            FROM cause_s s), '[]'::jsonb)),
    'rolling', jsonb_build_object(
        'overall', COALESCE((SELECT jsonb_agg(r.j ORDER BY r.idx) FROM roll_j r WHERE r.hs IS NULL), '[]'::jsonb),
        'harnesses', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('harness', s.hs,
                                                'points', COALESCE((SELECT jsonb_agg(r.j ORDER BY r.idx) FROM roll_j r WHERE r.hs = s.hs), '[]'::jsonb))
                             ORDER BY s.ord)
            FROM roll_series s WHERE s.hs IS NOT NULL), '[]'::jsonb)),
    'breakdown', jsonb_build_object(
        'dimension', @breakdown::text,
        'rows', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', c.idx,
                    'is_other', c.idx = 51,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid,
                    'n', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued)
                ORDER BY c.idx)
            FROM cnt_j c
            LEFT JOIN bd_groups g ON g.rnk = c.idx AND c.idx < 51
            WHERE c.kind = 'bd'), '[]'::jsonb)),
    'needs_landing', (SELECT count(*) FROM rfb x WHERE x.win = 'cur' AND x.needs_landing)
)::jsonb AS doc;

-- name: AnalyticsAllReliability :one
-- Issue #4685: the Reliability document, one jsonb value built over ONE materialized cohort and
-- ONE per-run fold (exactly one row per run, so every count is a plain count(*)). Reuses the
-- Spend statements' window, bucket, rolling-segment, masking and Primary-model rules.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- The caller passes the effective Delivery kinds in @kinds (chat, judge and cross_check never
-- reach this statement). Every range comes from the parameters (half-open); no calendar
-- arithmetic here. Outcome classes come from run_outcome_classes; needs_landing mirrors
-- SelfRunOutcomes (the Go-owned @landable_origins plus an owner-scoped available capture or a
-- preserved patch).
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
rp AS (
    SELECT t.ord::int AS idx, t.s AS r_start, t.e AS r_end
    FROM ROWS FROM (unnest(@roll_starts::timestamptz[]), unnest(@roll_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
bnd AS (
    SELECT COALESCE(array_agg(d.x ORDER BY d.x), '{}'::timestamptz[]) AS arr
    FROM (SELECT r_start AS x FROM rp UNION SELECT r_end FROM rp) d
),
roll_series AS (
    SELECT NULL::text AS hs, 0 AS ord
    UNION ALL
    SELECT t.h, t.ord::int FROM unnest(@roll_harnesses::text[]) WITH ORDINALITY AS t(h, ord)
),
cohort AS MATERIALIZED (
    SELECT r.id, r.user_id, u.email, r.repo_id, rpo.path_with_namespace AS repo_path,
           true AS repo_shown,  -- all-only
           r.kind, r.harness, r.fail_origin, (r.requeue_count > 0) AS requeued, r.created_at,
           CASE WHEN r.status = 'failed' AND r.fail_origin = ANY(@landable_origins::text[])
                THEN (EXISTS (SELECT 1 FROM recovery_captures rc
                              WHERE rc.run_id = r.id AND rc.user_id = r.user_id AND rc.state = 'available')
                      OR r.preserved_patch IS NOT NULL)
                ELSE false END AS needs_landing
    FROM runs r
    JOIN users u ON u.id = r.user_id
    LEFT JOIN repos rpo ON rpo.id = r.repo_id
    WHERE r.created_at >= @fold_from::timestamptz AND r.created_at < @now::timestamptz
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
),
run_fold AS MATERIALIZED (
    SELECT c.id AS run_id, c.user_id, c.email, c.repo_id, c.kind, c.harness, c.fail_origin, c.requeued,
           c.needs_landing, c.created_at,
           CASE WHEN c.repo_id IS NULL THEN 'no_repo'
                WHEN c.repo_shown THEN c.repo_id::text
                ELSE 'inaccessible_repo' END AS repo_key,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_id END AS repo_id_out,
           CASE WHEN c.repo_id IS NOT NULL AND c.repo_shown THEN c.repo_path END AS repo_path_out,
           CASE WHEN c.created_at >= sqlc.arg('from')::timestamptz THEN 'cur'
                WHEN sqlc.narg('previous_from')::timestamptz IS NOT NULL AND c.created_at >= sqlc.narg('previous_from')::timestamptz AND c.created_at < sqlc.arg('from')::timestamptz THEN 'prev'
           END AS win,
           o.outcome,
           pm.primary_model,
           width_bucket(c.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM cohort c
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0
    ) o ON true
    LEFT JOIN LATERAL (
        SELECT
               COALESCE((array_agg(mf.model ORDER BY mf.output_tokens DESC, mf.model COLLATE "C" ASC)
                         FILTER (WHERE mf.model IS NOT NULL))[1], 'unknown') AS primary_model
        FROM run_usage_model_totals mf
        WHERE mf.run_id = c.id AND @breakdown::text = 'primary_model'
          AND c.created_at >= sqlc.arg('from')::timestamptz  -- only current-window rows read primary_model
        OFFSET 0
    ) pm ON true
),
rfb AS MATERIALIZED (
    SELECT rf.*,
           CASE WHEN rf.win = 'cur' AND rf.wb >= 1 AND rf.created_at < (@bucket_ends::timestamptz[])[rf.wb] THEN rf.wb END AS bidx
    FROM run_fold rf
),
bd_items AS MATERIALIZED (
    SELECT x.*,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id::text
                WHEN 'harness' THEN x.harness
                WHEN 'repo' THEN x.repo_key
                WHEN 'kind' THEN x.kind
                WHEN 'primary_model' THEN x.primary_model
            END AS gkey,
           CASE @breakdown::text
                WHEN 'user' THEN x.email
                WHEN 'repo' THEN x.repo_path_out
            END AS glabel,
           CASE @breakdown::text
                WHEN 'user' THEN x.user_id
                WHEN 'repo' THEN x.repo_id_out
            END AS gid
    FROM rfb x
    WHERE x.win = 'cur'
),
bd_ranked AS (
    SELECT y.*, dense_rank() OVER (ORDER BY y.gfin DESC, y.gkey COLLATE "C")::int AS rnk
    FROM (SELECT i.*, count(*) FILTER (WHERE i.outcome IS NOT NULL) OVER (PARTITION BY i.gkey) AS gfin FROM bd_items i) y
),
bd_groups AS MATERIALIZED (
    SELECT r.gkey, min(r.glabel COLLATE "C") AS glabel, (array_agg(r.gid ORDER BY r.gid))[1] AS gid, min(r.rnk) AS rnk
    FROM bd_ranked r
    GROUP BY r.gkey
),
fan AS (
    SELECT 'tot_cur'::text AS kind, 0 AS idx, x.outcome, x.requeued FROM rfb x WHERE x.win = 'cur'
    UNION ALL SELECT 'tot_prev', 0, x.outcome, x.requeued FROM rfb x WHERE x.win = 'prev'
    UNION ALL SELECT 'bucket', x.bidx, x.outcome, x.requeued FROM rfb x WHERE x.bidx IS NOT NULL
    UNION ALL SELECT 'bd', LEAST(x.rnk, 51), x.outcome, x.requeued FROM bd_ranked x
    UNION ALL SELECT 'tot_cur', 0, NULL::text, false
    UNION ALL SELECT 'tot_prev', 0, NULL::text, false
    UNION ALL SELECT 'bucket', bk.idx, NULL::text, false FROM bk
),
cnt AS (
    SELECT f.kind, f.idx,
           (CASE WHEN f.kind = 'bd' THEN 5 ELSE 1 END) AS min_n,
           count(*) FILTER (WHERE f.outcome IS NOT NULL) AS fin,
           count(*) FILTER (WHERE f.outcome = 'completed') AS n_completed,
           count(*) FILTER (WHERE f.outcome = 'cancelled') AS n_cancelled,
           count(*) FILTER (WHERE f.outcome = 'plan_rejected') AS n_plan_rejected,
           count(*) FILTER (WHERE f.outcome = 'base_diverged') AS n_base_diverged,
           count(*) FILTER (WHERE f.outcome = 'failed') AS n_failed,
           count(*) FILTER (WHERE f.requeued) AS n_requeued
    FROM fan f
    GROUP BY f.kind, f.idx
),
cnt_j AS (
    SELECT c.kind, c.idx, c.fin, c.n_requeued,
           jsonb_build_object('completed', c.n_completed, 'cancelled', c.n_cancelled, 'plan_rejected', c.n_plan_rejected,
                              'base_diverged', c.n_base_diverged, 'failed', c.n_failed) AS outcomes,
           jsonb_build_object('value', CASE WHEN c.fin < c.min_n THEN NULL ELSE round(c.n_failed::numeric / c.fin, 6) END,
                              'reason', CASE WHEN c.fin >= c.min_n THEN NULL
                                             WHEN c.kind = 'bd' THEN 'insufficient_runs'
                                             ELSE 'no_finished_runs' END) AS rate
    FROM cnt c
),
cause_g AS (
    SELECT COALESCE(x.fail_origin, 'unknown') AS origin, count(*) AS n
    FROM rfb x
    WHERE x.win = 'cur' AND x.outcome = 'failed'
    GROUP BY 1
),
cause_r AS (
    SELECT g.origin, g.n, row_number() OVER (ORDER BY g.n DESC, g.origin COLLATE "C")::int AS rnk
    FROM cause_g g
),
cause_s AS (
    SELECT LEAST(r.rnk, 6) AS slot, sum(r.n)::bigint AS n, (array_agg(r.origin ORDER BY r.rnk) FILTER (WHERE r.rnk <= 5))[1] AS origin
    FROM cause_r r
    GROUP BY LEAST(r.rnk, 6)
),
ev_seg AS (
    SELECT NULL::text AS hs, width_bucket(x.created_at, (SELECT arr FROM bnd)) AS seg,
           count(*) FILTER (WHERE x.outcome IS NOT NULL) AS fin, count(*) FILTER (WHERE x.outcome = 'failed') AS failed
    FROM run_fold x
    GROUP BY width_bucket(x.created_at, (SELECT arr FROM bnd))
    UNION ALL
    SELECT x.harness, width_bucket(x.created_at, (SELECT arr FROM bnd)),
           count(*) FILTER (WHERE x.outcome IS NOT NULL), count(*) FILTER (WHERE x.outcome = 'failed')
    FROM run_fold x
    GROUP BY x.harness, width_bucket(x.created_at, (SELECT arr FROM bnd))
),
roll_pts AS (
    SELECT s.hs, s.ord, rp.idx, COALESCE(sum(g.fin), 0)::bigint AS fin, COALESCE(sum(g.failed), 0)::bigint AS failed
    FROM roll_series s
    CROSS JOIN rp
    LEFT JOIN ev_seg g ON g.hs IS NOT DISTINCT FROM s.hs
        AND (SELECT arr[g.seg] FROM bnd) >= rp.r_start
        AND (SELECT arr[g.seg] FROM bnd) < rp.r_end
    GROUP BY s.hs, s.ord, rp.idx
),
roll_j AS (
    SELECT p.hs, p.idx,
           jsonb_build_object('index', p.idx - 1, 'finished', p.fin, 'failed', p.failed,
                              'value', CASE WHEN p.fin < 5 THEN NULL ELSE round(p.failed::numeric / p.fin, 6) END,
                              'reason', CASE WHEN p.fin < 5 THEN 'insufficient_runs' END) AS j
    FROM roll_pts p
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'totals', jsonb_build_object('current', (SELECT jsonb_build_object('finished', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued) FROM cnt_j c WHERE c.kind = 'tot_cur'))
        || CASE WHEN sqlc.narg('previous_from')::timestamptz IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('previous', (SELECT jsonb_build_object('finished', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued) FROM cnt_j c WHERE c.kind = 'tot_prev')) END,
    'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'finished', c.fin, 'outcomes', c.outcomes) ORDER BY c.idx) FROM cnt_j c WHERE c.kind = 'bucket'), '[]'::jsonb),
    'causes', jsonb_build_object(
        'failed', (SELECT COALESCE(sum(g.n), 0)::bigint FROM cause_g g),
        'items', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('rank', s.slot, 'is_other', s.slot = 6, 'origin', s.origin, 'runs', s.n,
                                                'share', round(s.n::numeric / (SELECT sum(g.n) FROM cause_g g), 6)) ORDER BY s.slot)
            FROM cause_s s), '[]'::jsonb)),
    'rolling', jsonb_build_object(
        'overall', COALESCE((SELECT jsonb_agg(r.j ORDER BY r.idx) FROM roll_j r WHERE r.hs IS NULL), '[]'::jsonb),
        'harnesses', COALESCE((
            SELECT jsonb_agg(jsonb_build_object('harness', s.hs,
                                                'points', COALESCE((SELECT jsonb_agg(r.j ORDER BY r.idx) FROM roll_j r WHERE r.hs = s.hs), '[]'::jsonb))
                             ORDER BY s.ord)
            FROM roll_series s WHERE s.hs IS NOT NULL), '[]'::jsonb)),
    'breakdown', jsonb_build_object(
        'dimension', @breakdown::text,
        'rows', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'rank', c.idx,
                    'is_other', c.idx = 51,
                    'key', g.gkey, 'label', g.glabel, 'id', g.gid,
                    'n', c.fin, 'outcomes', c.outcomes, 'failure_rate', c.rate, 'requeued_runs', c.n_requeued)
                ORDER BY c.idx)
            FROM cnt_j c
            LEFT JOIN bd_groups g ON g.rnk = c.idx AND c.idx < 51
            WHERE c.kind = 'bd'), '[]'::jsonb)),
    'needs_landing', (SELECT count(*) FROM rfb x WHERE x.win = 'cur' AND x.needs_landing)
)::jsonb AS doc;

-- name: AnalyticsOwnerLatestFailure :one
-- Issue #4685: the lifetime latest failure of the scope (no created_at cutoff, no window): the
-- SelfRunOutcomes last_failure predicate verbatim, restricted by the request's repo, harness
-- and Delivery kinds. completed_since_last_failure counts completed outcome-kind runs of the
-- same scope strictly after the failure's terminal timestamp. found is false when there is no
-- failure (the other columns then carry zero values). The owner and all-scope statements are
-- textually identical except the lines tagged owner-only.
WITH last_failure AS MATERIALIZED (
    SELECT id, user_id, COALESCE(finished_at, status_since) AS ended_at,
           COALESCE(fail_origin, 'unknown') AS origin
    FROM runs
    WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND fail_origin IS DISTINCT FROM 'base_diverged'
      AND fn_outcome_kind(kind)
      AND user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR harness = sqlc.narg('harness')::text)
      AND kind = ANY(@kinds::text[])
    ORDER BY COALESCE(finished_at, status_since) DESC, id DESC LIMIT 1
)
SELECT
    EXISTS (SELECT 1 FROM last_failure)::boolean AS found,
    (SELECT f.ended_at FROM last_failure f)::timestamptz AS failed_at,
    COALESCE((SELECT f.origin FROM last_failure f), '')::text AS origin,
    COALESCE((SELECT f.id FROM last_failure f), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS run_id,
    COALESCE((SELECT f.user_id FROM last_failure f), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS failed_user_id,
    COALESCE((SELECT count(*)::bigint
       FROM last_failure f JOIN runs r3 ON COALESCE(r3.finished_at, r3.status_since) > f.ended_at
       WHERE r3.status = 'completed' AND fn_outcome_kind(r3.kind)
         AND r3.user_id = @user_id::uuid  -- owner-only
         AND (sqlc.narg('repo_id')::uuid IS NULL OR r3.repo_id = sqlc.narg('repo_id')::uuid)
         AND (sqlc.narg('harness')::text IS NULL OR r3.harness = sqlc.narg('harness')::text)
         AND r3.kind = ANY(@kinds::text[])), 0)::bigint AS completed_since_last_failure;

-- name: AnalyticsAllLatestFailure :one
-- Issue #4685: the lifetime latest failure of the scope (no created_at cutoff, no window): the
-- SelfRunOutcomes last_failure predicate verbatim, restricted by the request's repo, harness
-- and Delivery kinds. completed_since_last_failure counts completed outcome-kind runs of the
-- same scope strictly after the failure's terminal timestamp. found is false when there is no
-- failure (the other columns then carry zero values). The owner and all-scope statements are
-- textually identical except the lines tagged owner-only.
WITH last_failure AS MATERIALIZED (
    SELECT id, user_id, COALESCE(finished_at, status_since) AS ended_at,
           COALESCE(fail_origin, 'unknown') AS origin
    FROM runs
    WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND fail_origin IS DISTINCT FROM 'base_diverged'
      AND fn_outcome_kind(kind)
      AND (sqlc.narg('repo_id')::uuid IS NULL OR repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR harness = sqlc.narg('harness')::text)
      AND kind = ANY(@kinds::text[])
    ORDER BY COALESCE(finished_at, status_since) DESC, id DESC LIMIT 1
)
SELECT
    EXISTS (SELECT 1 FROM last_failure)::boolean AS found,
    (SELECT f.ended_at FROM last_failure f)::timestamptz AS failed_at,
    COALESCE((SELECT f.origin FROM last_failure f), '')::text AS origin,
    COALESCE((SELECT f.id FROM last_failure f), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS run_id,
    COALESCE((SELECT f.user_id FROM last_failure f), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS failed_user_id,
    COALESCE((SELECT count(*)::bigint
       FROM last_failure f JOIN runs r3 ON COALESCE(r3.finished_at, r3.status_since) > f.ended_at
       WHERE r3.status = 'completed' AND fn_outcome_kind(r3.kind)
         AND (sqlc.narg('repo_id')::uuid IS NULL OR r3.repo_id = sqlc.narg('repo_id')::uuid)
         AND (sqlc.narg('harness')::text IS NULL OR r3.harness = sqlc.narg('harness')::text)
         AND r3.kind = ANY(@kinds::text[])), 0)::bigint AS completed_since_last_failure;

-- name: AnalyticsOwnerFlow :one
-- Issue #4729: the Flow document, one jsonb value built over ONE materialized scoped set.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- Every range comes from the parameters (half-open: start <= ts < end); there is no calendar
-- arithmetic or clock read here. Timestamps are UTC strings so the document does not depend on
-- the session TimeZone. The document names no repo id or path, so repo masking is vacuous.
-- Populations (each measure names its own): waiting = current status; created = created_at in
-- [from, now); finished = COALESCE(finished_at, status_since) in [from, now), independent of
-- created_at; every other block is the created_at cohort.
-- Approval evidence is one receipt per run_user_inputs row fetched through a per-run lookup on
-- idx_run_user_inputs_run_id; it never scans run_user_inputs globally and never reads the run message log.
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
scoped AS MATERIALIZED (
    SELECT r.id, r.kind, r.status, r.created_at, r.finished_at, r.status_since, r.first_started_at,
           r.auto_approve, (r.plan_md IS NOT NULL) AS has_plan, r.plan_source, r.gate_revision,
           r.repo_id, r.mr_iid,
           (r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz) AS in_cohort
    FROM runs r
    WHERE r.kind = ANY(@kinds::text[])
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND ((r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz)
           OR r.status IN ('queued', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused', 'limit_wait', 'pool_wait', 'recovery_wait')
           OR (r.status IN ('completed', 'failed', 'cancelled')
               AND COALESCE(r.finished_at, r.status_since) >= sqlc.arg('from')::timestamptz
               AND COALESCE(r.finished_at, r.status_since) < @now::timestamptz))
),
cc AS MATERIALIZED (
    SELECT c.*,
           CASE WHEN c.wb >= 1 AND c.created_at < (@bucket_ends::timestamptz[])[c.wb] THEN c.wb END AS bidx
    FROM (SELECT s.*, width_bucket(s.created_at, @bucket_starts::timestamptz[]) AS wb FROM scoped s WHERE s.in_cohort) c
),
fin AS MATERIALIZED (
    SELECT c.id, o.outcome,
           CASE WHEN c.wb >= 1 AND c.end_at < (@bucket_ends::timestamptz[])[c.wb] THEN c.wb END AS bidx
    FROM (SELECT s.id, COALESCE(s.finished_at, s.status_since) AS end_at,
                 width_bucket(COALESCE(s.finished_at, s.status_since), @bucket_starts::timestamptz[]) AS wb
          FROM scoped s
          WHERE s.status IN ('completed', 'failed', 'cancelled')
            AND COALESCE(s.finished_at, s.status_since) >= sqlc.arg('from')::timestamptz
            AND COALESCE(s.finished_at, s.status_since) < @now::timestamptz) c
    JOIN LATERAL (SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0) o ON true
    WHERE o.outcome IS NOT NULL
),
merged_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
      AND w.user_id = @user_id::uuid  -- owner-only
),
wstates AS (
    SELECT t.s AS state, t.ord::int AS ord
    FROM unnest(ARRAY['queued', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused', 'limit_wait', 'pool_wait', 'recovery_wait']::text[]) WITH ORDINALITY AS t(s, ord)
),
waiting AS (
    SELECT w.ord, w.state, count(c.id) AS n, min(c.status_since) AS oldest
    FROM wstates w
    LEFT JOIN scoped c ON c.status = w.state
    GROUP BY w.ord, w.state
),
created_cells AS (
    SELECT bk.idx, count(c.id) AS n
    FROM bk LEFT JOIN cc c ON c.bidx = bk.idx
    GROUP BY bk.idx
),
fin_cells AS (
    SELECT bk.idx,
           count(f.id) FILTER (WHERE f.outcome = 'completed') AS completed,
           count(f.id) FILTER (WHERE f.outcome = 'cancelled') AS cancelled,
           count(f.id) FILTER (WHERE f.outcome = 'plan_rejected') AS plan_rejected,
           count(f.id) FILTER (WHERE f.outcome = 'base_diverged') AS base_diverged,
           count(f.id) FILTER (WHERE f.outcome = 'failed') AS failed,
           count(f.id) AS total
    FROM bk LEFT JOIN fin f ON f.bidx = bk.idx
    GROUP BY bk.idx
),
fin_total AS (
    SELECT jsonb_build_object(
               'completed', count(*) FILTER (WHERE f.outcome = 'completed'),
               'cancelled', count(*) FILTER (WHERE f.outcome = 'cancelled'),
               'plan_rejected', count(*) FILTER (WHERE f.outcome = 'plan_rejected'),
               'base_diverged', count(*) FILTER (WHERE f.outcome = 'base_diverged'),
               'failed', count(*) FILTER (WHERE f.outcome = 'failed'),
               'total', count(*)) AS j
    FROM fin f
),
start_delay AS (
    SELECT count(*) FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS observed,
           count(*) FILTER (WHERE c.first_started_at IS NULL) AS not_started,
           count(*) FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at < c.created_at) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (c.first_started_at - c.created_at)))
               FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (c.first_started_at - c.created_at)))
               FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS p90
    FROM cc c
),
gate_runs AS MATERIALIZED (
    -- Plan response covers every scoped kind, chat included when selected; only the funnel and
    -- approval cohorts (fr) restrict to issue and self_improve.
    SELECT c.id, c.gate_revision FROM cc c
),
rcpt AS MATERIALIZED (
    SELECT g.id AS run_id, g.gate_revision AS run_rev, i.gate_binding, i.gate_revision AS rev,
           i.created_at AS in_at, i.applied_at, i.disposition
    FROM gate_runs g
    JOIN LATERAL (
        SELECT x.gate_binding, x.gate_revision, x.created_at, x.applied_at, x.disposition
        FROM run_user_inputs x
        WHERE x.run_id = g.id AND x.kind = 'approve_plan'
        OFFSET 0
    ) i ON true
),
rcpt_c AS MATERIALIZED (
    SELECT r.*,
           COALESCE(r.gate_binding = 'bound' AND r.rev > 0 AND r.applied_at IS NOT NULL, false) AS applied_bound,
           (r.disposition IS NULL OR r.disposition = 'applied') AS disp_ok,
           COALESCE(r.in_at <= r.applied_at, false) AS chron_ok
    FROM rcpt r
),
rcpt_pair AS (
    SELECT x.run_id, x.rev,
           count(*) FILTER (WHERE x.disp_ok AND x.chron_ok) AS n_elig,
           count(*) FILTER (WHERE x.disp_ok AND x.chron_ok AND x.disposition = 'applied') AS n_elig_applied,
           count(*) FILTER (WHERE x.disp_ok AND NOT x.chron_ok) AS n_chron,
           count(*) FILTER (WHERE NOT x.disp_ok) AS n_excl,
           min(x.in_at) FILTER (WHERE x.disp_ok AND x.chron_ok) AS in_at
    FROM rcpt_c x
    WHERE x.applied_bound
    GROUP BY x.run_id, x.rev
),
pres AS MATERIALIZED (
    SELECT g.id AS run_id, p.revision AS rev, p.created_at AS pres_at
    FROM gate_runs g
    JOIN LATERAL (
        SELECT x.revision, x.created_at
        FROM run_gate_presentations x
        WHERE x.run_id = g.id
        OFFSET 0
    ) p ON true
),
pair_class AS (
    SELECT q.*,
           CASE WHEN q.n_elig > 1 THEN 'ambiguous'
                WHEN q.n_elig = 1 AND q.n_elig_applied = 1 THEN 'reconciliation_only'
                WHEN q.n_elig = 1 AND q.pres_at IS NULL THEN 'no_presentation'
                WHEN q.n_elig = 1 AND q.pres_at > q.in_at THEN 'invalid_chronology'
                WHEN q.n_elig = 1 THEN 'timed'
                WHEN q.n_chron > 0 THEN 'invalid_chronology'
                WHEN q.n_excl > 0 THEN 'excluded_disposition'
                ELSE 'not_approved' END AS cls
    FROM (SELECT COALESCE(p.run_id, a.run_id) AS run_id, COALESCE(p.rev, a.rev) AS rev, p.pres_at,
                 COALESCE(a.n_elig, 0) AS n_elig, COALESCE(a.n_elig_applied, 0) AS n_elig_applied,
                 COALESCE(a.n_chron, 0) AS n_chron, COALESCE(a.n_excl, 0) AS n_excl, a.in_at
          FROM pres p
          FULL OUTER JOIN rcpt_pair a ON a.run_id = p.run_id AND a.rev = p.rev) q
),
plan_resp AS (
    SELECT count(*) FILTER (WHERE pc.cls = 'timed') AS n,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (pc.in_at - pc.pres_at))) FILTER (WHERE pc.cls = 'timed') AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (pc.in_at - pc.pres_at))) FILTER (WHERE pc.cls = 'timed') AS p90,
           count(*) FILTER (WHERE pc.cls = 'ambiguous') AS ambiguous,
           count(*) FILTER (WHERE pc.cls = 'no_presentation') AS no_presentation,
           count(*) FILTER (WHERE pc.cls = 'invalid_chronology') AS invalid_chronology,
           count(*) FILTER (WHERE pc.cls = 'excluded_disposition') AS excluded_disposition,
           count(*) FILTER (WHERE pc.cls = 'reconciliation_only') AS reconciliation_only,
           count(*) FILTER (WHERE pc.cls = 'not_approved') AS not_approved
    FROM pair_class pc
),
plan_legacy AS (
    SELECT count(*) AS n FROM (SELECT x.run_id FROM rcpt_c x
                               GROUP BY x.run_id
                               HAVING bool_or(x.gate_binding IS NULL OR x.gate_binding = 'unbound')) l
),
fr AS MATERIALIZED (
    SELECT c.id, c.kind, c.status, c.created_at, c.auto_approve, c.has_plan, c.plan_source, c.gate_revision,
           c.repo_id, c.mr_iid, c.bidx, o.outcome,
           COALESCE(e.cur_n, 0) AS cur_n, COALESCE(e.hist_ok, 0) AS hist_ok, COALESCE(e.hist_amb, 0) AS hist_amb,
           COALESCE(e.legacy, false) AS legacy, COALESCE(e.any_receipt, 0) AS any_receipt,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL) AS pr_present,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL
                AND (c.repo_id, c.mr_iid) IN (SELECT mw.repo_id, mw.mr_iid FROM merged_witness mw)) AS witnessed
    FROM cc c
    LEFT JOIN LATERAL (SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0) o ON true
    -- Per-run receipt aggregate as a probe of idx_run_user_inputs_run_id: a join against a cohort-wide
    -- GROUP BY CTE is re-read once per run under a generic plan.
    -- Eligible receipts are counted per revision (g.n): an older revision with exactly one is historical
    -- approval (hist_ok), one with more than one proves nothing (hist_amb), and one with none is in neither.
    LEFT JOIN LATERAL (
        SELECT COALESCE(max(g.n) FILTER (WHERE g.rev = c.gate_revision), 0) AS cur_n,
               count(*) FILTER (WHERE g.rev < c.gate_revision AND g.n = 1) AS hist_ok,
               count(*) FILTER (WHERE g.rev < c.gate_revision AND g.n > 1) AS hist_amb,
               bool_or(g.legacy) AS legacy,
               COALESCE(sum(g.total), 0)::bigint AS any_receipt
        FROM (SELECT y.rev, count(*) FILTER (WHERE y.ab AND y.dok AND y.cok) AS n,
                     bool_or(y.gate_binding IS NULL OR y.gate_binding = 'unbound') AS legacy,
                     count(*) AS total
              FROM (SELECT x.gate_binding, x.gate_revision AS rev,
                           COALESCE(x.gate_binding = 'bound' AND x.gate_revision > 0 AND x.applied_at IS NOT NULL, false) AS ab,
                           (x.disposition IS NULL OR x.disposition = 'applied') AS dok,
                           COALESCE(x.created_at <= x.applied_at, false) AS cok
                    FROM run_user_inputs x WHERE x.run_id = c.id AND x.kind = 'approve_plan' OFFSET 0) y
              GROUP BY y.rev) g
    ) e ON true
    WHERE c.kind IN ('issue', 'self_improve')
),
fr_class AS (
    SELECT f.*,
           CASE WHEN f.outcome = 'plan_rejected' THEN 'rejected'
                WHEN f.cur_n = 1 THEN 'settled_human'
                WHEN f.auto_approve AND f.gate_revision = 0 AND f.has_plan AND f.plan_source <> 'seeded' THEN 'settled_auto'
                WHEN f.plan_source = 'seeded' AND f.gate_revision = 0 THEN 'skipped'
                WHEN f.status NOT IN ('completed', 'failed', 'cancelled') THEN 'in_progress'
                WHEN f.status = 'failed' THEN 'failed'
                WHEN f.status = 'cancelled' THEN 'cancelled'
                ELSE 'unknown' END AS planning,
           (f.gate_revision > 0 OR f.auto_approve OR f.any_receipt > 0) AS in_plan_cohort,
           CASE WHEN f.cur_n = 1 THEN 'human'
                WHEN f.auto_approve THEN 'auto_approve_configured'
                WHEN f.cur_n > 1 OR (f.hist_amb > 0 AND f.hist_ok = 0) THEN 'ambiguous'
                WHEN f.legacy THEN 'legacy_unbound'
                WHEN f.hist_ok > 0 THEN 'historical_approval_only'
                ELSE 'no_evidence' END AS approval
    FROM fr f
),
pr_units AS (
    SELECT DISTINCT ON (f.repo_id, f.mr_iid) f.repo_id, f.mr_iid, f.bidx, f.witnessed
    FROM fr f
    WHERE f.pr_present
    ORDER BY f.repo_id, f.mr_iid, f.created_at, f.id
),
pr_cells AS (
    SELECT bk.idx, count(u.mr_iid) AS present, count(u.mr_iid) FILTER (WHERE u.witnessed) AS merged
    FROM bk LEFT JOIN pr_units u ON u.bidx = bk.idx
    GROUP BY bk.idx
),
funnel_applicable AS (
    SELECT EXISTS (SELECT 1 FROM unnest(@kinds::text[]) k WHERE k IN ('issue', 'self_improve')) AS ok
),
funnel AS (
    SELECT count(*) AS created,
           count(*) FILTER (WHERE f.planning IN ('settled_human', 'settled_auto')) AS plan_settled,
           count(*) FILTER (WHERE f.outcome = 'completed') AS completed,
           count(*) FILTER (WHERE f.pr_present) AS pr_present,
           count(*) FILTER (WHERE f.witnessed) AS merged,
           count(*) FILTER (WHERE f.planning = 'settled_human') AS p_human,
           count(*) FILTER (WHERE f.planning = 'settled_auto') AS p_auto,
           count(*) FILTER (WHERE f.planning = 'skipped') AS p_skipped,
           count(*) FILTER (WHERE f.planning = 'rejected') AS p_rejected,
           count(*) FILTER (WHERE f.planning = 'in_progress') AS p_in_progress,
           count(*) FILTER (WHERE f.planning = 'failed') AS p_failed,
           count(*) FILTER (WHERE f.planning = 'cancelled') AS p_cancelled,
           count(*) FILTER (WHERE f.planning = 'unknown') AS p_unknown,
           count(*) FILTER (WHERE f.outcome = 'failed') AS o_failed,
           count(*) FILTER (WHERE f.outcome = 'cancelled') AS o_cancelled,
           count(*) FILTER (WHERE f.outcome = 'plan_rejected') AS o_plan_rejected,
           count(*) FILTER (WHERE f.outcome = 'base_diverged') AS o_base_diverged,
           count(*) FILTER (WHERE f.outcome IS NULL) AS o_in_progress,
           count(*) FILTER (WHERE f.cur_n = 0 AND f.hist_ok > 0) AS d_historical,
           count(*) FILTER (WHERE f.cur_n > 1) AS d_ambiguous,
           count(*) FILTER (WHERE f.legacy) AS d_legacy,
           count(*) FILTER (WHERE f.in_plan_cohort) AS a_total,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'human') AS a_human,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'auto_approve_configured') AS a_auto,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval NOT IN ('human', 'auto_approve_configured')) AS a_unknown,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'ambiguous') AS a_ambiguous,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'legacy_unbound') AS a_legacy,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'historical_approval_only') AS a_historical,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'no_evidence') AS a_none
    FROM fr_class f
),
pr_unit_totals AS (
    SELECT count(*) AS present, count(*) FILTER (WHERE u.witnessed) AS merged FROM pr_units u
),
el AS MATERIALIZED (
    SELECT t.id, t.bidx, t.terminal,
           CASE WHEN t.terminal AND t.first_started_at IS NOT NULL
                     AND COALESCE(t.finished_at, t.status_since) >= t.first_started_at
                THEN extract(epoch FROM (COALESCE(t.finished_at, t.status_since) - t.first_started_at))::float8 END AS dur
    FROM (SELECT c.id, c.bidx, c.first_started_at, c.finished_at, c.status_since,
                 (c.status IN ('completed', 'failed', 'cancelled')) AS terminal
          FROM cc c WHERE c.kind = 'issue') t
),
el_cells AS (
    SELECT bk.idx,
           count(e.dur) AS n,
           count(e.id) FILTER (WHERE NOT e.terminal) AS unfinished,
           count(e.id) FILTER (WHERE e.terminal AND e.dur IS NULL) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY e.dur) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY e.dur) AS p90
    FROM bk LEFT JOIN el e ON e.bidx = bk.idx
    GROUP BY bk.idx
),
el_total AS (
    SELECT count(e.dur) AS n,
           count(e.id) FILTER (WHERE NOT e.terminal) AS unfinished,
           count(e.id) FILTER (WHERE e.terminal AND e.dur IS NULL) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY e.dur) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY e.dur) AS p90
    FROM el e
),
hist_edges AS (
    SELECT t.e AS lower_s, t.ord::int AS ord, lead(t.e) OVER (ORDER BY t.ord) AS upper_s
    FROM unnest(@histogram_edges::float8[]) WITH ORDINALITY AS t(e, ord)
),
hist AS (
    SELECT h.ord, h.lower_s, h.upper_s, count(e.dur) AS n
    FROM hist_edges h
    LEFT JOIN el e ON e.dur IS NOT NULL AND GREATEST(width_bucket(e.dur, @histogram_edges::float8[]), 1) = h.ord
    GROUP BY h.ord, h.lower_s, h.upper_s
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'waiting_now', jsonb_build_object(
        'time_basis', 'now',
        'population', 'current_runs',
        'states', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'state', w.state,
                'count', w.n,
                'oldest_status_since', to_char(w.oldest AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                'oldest_age_seconds', CASE WHEN w.oldest IS NULL THEN NULL
                                           ELSE GREATEST(extract(epoch FROM ((SELECT p.now_ts FROM prm p) - w.oldest)), 0)::float8 END)
                ORDER BY w.ord)
            FROM waiting w), '[]'::jsonb),
        'total', (SELECT COALESCE(sum(w.n), 0)::bigint FROM waiting w)),
    'activity', jsonb_build_object(
        'created', jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window',
            'total', (SELECT count(*) FROM cc),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'count', c.n) ORDER BY c.idx) FROM created_cells c), '[]'::jsonb)),
        'finished', jsonb_build_object(
            'time_basis', 'finished_at',
            'population', 'finished_in_window',
            'excluded_kinds', jsonb_build_array('chat'),
            'total', (SELECT j FROM fin_total),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                    'index', c.idx - 1, 'completed', c.completed, 'cancelled', c.cancelled,
                    'plan_rejected', c.plan_rejected, 'base_diverged', c.base_diverged,
                    'failed', c.failed, 'total', c.total) ORDER BY c.idx) FROM fin_cells c), '[]'::jsonb))),
    'first_start_delay', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window',
            'approximate', true,
            'limitations', jsonb_build_array('first_started_at_legacy_backfill'),
            'observed', d.observed,
            'not_started', d.not_started,
            'invalid', d.invalid,
            'p50_seconds', jsonb_build_object('value', d.p50, 'reason', CASE WHEN d.observed = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', d.p90, 'reason', CASE WHEN d.observed = 0 THEN 'no_observations' END))
        FROM start_delay d),
    'plan_response', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_gate_pairs',
            'approximate', true,
            'n', p.n,
            'p50_seconds', jsonb_build_object('value', p.p50, 'reason', CASE WHEN p.n = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', p.p90, 'reason', CASE WHEN p.n = 0 THEN 'no_observations' END),
            'unknown', jsonb_build_object(
                'total', p.ambiguous + p.no_presentation + p.invalid_chronology + p.excluded_disposition + p.reconciliation_only + (SELECT l.n FROM plan_legacy l),
                'reasons', jsonb_build_object(
                    'ambiguous', p.ambiguous,
                    'no_presentation', p.no_presentation,
                    'invalid_chronology', p.invalid_chronology,
                    'excluded_disposition', p.excluded_disposition,
                    'reconciliation_only', p.reconciliation_only,
                    'legacy_unbound', (SELECT l.n FROM plan_legacy l))),
            'not_approved', p.not_approved)
        FROM plan_resp p),
    'delivery_funnel', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_delivery_kinds',
            'applicable', a.ok,
            'reason', CASE WHEN a.ok THEN NULL ELSE 'not_applicable' END,
            'stages', jsonb_build_object('created', f.created, 'plan_settled', f.plan_settled, 'completed', f.completed,
                                         'pr_present', f.pr_present, 'merged', f.merged),
            'planning', jsonb_build_object('settled_human', f.p_human, 'settled_auto', f.p_auto, 'skipped', f.p_skipped,
                                           'rejected', f.p_rejected, 'in_progress', f.p_in_progress, 'failed', f.p_failed,
                                           'cancelled', f.p_cancelled, 'unknown', f.p_unknown),
            'outcomes', jsonb_build_object('completed', f.completed, 'failed', f.o_failed, 'cancelled', f.o_cancelled,
                                           'plan_rejected', f.o_plan_rejected, 'base_diverged', f.o_base_diverged,
                                           'in_progress', f.o_in_progress),
            'disclosure', jsonb_build_object('historical_approval_only', f.d_historical, 'ambiguous_current', f.d_ambiguous,
                                             'legacy_evidence', f.d_legacy),
            'pr_units', jsonb_build_object(
                'present', (SELECT t.present FROM pr_unit_totals t),
                'merged', (SELECT t.merged FROM pr_unit_totals t),
                'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'present', c.present, 'merged', c.merged) ORDER BY c.idx) FROM pr_cells c), '[]'::jsonb)))
        FROM funnel f, funnel_applicable a),
    'approval', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_plan_cohort',
            'applicable', a.ok,
            'reason', CASE WHEN a.ok THEN NULL ELSE 'not_applicable' END,
            'total', f.a_total,
            'human', f.a_human,
            'auto_approve_configured', f.a_auto,
            'unknown', jsonb_build_object(
                'total', f.a_unknown,
                'reasons', jsonb_build_object('ambiguous', f.a_ambiguous, 'legacy_unbound', f.a_legacy,
                                              'historical_approval_only', f.a_historical, 'no_evidence', f.a_none)))
        FROM funnel f, funnel_applicable a),
    'issue_elapsed', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_issue_runs',
            'applicable', (SELECT 'issue' = ANY(@kinds::text[])),
            'reason', CASE WHEN 'issue' = ANY(@kinds::text[]) THEN NULL ELSE 'not_applicable' END,
            'n', t.n,
            'unfinished', t.unfinished,
            'invalid', t.invalid,
            'incomplete', t.unfinished > 0,
            'p50_seconds', jsonb_build_object('value', t.p50, 'reason', CASE WHEN t.n = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', t.p90, 'reason', CASE WHEN t.n = 0 THEN 'no_observations' END),
            'histogram', COALESCE((SELECT jsonb_agg(jsonb_build_object('lower_seconds', h.lower_s, 'upper_seconds', h.upper_s, 'count', h.n) ORDER BY h.ord) FROM hist h), '[]'::jsonb),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                    'index', c.idx - 1, 'n', c.n, 'unfinished', c.unfinished, 'invalid', c.invalid,
                    'incomplete', c.unfinished > 0,
                    'p50_seconds', jsonb_build_object(
                        'value', CASE WHEN c.n >= @min_percentile_n::int THEN c.p50 END,
                        'reason', CASE WHEN c.n = 0 THEN 'no_observations' WHEN c.n < @min_percentile_n::int THEN 'insufficient_observations' END),
                    'p90_seconds', jsonb_build_object(
                        'value', CASE WHEN c.n >= @min_percentile_n::int THEN c.p90 END,
                        'reason', CASE WHEN c.n = 0 THEN 'no_observations' WHEN c.n < @min_percentile_n::int THEN 'insufficient_observations' END))
                    ORDER BY c.idx) FROM el_cells c), '[]'::jsonb))
        FROM el_total t)
)::jsonb AS doc;

-- name: AnalyticsAllFlow :one
-- Issue #4729: the Flow document, one jsonb value built over ONE materialized scoped set.
-- The owner and all-scope statements are kept textually identical except the scope lines, each
-- tagged with a trailing marker comment (a Go test drops them and fails on any other drift).
-- Every range comes from the parameters (half-open: start <= ts < end); there is no calendar
-- arithmetic or clock read here. Timestamps are UTC strings so the document does not depend on
-- the session TimeZone. The document names no repo id or path, so repo masking is vacuous.
-- Populations (each measure names its own): waiting = current status; created = created_at in
-- [from, now); finished = COALESCE(finished_at, status_since) in [from, now), independent of
-- created_at; every other block is the created_at cohort.
-- Approval evidence is one receipt per run_user_inputs row fetched through a per-run lookup on
-- idx_run_user_inputs_run_id; it never scans run_user_inputs globally and never reads the run message log.
WITH
prm AS (SELECT @now::timestamptz AS now_ts),
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
scoped AS MATERIALIZED (
    SELECT r.id, r.kind, r.status, r.created_at, r.finished_at, r.status_since, r.first_started_at,
           r.auto_approve, (r.plan_md IS NOT NULL) AS has_plan, r.plan_source, r.gate_revision,
           r.repo_id, r.mr_iid,
           (r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz) AS in_cohort
    FROM runs r
    WHERE r.kind = ANY(@kinds::text[])
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND ((r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz)
           OR r.status IN ('queued', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused', 'limit_wait', 'pool_wait', 'recovery_wait')
           OR (r.status IN ('completed', 'failed', 'cancelled')
               AND COALESCE(r.finished_at, r.status_since) >= sqlc.arg('from')::timestamptz
               AND COALESCE(r.finished_at, r.status_since) < @now::timestamptz))
),
cc AS MATERIALIZED (
    SELECT c.*,
           CASE WHEN c.wb >= 1 AND c.created_at < (@bucket_ends::timestamptz[])[c.wb] THEN c.wb END AS bidx
    FROM (SELECT s.*, width_bucket(s.created_at, @bucket_starts::timestamptz[]) AS wb FROM scoped s WHERE s.in_cohort) c
),
fin AS MATERIALIZED (
    SELECT c.id, o.outcome,
           CASE WHEN c.wb >= 1 AND c.end_at < (@bucket_ends::timestamptz[])[c.wb] THEN c.wb END AS bidx
    FROM (SELECT s.id, COALESCE(s.finished_at, s.status_since) AS end_at,
                 width_bucket(COALESCE(s.finished_at, s.status_since), @bucket_starts::timestamptz[]) AS wb
          FROM scoped s
          WHERE s.status IN ('completed', 'failed', 'cancelled')
            AND COALESCE(s.finished_at, s.status_since) >= sqlc.arg('from')::timestamptz
            AND COALESCE(s.finished_at, s.status_since) < @now::timestamptz) c
    JOIN LATERAL (SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0) o ON true
    WHERE o.outcome IS NOT NULL
),
merged_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
),
wstates AS (
    SELECT t.s AS state, t.ord::int AS ord
    FROM unnest(ARRAY['queued', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused', 'limit_wait', 'pool_wait', 'recovery_wait']::text[]) WITH ORDINALITY AS t(s, ord)
),
waiting AS (
    SELECT w.ord, w.state, count(c.id) AS n, min(c.status_since) AS oldest
    FROM wstates w
    LEFT JOIN scoped c ON c.status = w.state
    GROUP BY w.ord, w.state
),
created_cells AS (
    SELECT bk.idx, count(c.id) AS n
    FROM bk LEFT JOIN cc c ON c.bidx = bk.idx
    GROUP BY bk.idx
),
fin_cells AS (
    SELECT bk.idx,
           count(f.id) FILTER (WHERE f.outcome = 'completed') AS completed,
           count(f.id) FILTER (WHERE f.outcome = 'cancelled') AS cancelled,
           count(f.id) FILTER (WHERE f.outcome = 'plan_rejected') AS plan_rejected,
           count(f.id) FILTER (WHERE f.outcome = 'base_diverged') AS base_diverged,
           count(f.id) FILTER (WHERE f.outcome = 'failed') AS failed,
           count(f.id) AS total
    FROM bk LEFT JOIN fin f ON f.bidx = bk.idx
    GROUP BY bk.idx
),
fin_total AS (
    SELECT jsonb_build_object(
               'completed', count(*) FILTER (WHERE f.outcome = 'completed'),
               'cancelled', count(*) FILTER (WHERE f.outcome = 'cancelled'),
               'plan_rejected', count(*) FILTER (WHERE f.outcome = 'plan_rejected'),
               'base_diverged', count(*) FILTER (WHERE f.outcome = 'base_diverged'),
               'failed', count(*) FILTER (WHERE f.outcome = 'failed'),
               'total', count(*)) AS j
    FROM fin f
),
start_delay AS (
    SELECT count(*) FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS observed,
           count(*) FILTER (WHERE c.first_started_at IS NULL) AS not_started,
           count(*) FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at < c.created_at) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (c.first_started_at - c.created_at)))
               FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (c.first_started_at - c.created_at)))
               FILTER (WHERE c.first_started_at IS NOT NULL AND c.first_started_at >= c.created_at) AS p90
    FROM cc c
),
gate_runs AS MATERIALIZED (
    -- Plan response covers every scoped kind, chat included when selected; only the funnel and
    -- approval cohorts (fr) restrict to issue and self_improve.
    SELECT c.id, c.gate_revision FROM cc c
),
rcpt AS MATERIALIZED (
    SELECT g.id AS run_id, g.gate_revision AS run_rev, i.gate_binding, i.gate_revision AS rev,
           i.created_at AS in_at, i.applied_at, i.disposition
    FROM gate_runs g
    JOIN LATERAL (
        SELECT x.gate_binding, x.gate_revision, x.created_at, x.applied_at, x.disposition
        FROM run_user_inputs x
        WHERE x.run_id = g.id AND x.kind = 'approve_plan'
        OFFSET 0
    ) i ON true
),
rcpt_c AS MATERIALIZED (
    SELECT r.*,
           COALESCE(r.gate_binding = 'bound' AND r.rev > 0 AND r.applied_at IS NOT NULL, false) AS applied_bound,
           (r.disposition IS NULL OR r.disposition = 'applied') AS disp_ok,
           COALESCE(r.in_at <= r.applied_at, false) AS chron_ok
    FROM rcpt r
),
rcpt_pair AS (
    SELECT x.run_id, x.rev,
           count(*) FILTER (WHERE x.disp_ok AND x.chron_ok) AS n_elig,
           count(*) FILTER (WHERE x.disp_ok AND x.chron_ok AND x.disposition = 'applied') AS n_elig_applied,
           count(*) FILTER (WHERE x.disp_ok AND NOT x.chron_ok) AS n_chron,
           count(*) FILTER (WHERE NOT x.disp_ok) AS n_excl,
           min(x.in_at) FILTER (WHERE x.disp_ok AND x.chron_ok) AS in_at
    FROM rcpt_c x
    WHERE x.applied_bound
    GROUP BY x.run_id, x.rev
),
pres AS MATERIALIZED (
    SELECT g.id AS run_id, p.revision AS rev, p.created_at AS pres_at
    FROM gate_runs g
    JOIN LATERAL (
        SELECT x.revision, x.created_at
        FROM run_gate_presentations x
        WHERE x.run_id = g.id
        OFFSET 0
    ) p ON true
),
pair_class AS (
    SELECT q.*,
           CASE WHEN q.n_elig > 1 THEN 'ambiguous'
                WHEN q.n_elig = 1 AND q.n_elig_applied = 1 THEN 'reconciliation_only'
                WHEN q.n_elig = 1 AND q.pres_at IS NULL THEN 'no_presentation'
                WHEN q.n_elig = 1 AND q.pres_at > q.in_at THEN 'invalid_chronology'
                WHEN q.n_elig = 1 THEN 'timed'
                WHEN q.n_chron > 0 THEN 'invalid_chronology'
                WHEN q.n_excl > 0 THEN 'excluded_disposition'
                ELSE 'not_approved' END AS cls
    FROM (SELECT COALESCE(p.run_id, a.run_id) AS run_id, COALESCE(p.rev, a.rev) AS rev, p.pres_at,
                 COALESCE(a.n_elig, 0) AS n_elig, COALESCE(a.n_elig_applied, 0) AS n_elig_applied,
                 COALESCE(a.n_chron, 0) AS n_chron, COALESCE(a.n_excl, 0) AS n_excl, a.in_at
          FROM pres p
          FULL OUTER JOIN rcpt_pair a ON a.run_id = p.run_id AND a.rev = p.rev) q
),
plan_resp AS (
    SELECT count(*) FILTER (WHERE pc.cls = 'timed') AS n,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (pc.in_at - pc.pres_at))) FILTER (WHERE pc.cls = 'timed') AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY extract(epoch FROM (pc.in_at - pc.pres_at))) FILTER (WHERE pc.cls = 'timed') AS p90,
           count(*) FILTER (WHERE pc.cls = 'ambiguous') AS ambiguous,
           count(*) FILTER (WHERE pc.cls = 'no_presentation') AS no_presentation,
           count(*) FILTER (WHERE pc.cls = 'invalid_chronology') AS invalid_chronology,
           count(*) FILTER (WHERE pc.cls = 'excluded_disposition') AS excluded_disposition,
           count(*) FILTER (WHERE pc.cls = 'reconciliation_only') AS reconciliation_only,
           count(*) FILTER (WHERE pc.cls = 'not_approved') AS not_approved
    FROM pair_class pc
),
plan_legacy AS (
    SELECT count(*) AS n FROM (SELECT x.run_id FROM rcpt_c x
                               GROUP BY x.run_id
                               HAVING bool_or(x.gate_binding IS NULL OR x.gate_binding = 'unbound')) l
),
fr AS MATERIALIZED (
    SELECT c.id, c.kind, c.status, c.created_at, c.auto_approve, c.has_plan, c.plan_source, c.gate_revision,
           c.repo_id, c.mr_iid, c.bidx, o.outcome,
           COALESCE(e.cur_n, 0) AS cur_n, COALESCE(e.hist_ok, 0) AS hist_ok, COALESCE(e.hist_amb, 0) AS hist_amb,
           COALESCE(e.legacy, false) AS legacy, COALESCE(e.any_receipt, 0) AS any_receipt,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL) AS pr_present,
           (c.mr_iid IS NOT NULL AND c.repo_id IS NOT NULL
                AND (c.repo_id, c.mr_iid) IN (SELECT mw.repo_id, mw.mr_iid FROM merged_witness mw)) AS witnessed
    FROM cc c
    LEFT JOIN LATERAL (SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0) o ON true
    -- Per-run receipt aggregate as a probe of idx_run_user_inputs_run_id: a join against a cohort-wide
    -- GROUP BY CTE is re-read once per run under a generic plan.
    -- Eligible receipts are counted per revision (g.n): an older revision with exactly one is historical
    -- approval (hist_ok), one with more than one proves nothing (hist_amb), and one with none is in neither.
    LEFT JOIN LATERAL (
        SELECT COALESCE(max(g.n) FILTER (WHERE g.rev = c.gate_revision), 0) AS cur_n,
               count(*) FILTER (WHERE g.rev < c.gate_revision AND g.n = 1) AS hist_ok,
               count(*) FILTER (WHERE g.rev < c.gate_revision AND g.n > 1) AS hist_amb,
               bool_or(g.legacy) AS legacy,
               COALESCE(sum(g.total), 0)::bigint AS any_receipt
        FROM (SELECT y.rev, count(*) FILTER (WHERE y.ab AND y.dok AND y.cok) AS n,
                     bool_or(y.gate_binding IS NULL OR y.gate_binding = 'unbound') AS legacy,
                     count(*) AS total
              FROM (SELECT x.gate_binding, x.gate_revision AS rev,
                           COALESCE(x.gate_binding = 'bound' AND x.gate_revision > 0 AND x.applied_at IS NOT NULL, false) AS ab,
                           (x.disposition IS NULL OR x.disposition = 'applied') AS dok,
                           COALESCE(x.created_at <= x.applied_at, false) AS cok
                    FROM run_user_inputs x WHERE x.run_id = c.id AND x.kind = 'approve_plan' OFFSET 0) y
              GROUP BY y.rev) g
    ) e ON true
    WHERE c.kind IN ('issue', 'self_improve')
),
fr_class AS (
    SELECT f.*,
           CASE WHEN f.outcome = 'plan_rejected' THEN 'rejected'
                WHEN f.cur_n = 1 THEN 'settled_human'
                WHEN f.auto_approve AND f.gate_revision = 0 AND f.has_plan AND f.plan_source <> 'seeded' THEN 'settled_auto'
                WHEN f.plan_source = 'seeded' AND f.gate_revision = 0 THEN 'skipped'
                WHEN f.status NOT IN ('completed', 'failed', 'cancelled') THEN 'in_progress'
                WHEN f.status = 'failed' THEN 'failed'
                WHEN f.status = 'cancelled' THEN 'cancelled'
                ELSE 'unknown' END AS planning,
           (f.gate_revision > 0 OR f.auto_approve OR f.any_receipt > 0) AS in_plan_cohort,
           CASE WHEN f.cur_n = 1 THEN 'human'
                WHEN f.auto_approve THEN 'auto_approve_configured'
                WHEN f.cur_n > 1 OR (f.hist_amb > 0 AND f.hist_ok = 0) THEN 'ambiguous'
                WHEN f.legacy THEN 'legacy_unbound'
                WHEN f.hist_ok > 0 THEN 'historical_approval_only'
                ELSE 'no_evidence' END AS approval
    FROM fr f
),
pr_units AS (
    SELECT DISTINCT ON (f.repo_id, f.mr_iid) f.repo_id, f.mr_iid, f.bidx, f.witnessed
    FROM fr f
    WHERE f.pr_present
    ORDER BY f.repo_id, f.mr_iid, f.created_at, f.id
),
pr_cells AS (
    SELECT bk.idx, count(u.mr_iid) AS present, count(u.mr_iid) FILTER (WHERE u.witnessed) AS merged
    FROM bk LEFT JOIN pr_units u ON u.bidx = bk.idx
    GROUP BY bk.idx
),
funnel_applicable AS (
    SELECT EXISTS (SELECT 1 FROM unnest(@kinds::text[]) k WHERE k IN ('issue', 'self_improve')) AS ok
),
funnel AS (
    SELECT count(*) AS created,
           count(*) FILTER (WHERE f.planning IN ('settled_human', 'settled_auto')) AS plan_settled,
           count(*) FILTER (WHERE f.outcome = 'completed') AS completed,
           count(*) FILTER (WHERE f.pr_present) AS pr_present,
           count(*) FILTER (WHERE f.witnessed) AS merged,
           count(*) FILTER (WHERE f.planning = 'settled_human') AS p_human,
           count(*) FILTER (WHERE f.planning = 'settled_auto') AS p_auto,
           count(*) FILTER (WHERE f.planning = 'skipped') AS p_skipped,
           count(*) FILTER (WHERE f.planning = 'rejected') AS p_rejected,
           count(*) FILTER (WHERE f.planning = 'in_progress') AS p_in_progress,
           count(*) FILTER (WHERE f.planning = 'failed') AS p_failed,
           count(*) FILTER (WHERE f.planning = 'cancelled') AS p_cancelled,
           count(*) FILTER (WHERE f.planning = 'unknown') AS p_unknown,
           count(*) FILTER (WHERE f.outcome = 'failed') AS o_failed,
           count(*) FILTER (WHERE f.outcome = 'cancelled') AS o_cancelled,
           count(*) FILTER (WHERE f.outcome = 'plan_rejected') AS o_plan_rejected,
           count(*) FILTER (WHERE f.outcome = 'base_diverged') AS o_base_diverged,
           count(*) FILTER (WHERE f.outcome IS NULL) AS o_in_progress,
           count(*) FILTER (WHERE f.cur_n = 0 AND f.hist_ok > 0) AS d_historical,
           count(*) FILTER (WHERE f.cur_n > 1) AS d_ambiguous,
           count(*) FILTER (WHERE f.legacy) AS d_legacy,
           count(*) FILTER (WHERE f.in_plan_cohort) AS a_total,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'human') AS a_human,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'auto_approve_configured') AS a_auto,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval NOT IN ('human', 'auto_approve_configured')) AS a_unknown,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'ambiguous') AS a_ambiguous,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'legacy_unbound') AS a_legacy,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'historical_approval_only') AS a_historical,
           count(*) FILTER (WHERE f.in_plan_cohort AND f.approval = 'no_evidence') AS a_none
    FROM fr_class f
),
pr_unit_totals AS (
    SELECT count(*) AS present, count(*) FILTER (WHERE u.witnessed) AS merged FROM pr_units u
),
el AS MATERIALIZED (
    SELECT t.id, t.bidx, t.terminal,
           CASE WHEN t.terminal AND t.first_started_at IS NOT NULL
                     AND COALESCE(t.finished_at, t.status_since) >= t.first_started_at
                THEN extract(epoch FROM (COALESCE(t.finished_at, t.status_since) - t.first_started_at))::float8 END AS dur
    FROM (SELECT c.id, c.bidx, c.first_started_at, c.finished_at, c.status_since,
                 (c.status IN ('completed', 'failed', 'cancelled')) AS terminal
          FROM cc c WHERE c.kind = 'issue') t
),
el_cells AS (
    SELECT bk.idx,
           count(e.dur) AS n,
           count(e.id) FILTER (WHERE NOT e.terminal) AS unfinished,
           count(e.id) FILTER (WHERE e.terminal AND e.dur IS NULL) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY e.dur) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY e.dur) AS p90
    FROM bk LEFT JOIN el e ON e.bidx = bk.idx
    GROUP BY bk.idx
),
el_total AS (
    SELECT count(e.dur) AS n,
           count(e.id) FILTER (WHERE NOT e.terminal) AS unfinished,
           count(e.id) FILTER (WHERE e.terminal AND e.dur IS NULL) AS invalid,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY e.dur) AS p50,
           percentile_cont(0.9) WITHIN GROUP (ORDER BY e.dur) AS p90
    FROM el e
),
hist_edges AS (
    SELECT t.e AS lower_s, t.ord::int AS ord, lead(t.e) OVER (ORDER BY t.ord) AS upper_s
    FROM unnest(@histogram_edges::float8[]) WITH ORDINALITY AS t(e, ord)
),
hist AS (
    SELECT h.ord, h.lower_s, h.upper_s, count(e.dur) AS n
    FROM hist_edges h
    LEFT JOIN el e ON e.dur IS NOT NULL AND GREATEST(width_bucket(e.dur, @histogram_edges::float8[]), 1) = h.ord
    GROUP BY h.ord, h.lower_s, h.upper_s
)
SELECT jsonb_build_object(
    'generated_at', (SELECT to_char(prm.now_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM prm),
    'waiting_now', jsonb_build_object(
        'time_basis', 'now',
        'population', 'current_runs',
        'states', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'state', w.state,
                'count', w.n,
                'oldest_status_since', to_char(w.oldest AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                'oldest_age_seconds', CASE WHEN w.oldest IS NULL THEN NULL
                                           ELSE GREATEST(extract(epoch FROM ((SELECT p.now_ts FROM prm p) - w.oldest)), 0)::float8 END)
                ORDER BY w.ord)
            FROM waiting w), '[]'::jsonb),
        'total', (SELECT COALESCE(sum(w.n), 0)::bigint FROM waiting w)),
    'activity', jsonb_build_object(
        'created', jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window',
            'total', (SELECT count(*) FROM cc),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'count', c.n) ORDER BY c.idx) FROM created_cells c), '[]'::jsonb)),
        'finished', jsonb_build_object(
            'time_basis', 'finished_at',
            'population', 'finished_in_window',
            'excluded_kinds', jsonb_build_array('chat'),
            'total', (SELECT j FROM fin_total),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                    'index', c.idx - 1, 'completed', c.completed, 'cancelled', c.cancelled,
                    'plan_rejected', c.plan_rejected, 'base_diverged', c.base_diverged,
                    'failed', c.failed, 'total', c.total) ORDER BY c.idx) FROM fin_cells c), '[]'::jsonb))),
    'first_start_delay', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window',
            'approximate', true,
            'limitations', jsonb_build_array('first_started_at_legacy_backfill'),
            'observed', d.observed,
            'not_started', d.not_started,
            'invalid', d.invalid,
            'p50_seconds', jsonb_build_object('value', d.p50, 'reason', CASE WHEN d.observed = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', d.p90, 'reason', CASE WHEN d.observed = 0 THEN 'no_observations' END))
        FROM start_delay d),
    'plan_response', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_gate_pairs',
            'approximate', true,
            'n', p.n,
            'p50_seconds', jsonb_build_object('value', p.p50, 'reason', CASE WHEN p.n = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', p.p90, 'reason', CASE WHEN p.n = 0 THEN 'no_observations' END),
            'unknown', jsonb_build_object(
                'total', p.ambiguous + p.no_presentation + p.invalid_chronology + p.excluded_disposition + p.reconciliation_only + (SELECT l.n FROM plan_legacy l),
                'reasons', jsonb_build_object(
                    'ambiguous', p.ambiguous,
                    'no_presentation', p.no_presentation,
                    'invalid_chronology', p.invalid_chronology,
                    'excluded_disposition', p.excluded_disposition,
                    'reconciliation_only', p.reconciliation_only,
                    'legacy_unbound', (SELECT l.n FROM plan_legacy l))),
            'not_approved', p.not_approved)
        FROM plan_resp p),
    'delivery_funnel', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_delivery_kinds',
            'applicable', a.ok,
            'reason', CASE WHEN a.ok THEN NULL ELSE 'not_applicable' END,
            'stages', jsonb_build_object('created', f.created, 'plan_settled', f.plan_settled, 'completed', f.completed,
                                         'pr_present', f.pr_present, 'merged', f.merged),
            'planning', jsonb_build_object('settled_human', f.p_human, 'settled_auto', f.p_auto, 'skipped', f.p_skipped,
                                           'rejected', f.p_rejected, 'in_progress', f.p_in_progress, 'failed', f.p_failed,
                                           'cancelled', f.p_cancelled, 'unknown', f.p_unknown),
            'outcomes', jsonb_build_object('completed', f.completed, 'failed', f.o_failed, 'cancelled', f.o_cancelled,
                                           'plan_rejected', f.o_plan_rejected, 'base_diverged', f.o_base_diverged,
                                           'in_progress', f.o_in_progress),
            'disclosure', jsonb_build_object('historical_approval_only', f.d_historical, 'ambiguous_current', f.d_ambiguous,
                                             'legacy_evidence', f.d_legacy),
            'pr_units', jsonb_build_object(
                'present', (SELECT t.present FROM pr_unit_totals t),
                'merged', (SELECT t.merged FROM pr_unit_totals t),
                'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1, 'present', c.present, 'merged', c.merged) ORDER BY c.idx) FROM pr_cells c), '[]'::jsonb)))
        FROM funnel f, funnel_applicable a),
    'approval', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_plan_cohort',
            'applicable', a.ok,
            'reason', CASE WHEN a.ok THEN NULL ELSE 'not_applicable' END,
            'total', f.a_total,
            'human', f.a_human,
            'auto_approve_configured', f.a_auto,
            'unknown', jsonb_build_object(
                'total', f.a_unknown,
                'reasons', jsonb_build_object('ambiguous', f.a_ambiguous, 'legacy_unbound', f.a_legacy,
                                              'historical_approval_only', f.a_historical, 'no_evidence', f.a_none)))
        FROM funnel f, funnel_applicable a),
    'issue_elapsed', (
        SELECT jsonb_build_object(
            'time_basis', 'created_at',
            'population', 'created_in_window_issue_runs',
            'applicable', (SELECT 'issue' = ANY(@kinds::text[])),
            'reason', CASE WHEN 'issue' = ANY(@kinds::text[]) THEN NULL ELSE 'not_applicable' END,
            'n', t.n,
            'unfinished', t.unfinished,
            'invalid', t.invalid,
            'incomplete', t.unfinished > 0,
            'p50_seconds', jsonb_build_object('value', t.p50, 'reason', CASE WHEN t.n = 0 THEN 'no_observations' END),
            'p90_seconds', jsonb_build_object('value', t.p90, 'reason', CASE WHEN t.n = 0 THEN 'no_observations' END),
            'histogram', COALESCE((SELECT jsonb_agg(jsonb_build_object('lower_seconds', h.lower_s, 'upper_seconds', h.upper_s, 'count', h.n) ORDER BY h.ord) FROM hist h), '[]'::jsonb),
            'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object(
                    'index', c.idx - 1, 'n', c.n, 'unfinished', c.unfinished, 'invalid', c.invalid,
                    'incomplete', c.unfinished > 0,
                    'p50_seconds', jsonb_build_object(
                        'value', CASE WHEN c.n >= @min_percentile_n::int THEN c.p50 END,
                        'reason', CASE WHEN c.n = 0 THEN 'no_observations' WHEN c.n < @min_percentile_n::int THEN 'insufficient_observations' END),
                    'p90_seconds', jsonb_build_object(
                        'value', CASE WHEN c.n >= @min_percentile_n::int THEN c.p90 END,
                        'reason', CASE WHEN c.n = 0 THEN 'no_observations' WHEN c.n < @min_percentile_n::int THEN 'insufficient_observations' END))
                    ORDER BY c.idx) FROM el_cells c), '[]'::jsonb))
        FROM el_total t)
)::jsonb AS doc;
