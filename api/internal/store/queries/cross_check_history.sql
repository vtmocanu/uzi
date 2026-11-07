-- name: CrossCheckHistoryAvailability :one
SELECT EXISTS(SELECT 1 FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id
 WHERE lead.user_id = sqlc.arg(owner_id)::uuid AND lead.kind <> 'cross_check')::boolean AS available;

-- name: CrossCheckHistoryGroups :many
WITH evidence AS (
 SELECT cc.lead_run_id, cc.stage, cc.round, cc.created_at, cc.decided_at, cc.deadline_at,
  lead.issue_title, lead.status AS parent_status,
  CASE WHEN cc.stage = 'code' THEN 1 ELSE 0 END::integer AS stage_rank,
  CASE WHEN cc.stage = 'code' THEN cc.outcome ELSE cc.verdict END::text AS result,
  cc.reason_class, cc.interrupted_at,
  checker.id AS child_id, checker.status AS child_status,
  CASE WHEN octet_length(cc.checker_model) <= 512 THEN cc.checker_model ELSE '' END::text AS model,
  CASE WHEN octet_length(cc.checker_effort) <= 512 THEN cc.checker_effort ELSE '' END::text AS effort,
  CASE WHEN cc.stage = 'code' AND jsonb_typeof(cc.findings) = 'array' AND octet_length(cc.findings::text) <= 65536
    THEN jsonb_array_length(cc.findings)
   WHEN cc.stage = 'plan' AND jsonb_typeof(cc.findings->'items') = 'array'
    AND octet_length(cc.findings::text) <= 65536 THEN jsonb_array_length(cc.findings->'items')
   ELSE -1 END::integer AS findings_count,
  ((CASE WHEN cc.stage = 'code' THEN cc.outcome = 'pending' ELSE cc.verdict = 'pending' END
     AND cc.interrupted_at IS NULL)
   OR checker.status NOT IN ('completed','failed','cancelled'))::boolean AS active,
  (cc.interrupted_at IS NOT NULL OR cc.reason_class IS NOT DISTINCT FROM 'approved_not_stored' OR cc.lead_claim_generation <> lead.claim_generation
    OR (cc.stage = 'plan' AND (lead.revise_count > 0
      OR cc.round < (SELECT max(newer.round) FROM cross_checks newer WHERE newer.lead_run_id=cc.lead_run_id AND newer.stage='plan')
      OR CASE WHEN lead.plan_md IS NULL THEN lead.gate_revision > 0 ELSE NOT COALESCE(cc.plan_md = lead.plan_md
       AND (cc.milestones = CASE WHEN lead.status='awaiting_approval' THEN lead.milestones_candidate ELSE lead.milestones_frozen END
          OR (lead.kind <> 'issue' AND CASE WHEN lead.status='awaiting_approval' THEN lead.milestones_candidate ELSE lead.milestones_frozen END IS NULL))
       AND cc.required_capabilities <@ lead.required_capabilities AND lead.required_capabilities <@ cc.required_capabilities
       AND cc.required_tools <@ lead.required_tools AND lead.required_tools <@ cc.required_tools
       AND cc.size_class=lead.size_class, false) END)))::boolean AS historical
 FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id
 LEFT JOIN runs checker ON checker.id=cc.checker_run_id AND checker.user_id=lead.user_id
  AND checker.kind='cross_check' AND checker.target_run_id=lead.id AND checker.harness=cc.checker_harness
 WHERE lead.user_id = sqlc.arg(owner_id)::uuid AND lead.kind <> 'cross_check'
), classified AS (
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
 FROM evidence
), matching AS (
 SELECT *, ((sqlc.arg(stage_filter)::text='all' OR stage= sqlc.arg(stage_filter))
   AND (sqlc.arg(outcome_filter)::text='any' OR category= sqlc.arg(outcome_filter)))::boolean AS matches
 FROM classified
), grouped AS (
 SELECT lead_run_id, max(created_at)::timestamptz AS latest_at,
  COALESCE(bool_or(active),false)::boolean AS active,
  count(*)::bigint AS total_attempts,
  count(*) FILTER (WHERE matches)::bigint AS matching_attempts,
  count(*) FILTER (WHERE active)::bigint AS active_attempts
 FROM matching GROUP BY lead_run_id
)
SELECT grouped.*,
 (CASE WHEN octet_length(lead.issue_title)<=2048 THEN COALESCE(lead.issue_title,'') ELSE '' END)::text AS title,
 lead.status AS parent_status
FROM grouped JOIN runs lead ON lead.id=grouped.lead_run_id
WHERE (cardinality(sqlc.arg(parent_ids)::uuid[]) > 0 AND lead_run_id=ANY(sqlc.arg(parent_ids)::uuid[]))
 OR (cardinality(sqlc.arg(parent_ids)::uuid[])=0
  AND active=(sqlc.arg(lifecycle)::text='active') AND matching_attempts>0
  AND (latest_at,lead_run_id)<(sqlc.arg(before_created)::timestamptz,sqlc.arg(before_id)::uuid))
