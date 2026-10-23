-- name: PublicationContextSnapshot :one
-- One READ COMMITTED statement observes every root and every authority field again
-- after the forge barrier. Inspect at most 4096 unique associations; overflow is unknown.
-- Bound each discovery arm before deduplication, and never walk a truncated inventory.
-- Count distinct roots independently of this work budget; return at most two roots.
-- UNION deduplicates shared ancestors at each depth; no authority JSON rides the walk.
WITH RECURSIVE
request AS (
 SELECT r0.* FROM runs r0 WHERE r0.id = @run_id AND r0.user_id = @user_id
),
ci_branch_ids AS MATERIALIZED (
 SELECT a.id FROM request r JOIN LATERAL (
  SELECT s.id FROM runs s WHERE r.kind = 'ci_fix'
   AND s.user_id = r.user_id AND s.repo_id = r.repo_id AND s.branch = r.pipeline_ref
  LIMIT 4097
 ) a ON true
),
ci_rework_ids AS MATERIALIZED (
 SELECT a.id FROM request r JOIN LATERAL (
  SELECT s.id FROM runs s WHERE r.kind = 'ci_fix' AND s.id <> r.id
   AND s.user_id = r.user_id AND s.repo_id = r.repo_id
   AND s.kind = 'mr_rework' AND s.pipeline_ref = r.pipeline_ref
  LIMIT 4097
 ) a ON true
),
association_ids AS MATERIALIZED (
 SELECT id FROM ci_branch_ids UNION SELECT id FROM ci_rework_ids LIMIT 4097
),
association_budget AS (
 SELECT count(*) > 4096 AS overflow FROM association_ids
),
seeds AS (
 SELECT s.id FROM request r JOIN runs s ON r.kind = 'mr_rework' AND s.id = r.target_run_id
 UNION ALL
 SELECT i.id FROM association_ids i JOIN runs s ON s.id = i.id
 CROSS JOIN association_budget b CROSS JOIN request r
 WHERE NOT b.overflow AND s.id <> r.id AND s.kind IN ('issue','prompt','self_improve','mr_rework')
),
walk AS (
 SELECT s.id, 0 AS depth FROM seeds s
 UNION
 SELECT n.id, w.depth + 1
 FROM walk w JOIN runs p ON p.id = w.id
 JOIN runs n ON n.id = p.target_run_id
 WHERE p.kind = 'mr_rework' AND w.depth < 16
),
summary AS (
 SELECT
 COALESCE(bool_or(w.depth >= 16 OR (a.kind = 'mr_rework' AND
  (a.pipeline_ref IS DISTINCT FROM r.pipeline_ref OR
   (a.branch IS NOT NULL AND a.branch IS DISTINCT FROM r.pipeline_ref) OR
   n.id IS NULL OR w.depth = 15))), false) AS ancestry_unknown,
 COALESCE(bool_or(a.user_id IS DISTINCT FROM r.user_id OR a.repo_id IS DISTINCT FROM r.repo_id OR
  a.mr_iid IS NULL OR a.mr_iid <= 0 OR a.mr_iid > 9007199254740991 OR
  (a.kind <> 'mr_rework' AND a.branch IS DISTINCT FROM r.pipeline_ref) OR
  (a.kind = 'mr_rework' AND n.id IS NOT NULL AND a.mr_iid IS DISTINCT FROM n.mr_iid)), false) AS association_unknown
 FROM walk w JOIN runs a ON a.id = w.id CROSS JOIN request r
 LEFT JOIN runs n ON n.id = a.target_run_id
),
roots AS (
 SELECT DISTINCT a.id FROM walk w JOIN runs a ON a.id = w.id
 WHERE a.kind <> 'mr_rework' ORDER BY a.id LIMIT 2
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
 'nodes', COALESCE((SELECT jsonb_agg(jsonb_build_object('run', to_jsonb(a), 'seed', a.id, 'depth', 0, 'cycle', false)
 ORDER BY a.id) FROM roots z JOIN authority a ON a.id = z.id), '[]'::jsonb),
 'ancestry_unknown', (SELECT ancestry_unknown FROM summary),
 'association_unknown', (SELECT overflow FROM association_budget) OR (SELECT association_unknown FROM summary),
 'ci_mr_conflict', EXISTS (
 SELECT 1 FROM association_ids i JOIN runs x ON x.id = i.id
 WHERE r.kind = 'ci_fix' AND x.branch = r.pipeline_ref AND x.mr_iid IS NOT NULL
 AND x.mr_iid IS DISTINCT FROM (SELECT a.mr_iid FROM roots z JOIN runs a ON a.id = z.id ORDER BY a.id LIMIT 1)
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
