-- name: AnalyticsOwnerQuality :one
-- Issue #4730: the Quality document, one jsonb value: PR rework groups, CI-fix outcomes,
-- cross-check outcomes by stage and judge review coverage. The owner and all-scope statements
-- are kept textually identical except the scope lines, each tagged with a trailing marker
-- comment (a Go test drops them and fails on any other drift). Every range comes from the
-- parameters (half-open: start <= ts < end); there is no calendar arithmetic and no clock read
-- here. The document carries counts only, so it does not depend on the session TimeZone.
-- Every array is built with jsonb_agg(... ORDER BY idx),
-- one entry per bucket (empty buckets included, zero-based index); every class map has a fixed
-- key list, and an absent class is zero.
WITH
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
-- Rework: seed PRs (creation cohort collapsed to one row per PR BEFORE any join), the scoped
-- merge witness, the retained mr_rework runs on the selected PRs and their verified links.
rw_seed AS MATERIALIZED (
    SELECT r.repo_id, r.mr_iid, min(r.created_at) AS first_at
    FROM runs r
    WHERE r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
      AND r.kind = ANY(@delivery_kinds::text[])
      AND r.repo_id IS NOT NULL AND r.mr_iid IS NOT NULL
    GROUP BY r.repo_id, r.mr_iid
),
rw_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
      AND w.user_id = @user_id::uuid  -- owner-only
),
-- The witness is folded in by one GROUP BY over a UNION ALL of the seed and the witness keys
-- (a PR is witnessed when any witness row carries its key; HAVING keeps only seeded PRs). A
-- membership subquery here would become an unhashed per-row SubPlan once the witness set
-- outgrew hash_mem.
rw_pr AS MATERIALIZED (
    SELECT p.repo_id, p.mr_iid, p.witnessed,
           CASE WHEN p.wb >= 1 AND p.first_at < (@bucket_ends::timestamptz[])[p.wb] THEN p.wb END AS bidx
    FROM (
        SELECT u.repo_id, u.mr_iid, min(u.first_at) AS first_at, bool_or(u.w) AS witnessed,
               width_bucket(min(u.first_at), @bucket_starts::timestamptz[]) AS wb
        FROM (
            SELECT s.repo_id, s.mr_iid, s.first_at, false AS w FROM rw_seed s
            UNION ALL
            SELECT w.repo_id, w.mr_iid, NULL::timestamptz, true FROM rw_witness w
        ) u
        GROUP BY u.repo_id, u.mr_iid
        HAVING bool_or(NOT u.w)
    ) p
),
rw_sel AS MATERIALIZED (
    SELECT p.repo_id, p.mr_iid, p.bidx FROM rw_pr p WHERE p.witnessed
),
rework AS MATERIALIZED (
    SELECT id, repo_id, mr_iid, target_run_id, user_id
    FROM runs
    WHERE kind = 'mr_rework' AND created_at < @now::timestamptz
      AND user_id = @user_id::uuid  -- owner-only
),
rw_link AS MATERIALIZED (
    SELECT rw.repo_id, rw.mr_iid, rw.id AS rework_id,
           (t.repo_id = rw.repo_id AND t.mr_iid = rw.mr_iid AND t.user_id = rw.user_id) IS TRUE AS verified
    FROM rework rw
    LEFT JOIN runs t ON t.id = rw.target_run_id
    -- The IS TRUE wrapper prevents semi-join pull-up; PostgreSQL hashes the SubPlan only when it fits the hash memory budget.
    WHERE ((rw.repo_id, rw.mr_iid) IN (SELECT q.repo_id, q.mr_iid FROM rw_sel q)) IS TRUE
),
rw_cnt AS MATERIALIZED (
    SELECT u.repo_id, u.mr_iid, max(u.bidx) AS bidx,
           CASE WHEN sum(u.v) = 0 THEN 'zero'
                WHEN sum(u.v) = 1 THEN 'one'
                WHEN sum(u.v) = 2 THEN 'two'
                ELSE 'three_plus' END AS grp
    FROM (
        SELECT repo_id, mr_iid, bidx, 0 AS v FROM rw_sel
        UNION ALL
        SELECT repo_id, mr_iid, NULL::int, CASE WHEN verified THEN 1 ELSE 0 END FROM rw_link
    ) u
    GROUP BY u.repo_id, u.mr_iid
),
rw_key AS MATERIALIZED (
    SELECT s.repo_id, s.mr_iid, (fc.id IS NOT NULL) AS detectable,
           fc.forge_type, lower(rtrim(fc.base_url, '/')) AS base_key, rp.forge_project_id
    FROM rw_sel s
    JOIN repos rp ON rp.id = s.repo_id
    LEFT JOIN forge_connections fc ON fc.id = rp.connection_id
      AND fc.user_id = @user_id::uuid  -- owner-only
),
rw_shared AS (
    SELECT k.repo_id, k.mr_iid
    FROM (
        SELECT y.repo_id, y.mr_iid,
               count(*) OVER (PARTITION BY y.forge_type, y.base_key, y.forge_project_id, y.mr_iid) AS n
        FROM rw_key y WHERE y.detectable
    ) k
    WHERE k.n >= 2
),
rw_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'prs', count(c.grp),
            'groups', jsonb_build_object('zero', count(c.grp) FILTER (WHERE c.grp = 'zero'), 'one', count(c.grp) FILTER (WHERE c.grp = 'one'), 'two', count(c.grp) FILTER (WHERE c.grp = 'two'), 'three_plus', count(c.grp) FILTER (WHERE c.grp = 'three_plus'))) AS j
    FROM bk LEFT JOIN rw_cnt c ON c.bidx = bk.idx
    GROUP BY bk.idx
),
-- CI fix: runs created in the window, their outcome class from run_outcome_classes.
cf AS MATERIALIZED (
    SELECT r.id, r.created_at, o.outcome,
           width_bucket(r.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM runs r
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = r.id OFFSET 0
    ) o ON true
    WHERE r.kind = 'ci_fix' AND r.kind = ANY(@kinds::text[])
      AND r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
),
cfb AS (
    SELECT f.*,
           CASE WHEN f.wb >= 1 AND f.created_at < (@bucket_ends::timestamptz[])[f.wb] THEN f.wb END AS bidx
    FROM cf f
),
cf_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'runs', count(x.id),
            'finished', count(x.id) FILTER (WHERE x.outcome IS NOT NULL),
            'in_flight', count(x.id) FILTER (WHERE x.outcome IS NULL),
            'outcomes', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.outcome = 'completed'), 'cancelled', count(x.id) FILTER (WHERE x.outcome = 'cancelled'), 'plan_rejected', count(x.id) FILTER (WHERE x.outcome = 'plan_rejected'), 'base_diverged', count(x.id) FILTER (WHERE x.outcome = 'base_diverged'), 'failed', count(x.id) FILTER (WHERE x.outcome = 'failed'))) AS j
    FROM bk LEFT JOIN cfb x ON x.bidx = bk.idx
    GROUP BY bk.idx
),
-- Cross-check: attempts selected through the lead run (lead repo, harness and kind), with no
-- creation-time restriction. Attempts whose stage cannot apply to the lead kind (plan_lead_kinds /
-- code_lead_kinds) are excluded under every filter. Each remaining non-pending attempt is either
-- dated (decided_at and created_at finite, decided_at >= created_at) or undated; a dated attempt
-- decided outside [from, now), including a future decided_at, is outside the window and not
-- counted. Stage selects the stored result field: plan reads verdict, code reads
-- outcome (the legacy code verdict column is inert).
cc_base AS MATERIALIZED (
    SELECT cc.id, cc.stage,
           CASE WHEN cc.stage = 'code' THEN cc.outcome ELSE cc.verdict END::text AS result,
           cc.reason_class, cc.interrupted_at, cc.decided_at, cc.created_at, cc.findings,
           CASE WHEN cc.checker_harness IN ('claude', 'codex') THEN cc.checker_harness ELSE 'unknown' END::text AS checker,
           (((cc.stage = 'plan' AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending'))
             AND cc.interrupted_at IS NULL) AS is_pending
    FROM cross_checks cc
    JOIN runs lead ON lead.id = cc.lead_run_id
    WHERE (sqlc.narg('repo_id')::uuid IS NULL OR lead.repo_id = sqlc.narg('repo_id')::uuid)
      AND lead.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('harness')::text IS NULL OR lead.harness = sqlc.narg('harness')::text)
      AND lead.kind = ANY(@kinds::text[]) AND lead.kind <> 'cross_check'
      AND ((cc.stage = 'plan' AND lead.kind = ANY(@plan_lead_kinds::text[]))
        OR (cc.stage = 'code' AND lead.kind = ANY(@code_lead_kinds::text[])))
),
cc_dec AS MATERIALIZED (
    SELECT b.*,
           (b.decided_at IS NOT NULL AND isfinite(b.decided_at) AND isfinite(b.created_at)
             AND b.decided_at >= b.created_at) AS dated
    FROM cc_base b
    WHERE NOT b.is_pending
),
cc_win AS MATERIALIZED (
    SELECT cc.id, cc.stage, cc.result, cc.reason_class, cc.interrupted_at, cc.decided_at, cc.checker,
           CASE WHEN cc.stage = 'code' AND jsonb_typeof(cc.findings) = 'array' AND octet_length(cc.findings::text) <= 65536
             THEN jsonb_array_length(cc.findings)
            WHEN cc.stage = 'plan' AND jsonb_typeof(cc.findings->'items') = 'array'
             AND octet_length(cc.findings::text) <= 65536 THEN jsonb_array_length(cc.findings->'items')
            ELSE -1 END::integer AS findings_count,
           width_bucket(cc.decided_at, @bucket_starts::timestamptz[]) AS wb
    FROM cc_dec cc
    WHERE cc.dated AND cc.decided_at >= sqlc.arg('from')::timestamptz AND cc.decided_at < @now::timestamptz
),
cc_cls AS MATERIALIZED (
    SELECT w.*,
           CASE WHEN w.wb >= 1 AND w.decided_at < (@bucket_ends::timestamptz[])[w.wb] THEN w.wb END AS bidx
    FROM (
        SELECT *, CASE
         -- A Plan verdict recorded before the lead lost custody keeps its verdict (#4037).
         -- IS NOT TRUE keeps any NULL term (e.g. a missing reason_class) replaced.
         WHEN reason_class IN ('superseded','approved_not_stored')
          OR (interrupted_at IS NOT NULL AND (stage='plan' AND decided_at IS NOT NULL AND decided_at <= interrupted_at
           AND result IN ('approve','revise','block') AND reason_class = result) IS NOT TRUE) THEN 'replaced'
         WHEN result='failed' THEN 'didnt_finish'
         WHEN stage='plan' AND result IN ('revise','block') AND reason_class=result THEN 'raised_issues'
         WHEN stage='plan' AND result='approve' AND reason_class='approve' THEN 'passed'
         WHEN stage='code' AND result='completed' AND findings_count > 0 THEN 'raised_issues'
         WHEN stage='code' AND result='completed' AND findings_count = 0 THEN 'passed'
         ELSE '' END::text AS category
        FROM cc_win
    ) w
),
cc_plan_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'decided', count(x.id),
            'stored_result', jsonb_build_object('approve', count(x.id) FILTER (WHERE x.result = 'approve'), 'revise', count(x.id) FILTER (WHERE x.result = 'revise'), 'block', count(x.id) FILTER (WHERE x.result = 'block'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
            'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = ''))) AS j
    FROM bk LEFT JOIN cc_cls x ON x.stage = 'plan' AND x.bidx = bk.idx
    GROUP BY bk.idx
),
cc_plan AS (
    SELECT jsonb_build_object(
        'decided_in_window', count(x.id),
        'stored_result', jsonb_build_object('approve', count(x.id) FILTER (WHERE x.result = 'approve'), 'revise', count(x.id) FILTER (WHERE x.result = 'revise'), 'block', count(x.id) FILTER (WHERE x.result = 'block'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
        'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = '')),
        'checker_harness', jsonb_build_object('claude', count(x.id) FILTER (WHERE x.checker = 'claude'), 'codex', count(x.id) FILTER (WHERE x.checker = 'codex'), 'unknown', count(x.id) FILTER (WHERE x.checker = 'unknown')),
        'undated', (SELECT jsonb_build_object('count', count(d.id), 'stored_result', jsonb_build_object('approve', count(d.id) FILTER (WHERE d.result = 'approve'), 'revise', count(d.id) FILTER (WHERE d.result = 'revise'), 'block', count(d.id) FILTER (WHERE d.result = 'block'), 'failed', count(d.id) FILTER (WHERE d.result = 'failed')))
                    FROM cc_dec d WHERE d.stage = 'plan' AND NOT d.dated),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', g.idx - 1) || g.j ORDER BY g.idx)
                             FROM cc_plan_cells g), '[]'::jsonb)) AS j
    FROM cc_cls x WHERE x.stage = 'plan'
),
cc_code_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'decided', count(x.id),
            'stored_result', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.result = 'completed'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
            'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = ''))) AS j
    FROM bk LEFT JOIN cc_cls x ON x.stage = 'code' AND x.bidx = bk.idx
    GROUP BY bk.idx
),
cc_code AS (
    SELECT jsonb_build_object(
        'decided_in_window', count(x.id),
        'stored_result', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.result = 'completed'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
        'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = '')),
        'checker_harness', jsonb_build_object('claude', count(x.id) FILTER (WHERE x.checker = 'claude'), 'codex', count(x.id) FILTER (WHERE x.checker = 'codex'), 'unknown', count(x.id) FILTER (WHERE x.checker = 'unknown')),
        'undated', (SELECT jsonb_build_object('count', count(d.id), 'stored_result', jsonb_build_object('completed', count(d.id) FILTER (WHERE d.result = 'completed'), 'failed', count(d.id) FILTER (WHERE d.result = 'failed')))
                    FROM cc_dec d WHERE d.stage = 'code' AND NOT d.dated),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', g.idx - 1) || g.j ORDER BY g.idx)
                             FROM cc_code_cells g), '[]'::jsonb)) AS j
    FROM cc_cls x WHERE x.stage = 'code'
),
cc_pending AS (
    SELECT jsonb_build_object(
            'total', count(p.id),
            'plan', count(p.id) FILTER (WHERE p.stage = 'plan'),
            'code', count(p.id) FILTER (WHERE p.stage = 'code')) AS j
    FROM cc_base p WHERE p.is_pending
),
-- Judge: the target cohort by creation time, the stable skip rules by failure origin, and the
-- retained review (one row per target; a rejudge overwrites it in place).
jd AS MATERIALIZED (
    SELECT r.id, r.status, r.created_at,
           (r.status = 'failed' AND (r.fail_origin = ANY(@always_skip_origins::text[])
             OR (r.iteration_count = 0 AND r.fail_origin = ANY(@prestart_origins::text[])))) AS skip_expr,
           rv.id AS rv_id, rv.status AS rv_status, rv.verdict AS rv_verdict,
           EXISTS (SELECT 1 FROM judge_skips js WHERE js.target_run_id = r.id) AS skipped,
           width_bucket(r.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM runs r
    LEFT JOIN run_reviews rv ON rv.target_run_id = r.id AND rv.user_id = r.user_id
    WHERE r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND r.user_id = @user_id::uuid  -- owner-only
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@judge_kinds::text[])
      AND r.kind = ANY(@kinds::text[])
),
jd2 AS MATERIALIZED (
    SELECT j.*,
           (j.status IN ('completed', 'failed') AND j.skip_expr IS NOT TRUE) AS eligible,
           CASE WHEN j.wb >= 1 AND j.created_at < (@bucket_ends::timestamptz[])[j.wb] THEN j.wb END AS bidx
    FROM jd j
),
jd_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'eligible', count(x.id) FILTER (WHERE x.eligible),
            'complete', count(x.id) FILTER (WHERE x.eligible AND x.rv_status = 'complete')) AS j
    FROM bk LEFT JOIN jd2 x ON x.bidx = bk.idx
    GROUP BY bk.idx
)
SELECT jsonb_build_object(
    'rework', jsonb_build_object(
        'applicable_prs', (SELECT count(*) FROM rw_sel),
        'groups', (SELECT jsonb_build_object('zero', count(*) FILTER (WHERE c.grp = 'zero'), 'one', count(*) FILTER (WHERE c.grp = 'one'), 'two', count(*) FILTER (WHERE c.grp = 'two'), 'three_plus', count(*) FILTER (WHERE c.grp = 'three_plus')) FROM rw_cnt c),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM rw_cells c), '[]'::jsonb),
        'unwitnessed_prs', (SELECT count(*) FROM rw_pr p WHERE NOT p.witnessed),
        'unverified_links', (SELECT count(DISTINCT l.rework_id) FROM rw_link l WHERE NOT l.verified),
        'shared_external_prs', (SELECT count(*) FROM rw_shared),
        'shared_detection_unavailable', (SELECT count(*) FROM rw_key k WHERE NOT k.detectable)),
    'ci_fix', jsonb_build_object(
        'runs', (SELECT count(*) FROM cfb),
        'finished', (SELECT count(*) FROM cfb x WHERE x.outcome IS NOT NULL),
        'in_flight', (SELECT count(*) FROM cfb x WHERE x.outcome IS NULL),
        'outcomes', (SELECT jsonb_build_object('completed', count(*) FILTER (WHERE x.outcome = 'completed'), 'cancelled', count(*) FILTER (WHERE x.outcome = 'cancelled'), 'plan_rejected', count(*) FILTER (WHERE x.outcome = 'plan_rejected'), 'base_diverged', count(*) FILTER (WHERE x.outcome = 'base_diverged'), 'failed', count(*) FILTER (WHERE x.outcome = 'failed')) FROM cfb x),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cf_cells c), '[]'::jsonb)),
    'cross_check', jsonb_build_object(
        'plan', (SELECT j FROM cc_plan),
        'code', (SELECT j FROM cc_code),
        'pending', (SELECT j FROM cc_pending)),
    'judge', jsonb_build_object(
        'candidates', (SELECT jsonb_build_object('total', count(*) FILTER (WHERE x.status IN ('completed', 'failed')), 'completed', count(*) FILTER (WHERE x.status = 'completed'), 'failed', count(*) FILTER (WHERE x.status = 'failed')) FROM jd2 x),
        'eligible', (SELECT jsonb_build_object(
            'total', count(*) FILTER (WHERE x.eligible),
            'unreviewed', count(*) FILTER (WHERE x.eligible AND x.rv_id IS NULL),
            'unreviewed_skipped', count(*) FILTER (WHERE x.eligible AND x.rv_id IS NULL AND x.skipped),
            'failed', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'failed'),
            'complete', jsonb_build_object(
                'total', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete'),
                'ideal', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'ideal'),
                'ok', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'ok'),
                'issues', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'issues'))) FROM jd2 x),
        'excluded', (SELECT jsonb_build_object(
            'cancelled', count(*) FILTER (WHERE x.status = 'cancelled'),
            'in_flight', count(*) FILTER (WHERE x.status NOT IN ('completed', 'failed', 'cancelled')),
            'skip_origin', count(*) FILTER (WHERE x.skip_expr IS TRUE),
            'reviewed_excluded', count(*) FILTER (WHERE x.skip_expr IS TRUE AND x.rv_id IS NOT NULL)) FROM jd2 x),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM jd_cells c), '[]'::jsonb))
)::jsonb;