ORDER BY latest_at DESC,lead_run_id DESC
LIMIT sqlc.arg(group_limit)::integer;

-- name: CrossCheckHistoryAttempts :many
WITH evidence AS (
 SELECT cc.lead_run_id, cc.stage, cc.round, cc.created_at, cc.decided_at, cc.deadline_at,
  lead.issue_title, lead.status AS parent_status,
  CASE WHEN cc.stage = 'code' THEN 1 ELSE 0 END::integer AS stage_rank,
  CASE WHEN cc.stage = 'code' THEN cc.outcome ELSE cc.verdict END::text AS result,
  cc.reason_class, cc.interrupted_at,
  checker.id AS child_id, checker.status AS child_status,
  CASE WHEN octet_length(cc.checker_model) <= 512 THEN cc.checker_model ELSE '' END::text AS model,
  CASE WHEN octet_length(cc.checker_effort) <= 512 THEN cc.checker_effort ELSE '' END::text AS effort,
  CASE WHEN cc.stage = 'code' AND jsonb_typeof(cc.findings) = 'array' AND octet_length(cc.findings::text) <= 65536
    THEN jsonb_array_length(cc.findings)
   WHEN cc.stage = 'plan' AND jsonb_typeof(cc.findings->'items') = 'array'
    AND octet_length(cc.findings::text) <= 65536 THEN jsonb_array_length(cc.findings->'items')
   ELSE -1 END::integer AS findings_count,
  ((CASE WHEN cc.stage = 'code' THEN cc.outcome = 'pending' ELSE cc.verdict = 'pending' END
     AND cc.interrupted_at IS NULL)
   OR checker.status NOT IN ('completed','failed','cancelled'))::boolean AS active,
  (cc.interrupted_at IS NOT NULL OR cc.reason_class IS NOT DISTINCT FROM 'approved_not_stored' OR cc.lead_claim_generation <> lead.claim_generation
    OR (cc.stage = 'plan' AND (lead.revise_count > 0
      OR cc.round < (SELECT max(newer.round) FROM cross_checks newer WHERE newer.lead_run_id=cc.lead_run_id AND newer.stage='plan')
      OR CASE WHEN lead.plan_md IS NULL THEN lead.gate_revision > 0 ELSE NOT COALESCE(cc.plan_md = lead.plan_md
       AND (cc.milestones = CASE WHEN lead.status='awaiting_approval' THEN lead.milestones_candidate ELSE lead.milestones_frozen END
          OR (lead.kind <> 'issue' AND CASE WHEN lead.status='awaiting_approval' THEN lead.milestones_candidate ELSE lead.milestones_frozen END IS NULL))
       AND cc.required_capabilities <@ lead.required_capabilities AND lead.required_capabilities <@ cc.required_capabilities
       AND cc.required_tools <@ lead.required_tools AND lead.required_tools <@ cc.required_tools
       AND cc.size_class=lead.size_class, false) END)))::boolean AS historical
 FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id
 LEFT JOIN runs checker ON checker.id=cc.checker_run_id AND checker.user_id=lead.user_id
  AND checker.kind='cross_check' AND checker.target_run_id=lead.id AND checker.harness=cc.checker_harness
 WHERE lead.user_id = sqlc.arg(owner_id)::uuid AND lead.kind <> 'cross_check'
), classified AS (
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
 FROM evidence
), matching AS (
 SELECT *, ((sqlc.arg(stage_filter)::text='all' OR stage= sqlc.arg(stage_filter))
   AND (sqlc.arg(outcome_filter)::text='any' OR category= sqlc.arg(outcome_filter)))::boolean AS matches
 FROM classified
), numbered AS (
 SELECT m.lead_run_id,m.stage,m.round,m.stage_rank,m.result,m.category,m.reason_class,m.created_at,m.decided_at,m.interrupted_at,m.deadline_at,m.child_id,m.child_status,m.model,m.effort,m.findings_count,m.active,m.historical, row_number() OVER (PARTITION BY m.lead_run_id ORDER BY m.stage_rank DESC,m.round DESC) AS attempt_position
 FROM matching m
 WHERE m.matches AND m.lead_run_id=ANY(sqlc.arg(parent_ids)::uuid[])
  AND (m.stage_rank::bigint*2147483648+m.round::bigint) <
   (sqlc.arg(before_keys)::bigint[])[array_position(sqlc.arg(parent_ids)::uuid[],m.lead_run_id)]
)
SELECT lead_run_id,stage,round,stage_rank,result,category,
 CASE WHEN reason_class IN ('approve','revise','block','malformed','model_error','model_timeout',
 'checker_unavailable','confinement_failed','timed_out','superseded','approved_not_stored',
 'worker_unsupported','snapshot_failed') THEN reason_class ELSE '' END::text AS reason_class,
 created_at,decided_at,interrupted_at,deadline_at,child_id,child_status,model,effort,findings_count,
 COALESCE(active,false)::boolean AS active,historical
FROM numbered n WHERE n.attempt_position <= sqlc.arg(attempt_limit)::integer
ORDER BY lead_run_id,stage_rank DESC,round DESC;
