-- name: PublicationContextSnapshot :one
-- One READ COMMITTED statement observes every root and every authority field again
-- after the forge barrier. Enumeration is at most 65 seeds times 17 ancestry rows.
WITH RECURSIVE
request AS (
 SELECT r0.* FROM runs r0 WHERE r0.id = @run_id AND r0.user_id = @user_id
),
seeds AS (
 SELECT a.id FROM request r
 JOIN LATERAL (
  SELECT s.id FROM runs s
  WHERE (r.kind = 'mr_rework' AND s.id = r.target_run_id)
   OR (r.kind = 'ci_fix' AND s.id <> r.id AND s.user_id = r.user_id AND s.repo_id = r.repo_id
    AND s.kind IN ('issue','prompt','self_improve','mr_rework')
    AND (s.branch = r.pipeline_ref OR (s.kind = 'mr_rework' AND s.pipeline_ref = r.pipeline_ref)))
  ORDER BY s.id LIMIT 65
 ) a ON true
),
walk AS (
 SELECT s.id, s.id AS seed, 0 AS depth, ARRAY[s.id] AS path, false AS cycle FROM seeds s
 UNION ALL
 SELECT n.id, w.seed, w.depth + 1, w.path || n.id, n.id = ANY(w.path)
 FROM walk w JOIN runs p ON p.id = w.id
 JOIN runs n ON n.id = p.target_run_id
 WHERE p.kind = 'mr_rework' AND w.depth < 16 AND NOT w.cycle
),
authority AS NOT MATERIALIZED (
 SELECT id, user_id, repo_id, worker_id, kind, status, claim_generation, claim_released_at,
 branch, pipeline_ref, mr_iid, target_run_id, issue_iid, report_only, stop_kind,
 completion_non_closing_reason, completion_contract_version, contract_revision,
 CASE WHEN octet_length(completion_contract::text) <= 1048576 THEN completion_contract ELSE 'null'::jsonb END AS completion_contract,
 CASE WHEN octet_length(milestones_frozen::text) <= 131072 THEN milestones_frozen ELSE 'null'::jsonb END AS milestones_frozen,
 CASE WHEN octet_length(milestones_completed::text) <= 131072 THEN milestones_completed ELSE 'null'::jsonb END AS milestones_completed,
 source_publication_intent
 FROM runs
)
SELECT jsonb_build_object(
 'request', (SELECT to_jsonb(a) FROM authority a WHERE a.id = r.id),
 'connection_id', c.id, 'project_id', p.forge_project_id, 'forge_type', c.forge_type,
 'base_url', c.base_url, 'token_ciphertext', encode(c.token_ciphertext, 'base64'),
 'nodes', COALESCE((SELECT jsonb_agg(jsonb_build_object('run', to_jsonb(a), 'seed', w.seed, 'depth', w.depth, 'cycle', w.cycle)
 ORDER BY w.seed, w.depth) FROM walk w JOIN authority a ON a.id = w.id), '[]'::jsonb),
 'ci_mr_conflict', EXISTS (
 SELECT 1 FROM runs x WHERE r.kind = 'ci_fix' AND x.user_id = r.user_id AND x.repo_id = r.repo_id
 AND x.branch = r.pipeline_ref AND x.mr_iid IS NOT NULL
 AND x.mr_iid IS DISTINCT FROM (SELECT a.mr_iid FROM seeds s JOIN runs a ON a.id = s.id ORDER BY s.id LIMIT 1)
 )
)::jsonb AS snapshot
FROM request r LEFT JOIN repos p ON p.id = r.repo_id
LEFT JOIN forge_connections c ON c.id = p.connection_id AND c.user_id = r.user_id;

-- name: BindSourcePublicationIntent :execrows
UPDATE runs SET source_publication_intent = @intent::jsonb
WHERE id = @id AND user_id = @user_id AND worker_id = @worker_id
 AND claim_generation = @generation AND claim_generation > 0 AND claim_released_at IS NULL
 AND kind = 'issue' AND status = 'completed' AND NOT report_only
 AND branch = @branch AND mr_iid = @mr_iid AND source_publication_intent IS NULL;