-- name: AnalyticsAllQuality :one
-- Issue #4730: the Quality document, one jsonb value: PR rework groups, CI-fix outcomes,
-- cross-check outcomes by stage and judge review coverage. The owner and all-scope statements
-- are kept textually identical except the scope lines, each tagged with a trailing marker
-- comment (a Go test drops them and fails on any other drift). Every range comes from the
-- parameters (half-open: start <= ts < end); there is no calendar arithmetic and no clock read
-- here. The document carries counts only, so it does not depend on the session TimeZone.
-- Every array is built with jsonb_agg(... ORDER BY idx),
-- one entry per bucket (empty buckets included, zero-based index); every class map has a fixed
-- key list, and an absent class is zero.
WITH
bk AS (
    SELECT t.ord::int AS idx, t.s AS b_start, t.e AS b_end
    FROM ROWS FROM (unnest(@bucket_starts::timestamptz[]), unnest(@bucket_ends::timestamptz[])) WITH ORDINALITY AS t(s, e, ord)
),
-- Rework: seed PRs (creation cohort collapsed to one row per PR BEFORE any join), the scoped
-- merge witness, the retained mr_rework runs on the selected PRs and their verified links.
rw_seed AS MATERIALIZED (
    SELECT r.repo_id, r.mr_iid, min(r.created_at) AS first_at
    FROM runs r
    WHERE r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@kinds::text[])
      AND r.kind = ANY(@delivery_kinds::text[])
      AND r.repo_id IS NOT NULL AND r.mr_iid IS NOT NULL
    GROUP BY r.repo_id, r.mr_iid
),
rw_witness AS MATERIALIZED (
    SELECT DISTINCT w.repo_id, w.mr_iid
    FROM runs w
    WHERE w.mr_state = 'merged' AND w.mr_iid IS NOT NULL AND w.repo_id IS NOT NULL
),
-- The witness is folded in by one GROUP BY over a UNION ALL of the seed and the witness keys
-- (a PR is witnessed when any witness row carries its key; HAVING keeps only seeded PRs). A
-- membership subquery here would become an unhashed per-row SubPlan once the witness set
-- outgrew hash_mem.
rw_pr AS MATERIALIZED (
    SELECT p.repo_id, p.mr_iid, p.witnessed,
           CASE WHEN p.wb >= 1 AND p.first_at < (@bucket_ends::timestamptz[])[p.wb] THEN p.wb END AS bidx
    FROM (
        SELECT u.repo_id, u.mr_iid, min(u.first_at) AS first_at, bool_or(u.w) AS witnessed,
               width_bucket(min(u.first_at), @bucket_starts::timestamptz[]) AS wb
        FROM (
            SELECT s.repo_id, s.mr_iid, s.first_at, false AS w FROM rw_seed s
            UNION ALL
            SELECT w.repo_id, w.mr_iid, NULL::timestamptz, true FROM rw_witness w
        ) u
        GROUP BY u.repo_id, u.mr_iid
        HAVING bool_or(NOT u.w)
    ) p
),
rw_sel AS MATERIALIZED (
    SELECT p.repo_id, p.mr_iid, p.bidx FROM rw_pr p WHERE p.witnessed
),
rework AS MATERIALIZED (
    SELECT id, repo_id, mr_iid, target_run_id, user_id
    FROM runs
    WHERE kind = 'mr_rework' AND created_at < @now::timestamptz
),
rw_link AS MATERIALIZED (
    SELECT rw.repo_id, rw.mr_iid, rw.id AS rework_id,
           (t.repo_id = rw.repo_id AND t.mr_iid = rw.mr_iid AND t.user_id = rw.user_id) IS TRUE AS verified
    FROM rework rw
    LEFT JOIN runs t ON t.id = rw.target_run_id
    -- The IS TRUE wrapper prevents semi-join pull-up; PostgreSQL hashes the SubPlan only when it fits the hash memory budget.
    WHERE ((rw.repo_id, rw.mr_iid) IN (SELECT q.repo_id, q.mr_iid FROM rw_sel q)) IS TRUE
),
rw_cnt AS MATERIALIZED (
    SELECT u.repo_id, u.mr_iid, max(u.bidx) AS bidx,
           CASE WHEN sum(u.v) = 0 THEN 'zero'
                WHEN sum(u.v) = 1 THEN 'one'
                WHEN sum(u.v) = 2 THEN 'two'
                ELSE 'three_plus' END AS grp
    FROM (
        SELECT repo_id, mr_iid, bidx, 0 AS v FROM rw_sel
        UNION ALL
        SELECT repo_id, mr_iid, NULL::int, CASE WHEN verified THEN 1 ELSE 0 END FROM rw_link
    ) u
    GROUP BY u.repo_id, u.mr_iid
),
rw_key AS MATERIALIZED (
    SELECT s.repo_id, s.mr_iid, (fc.id IS NOT NULL) AS detectable,
           fc.forge_type, lower(rtrim(fc.base_url, '/')) AS base_key, rp.forge_project_id
    FROM rw_sel s
    JOIN repos rp ON rp.id = s.repo_id
    LEFT JOIN forge_connections fc ON fc.id = rp.connection_id
),
rw_shared AS (
    SELECT k.repo_id, k.mr_iid
    FROM (
        SELECT y.repo_id, y.mr_iid,
               count(*) OVER (PARTITION BY y.forge_type, y.base_key, y.forge_project_id, y.mr_iid) AS n
        FROM rw_key y WHERE y.detectable
    ) k
    WHERE k.n >= 2
),
rw_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'prs', count(c.grp),
            'groups', jsonb_build_object('zero', count(c.grp) FILTER (WHERE c.grp = 'zero'), 'one', count(c.grp) FILTER (WHERE c.grp = 'one'), 'two', count(c.grp) FILTER (WHERE c.grp = 'two'), 'three_plus', count(c.grp) FILTER (WHERE c.grp = 'three_plus'))) AS j
    FROM bk LEFT JOIN rw_cnt c ON c.bidx = bk.idx
    GROUP BY bk.idx
),
-- CI fix: runs created in the window, their outcome class from run_outcome_classes.
cf AS MATERIALIZED (
    SELECT r.id, r.created_at, o.outcome,
           width_bucket(r.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM runs r
    LEFT JOIN LATERAL (
        SELECT oc.outcome FROM run_outcome_classes oc WHERE oc.run_id = r.id OFFSET 0
    ) o ON true
    WHERE r.kind = 'ci_fix' AND r.kind = ANY(@kinds::text[])
      AND r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
),
cfb AS (
    SELECT f.*,
           CASE WHEN f.wb >= 1 AND f.created_at < (@bucket_ends::timestamptz[])[f.wb] THEN f.wb END AS bidx
    FROM cf f
),
cf_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'runs', count(x.id),
            'finished', count(x.id) FILTER (WHERE x.outcome IS NOT NULL),
            'in_flight', count(x.id) FILTER (WHERE x.outcome IS NULL),
            'outcomes', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.outcome = 'completed'), 'cancelled', count(x.id) FILTER (WHERE x.outcome = 'cancelled'), 'plan_rejected', count(x.id) FILTER (WHERE x.outcome = 'plan_rejected'), 'base_diverged', count(x.id) FILTER (WHERE x.outcome = 'base_diverged'), 'failed', count(x.id) FILTER (WHERE x.outcome = 'failed'))) AS j
    FROM bk LEFT JOIN cfb x ON x.bidx = bk.idx
    GROUP BY bk.idx
),
-- Cross-check: attempts selected through the lead run (lead repo, harness and kind), with no
-- creation-time restriction. Attempts whose stage cannot apply to the lead kind (plan_lead_kinds /
-- code_lead_kinds) are excluded under every filter. Each remaining non-pending attempt is either
-- dated (decided_at and created_at finite, decided_at >= created_at) or undated; a dated attempt
-- decided outside [from, now), including a future decided_at, is outside the window and not
-- counted. Stage selects the stored result field: plan reads verdict, code reads
-- outcome (the legacy code verdict column is inert).
cc_base AS MATERIALIZED (
    SELECT cc.id, cc.stage,
           CASE WHEN cc.stage = 'code' THEN cc.outcome ELSE cc.verdict END::text AS result,
           cc.reason_class, cc.interrupted_at, cc.decided_at, cc.created_at, cc.findings,
           CASE WHEN cc.checker_harness IN ('claude', 'codex') THEN cc.checker_harness ELSE 'unknown' END::text AS checker,
           (((cc.stage = 'plan' AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending'))
             AND cc.interrupted_at IS NULL) AS is_pending
    FROM cross_checks cc
    JOIN runs lead ON lead.id = cc.lead_run_id
    WHERE (sqlc.narg('repo_id')::uuid IS NULL OR lead.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR lead.harness = sqlc.narg('harness')::text)
      AND lead.kind = ANY(@kinds::text[]) AND lead.kind <> 'cross_check'
      AND ((cc.stage = 'plan' AND lead.kind = ANY(@plan_lead_kinds::text[]))
        OR (cc.stage = 'code' AND lead.kind = ANY(@code_lead_kinds::text[])))
),
cc_dec AS MATERIALIZED (
    SELECT b.*,
           (b.decided_at IS NOT NULL AND isfinite(b.decided_at) AND isfinite(b.created_at)
             AND b.decided_at >= b.created_at) AS dated
    FROM cc_base b
    WHERE NOT b.is_pending
),
cc_win AS MATERIALIZED (
    SELECT cc.id, cc.stage, cc.result, cc.reason_class, cc.interrupted_at, cc.decided_at, cc.checker,
           CASE WHEN cc.stage = 'code' AND jsonb_typeof(cc.findings) = 'array' AND octet_length(cc.findings::text) <= 65536
             THEN jsonb_array_length(cc.findings)
            WHEN cc.stage = 'plan' AND jsonb_typeof(cc.findings->'items') = 'array'
             AND octet_length(cc.findings::text) <= 65536 THEN jsonb_array_length(cc.findings->'items')
            ELSE -1 END::integer AS findings_count,
           width_bucket(cc.decided_at, @bucket_starts::timestamptz[]) AS wb
    FROM cc_dec cc
    WHERE cc.dated AND cc.decided_at >= sqlc.arg('from')::timestamptz AND cc.decided_at < @now::timestamptz
),
cc_cls AS MATERIALIZED (
    SELECT w.*,
           CASE WHEN w.wb >= 1 AND w.decided_at < (@bucket_ends::timestamptz[])[w.wb] THEN w.wb END AS bidx
    FROM (
        SELECT *, CASE
         -- A Plan verdict recorded before the lead lost custody keeps its verdict (#4037).
         -- IS NOT TRUE keeps any NULL term (e.g. a missing reason_class) replaced.
         WHEN reason_class IN ('superseded','approved_not_stored')
          OR (interrupted_at IS NOT NULL AND (stage='plan' AND decided_at IS NOT NULL AND decided_at <= interrupted_at
           AND result IN ('approve','revise','block') AND reason_class = result) IS NOT TRUE) THEN 'replaced'
         WHEN result='failed' THEN 'didnt_finish'
         WHEN stage='plan' AND result IN ('revise','block') AND reason_class=result THEN 'raised_issues'
         WHEN stage='plan' AND result='approve' AND reason_class='approve' THEN 'passed'
         WHEN stage='code' AND result='completed' AND findings_count > 0 THEN 'raised_issues'
         WHEN stage='code' AND result='completed' AND findings_count = 0 THEN 'passed'
         ELSE '' END::text AS category
        FROM cc_win
    ) w
),
cc_plan_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'decided', count(x.id),
            'stored_result', jsonb_build_object('approve', count(x.id) FILTER (WHERE x.result = 'approve'), 'revise', count(x.id) FILTER (WHERE x.result = 'revise'), 'block', count(x.id) FILTER (WHERE x.result = 'block'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
            'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = ''))) AS j
    FROM bk LEFT JOIN cc_cls x ON x.stage = 'plan' AND x.bidx = bk.idx
    GROUP BY bk.idx
),
cc_plan AS (
    SELECT jsonb_build_object(
        'decided_in_window', count(x.id),
        'stored_result', jsonb_build_object('approve', count(x.id) FILTER (WHERE x.result = 'approve'), 'revise', count(x.id) FILTER (WHERE x.result = 'revise'), 'block', count(x.id) FILTER (WHERE x.result = 'block'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
        'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = '')),
        'checker_harness', jsonb_build_object('claude', count(x.id) FILTER (WHERE x.checker = 'claude'), 'codex', count(x.id) FILTER (WHERE x.checker = 'codex'), 'unknown', count(x.id) FILTER (WHERE x.checker = 'unknown')),
        'undated', (SELECT jsonb_build_object('count', count(d.id), 'stored_result', jsonb_build_object('approve', count(d.id) FILTER (WHERE d.result = 'approve'), 'revise', count(d.id) FILTER (WHERE d.result = 'revise'), 'block', count(d.id) FILTER (WHERE d.result = 'block'), 'failed', count(d.id) FILTER (WHERE d.result = 'failed')))
                    FROM cc_dec d WHERE d.stage = 'plan' AND NOT d.dated),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', g.idx - 1) || g.j ORDER BY g.idx)
                             FROM cc_plan_cells g), '[]'::jsonb)) AS j
    FROM cc_cls x WHERE x.stage = 'plan'
),
cc_code_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'decided', count(x.id),
            'stored_result', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.result = 'completed'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
            'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = ''))) AS j
    FROM bk LEFT JOIN cc_cls x ON x.stage = 'code' AND x.bidx = bk.idx
    GROUP BY bk.idx
),
cc_code AS (
    SELECT jsonb_build_object(
        'decided_in_window', count(x.id),
        'stored_result', jsonb_build_object('completed', count(x.id) FILTER (WHERE x.result = 'completed'), 'failed', count(x.id) FILTER (WHERE x.result = 'failed')),
        'classes', jsonb_build_object('passed', count(x.id) FILTER (WHERE x.category = 'passed'), 'raised_issues', count(x.id) FILTER (WHERE x.category = 'raised_issues'), 'didnt_finish', count(x.id) FILTER (WHERE x.category = 'didnt_finish'), 'replaced', count(x.id) FILTER (WHERE x.category = 'replaced'), 'unclassified', count(x.id) FILTER (WHERE x.category = '')),
        'checker_harness', jsonb_build_object('claude', count(x.id) FILTER (WHERE x.checker = 'claude'), 'codex', count(x.id) FILTER (WHERE x.checker = 'codex'), 'unknown', count(x.id) FILTER (WHERE x.checker = 'unknown')),
        'undated', (SELECT jsonb_build_object('count', count(d.id), 'stored_result', jsonb_build_object('completed', count(d.id) FILTER (WHERE d.result = 'completed'), 'failed', count(d.id) FILTER (WHERE d.result = 'failed')))
                    FROM cc_dec d WHERE d.stage = 'code' AND NOT d.dated),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', g.idx - 1) || g.j ORDER BY g.idx)
                             FROM cc_code_cells g), '[]'::jsonb)) AS j
    FROM cc_cls x WHERE x.stage = 'code'
),
cc_pending AS (
    SELECT jsonb_build_object(
            'total', count(p.id),
            'plan', count(p.id) FILTER (WHERE p.stage = 'plan'),
            'code', count(p.id) FILTER (WHERE p.stage = 'code')) AS j
    FROM cc_base p WHERE p.is_pending
),
-- Judge: the target cohort by creation time, the stable skip rules by failure origin, and the
-- retained review (one row per target; a rejudge overwrites it in place).
jd AS MATERIALIZED (
    SELECT r.id, r.status, r.created_at,
           (r.status = 'failed' AND (r.fail_origin = ANY(@always_skip_origins::text[])
             OR (r.iteration_count = 0 AND r.fail_origin = ANY(@prestart_origins::text[])))) AS skip_expr,
           rv.id AS rv_id, rv.status AS rv_status, rv.verdict AS rv_verdict,
           EXISTS (SELECT 1 FROM judge_skips js WHERE js.target_run_id = r.id) AS skipped,
           width_bucket(r.created_at, @bucket_starts::timestamptz[]) AS wb
    FROM runs r
    LEFT JOIN run_reviews rv ON rv.target_run_id = r.id AND rv.user_id = r.user_id
    WHERE r.created_at >= sqlc.arg('from')::timestamptz AND r.created_at < @now::timestamptz
      AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id')::uuid)
      AND (sqlc.narg('harness')::text IS NULL OR r.harness = sqlc.narg('harness')::text)
      AND r.kind = ANY(@judge_kinds::text[])
      AND r.kind = ANY(@kinds::text[])
),
jd2 AS MATERIALIZED (
    SELECT j.*,
           (j.status IN ('completed', 'failed') AND j.skip_expr IS NOT TRUE) AS eligible,
           CASE WHEN j.wb >= 1 AND j.created_at < (@bucket_ends::timestamptz[])[j.wb] THEN j.wb END AS bidx
    FROM jd j
),
jd_cells AS (
    SELECT bk.idx, jsonb_build_object(
            'eligible', count(x.id) FILTER (WHERE x.eligible),
            'complete', count(x.id) FILTER (WHERE x.eligible AND x.rv_status = 'complete')) AS j
    FROM bk LEFT JOIN jd2 x ON x.bidx = bk.idx
    GROUP BY bk.idx
)
SELECT jsonb_build_object(
    'rework', jsonb_build_object(
        'applicable_prs', (SELECT count(*) FROM rw_sel),
        'groups', (SELECT jsonb_build_object('zero', count(*) FILTER (WHERE c.grp = 'zero'), 'one', count(*) FILTER (WHERE c.grp = 'one'), 'two', count(*) FILTER (WHERE c.grp = 'two'), 'three_plus', count(*) FILTER (WHERE c.grp = 'three_plus')) FROM rw_cnt c),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM rw_cells c), '[]'::jsonb),
        'unwitnessed_prs', (SELECT count(*) FROM rw_pr p WHERE NOT p.witnessed),
        'unverified_links', (SELECT count(DISTINCT l.rework_id) FROM rw_link l WHERE NOT l.verified),
        'shared_external_prs', (SELECT count(*) FROM rw_shared),
        'shared_detection_unavailable', (SELECT count(*) FROM rw_key k WHERE NOT k.detectable)),
    'ci_fix', jsonb_build_object(
        'runs', (SELECT count(*) FROM cfb),
        'finished', (SELECT count(*) FROM cfb x WHERE x.outcome IS NOT NULL),
        'in_flight', (SELECT count(*) FROM cfb x WHERE x.outcome IS NULL),
        'outcomes', (SELECT jsonb_build_object('completed', count(*) FILTER (WHERE x.outcome = 'completed'), 'cancelled', count(*) FILTER (WHERE x.outcome = 'cancelled'), 'plan_rejected', count(*) FILTER (WHERE x.outcome = 'plan_rejected'), 'base_diverged', count(*) FILTER (WHERE x.outcome = 'base_diverged'), 'failed', count(*) FILTER (WHERE x.outcome = 'failed')) FROM cfb x),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM cf_cells c), '[]'::jsonb)),
    'cross_check', jsonb_build_object(
        'plan', (SELECT j FROM cc_plan),
        'code', (SELECT j FROM cc_code),
        'pending', (SELECT j FROM cc_pending)),
    'judge', jsonb_build_object(
        'candidates', (SELECT jsonb_build_object('total', count(*) FILTER (WHERE x.status IN ('completed', 'failed')), 'completed', count(*) FILTER (WHERE x.status = 'completed'), 'failed', count(*) FILTER (WHERE x.status = 'failed')) FROM jd2 x),
        'eligible', (SELECT jsonb_build_object(
            'total', count(*) FILTER (WHERE x.eligible),
            'unreviewed', count(*) FILTER (WHERE x.eligible AND x.rv_id IS NULL),
            'unreviewed_skipped', count(*) FILTER (WHERE x.eligible AND x.rv_id IS NULL AND x.skipped),
            'failed', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'failed'),
            'complete', jsonb_build_object(
                'total', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete'),
                'ideal', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'ideal'),
                'ok', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'ok'),
                'issues', count(*) FILTER (WHERE x.eligible AND x.rv_status = 'complete' AND x.rv_verdict = 'issues'))) FROM jd2 x),
        'excluded', (SELECT jsonb_build_object(
            'cancelled', count(*) FILTER (WHERE x.status = 'cancelled'),
            'in_flight', count(*) FILTER (WHERE x.status NOT IN ('completed', 'failed', 'cancelled')),
            'skip_origin', count(*) FILTER (WHERE x.skip_expr IS TRUE),
            'reviewed_excluded', count(*) FILTER (WHERE x.skip_expr IS TRUE AND x.rv_id IS NOT NULL)) FROM jd2 x),
        'buckets', COALESCE((SELECT jsonb_agg(jsonb_build_object('index', c.idx - 1) || c.j ORDER BY c.idx) FROM jd_cells c), '[]'::jsonb))
)::jsonb;
