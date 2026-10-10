-- name: GetCodeCrossCheck :one
SELECT * FROM cross_checks WHERE lead_run_id = @lead_run_id AND stage = 'code' FOR UPDATE;

-- name: GetCodeCrossCheckForOwner :one
SELECT cc.* FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
WHERE cc.lead_run_id = @lead_run_id AND lead.user_id = @user_id AND cc.stage = 'code';

-- name: GetCrossCheckByChild :one
SELECT * FROM cross_checks WHERE checker_run_id = @child_id;

-- name: CreateCodeCrossCheckChild :one
INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, priority,
 report_only, budget_wall_seconds, dispatched_at, auto_approve, issue_title,
 issue_description, required_capabilities, trigger_source, worker_id)
SELECT @child_id, lead.user_id, lead.repo_id, 'cross_check', lead.id, @child_harness::text, 2,
 true, @budget_wall_seconds::int, now(), true, lead.issue_title, lead.issue_description,
 COALESCE(repo.required_capabilities, '{}'), 'cross_check', lead.worker_id
FROM runs lead JOIN repos repo ON repo.id = lead.repo_id JOIN workers w ON w.id = lead.worker_id
WHERE lead.id = @lead_run_id AND lead.user_id = @user_id AND lead.worker_id = @worker_id
 AND lead.claim_generation = @claim_generation AND lead.claim_released_at IS NULL
 AND lead.status IN ('claimed','running') AND lead.code_cross_check_required
 AND lead.kind IN ('issue','prompt','self_improve','ci_fix','mr_rework','task')
 AND lead.harness IN ('claude','codex') AND lead.harness <> @child_harness::text
 AND 'cross_check_code_v1' = ANY(w.protocol_capabilities)
 AND 'cross_check_lane_v1' = ANY(w.protocol_capabilities)
 AND w.max_cross_check_slots > 0 AND NOT w.isolated_lane AND NOT w.maintenance_fenced
 AND NOT lead.report_only AND lead.fix_verdict IS DISTINCT FROM 'not_code'
 AND NOT EXISTS (SELECT 1 FROM cross_checks cc WHERE cc.lead_run_id = lead.id AND cc.stage = 'code')
RETURNING *;

-- name: InsertCodeCrossCheck :one
INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, head_commit,
 base_commit, candidate_digest, checker_run_id, checker_harness, deadline_at, verdict,
 outcome, code_context, guidance_snapshot, plan_md, milestones, required_capabilities,
 required_tools, size_class, guidance_text, guidance_digest,
 repo_instructions_enabled, repo_instructions_text, repo_instructions_digest)
SELECT lead.id, 'code', 1, lead.claim_generation, @head_commit::text, @base_commit::text,
 @candidate_digest::bytea, child.id, child.harness, @deadline_at::timestamptz, 'failed',
 'pending', @code_context::jsonb, '', lead.plan_md, lead.milestones_frozen,
 lead.required_capabilities, lead.required_tools, lead.size_class,
 '', sha256(convert_to('', 'UTF8')), false, '', sha256(convert_to('', 'UTF8'))
FROM runs lead JOIN runs child ON child.id = @checker_run_id
WHERE lead.id = @lead_run_id AND child.target_run_id = lead.id AND child.kind = 'cross_check'
 AND child.user_id = lead.user_id AND child.worker_id = lead.worker_id
 AND child.harness <> lead.harness
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM cross_checks cc WHERE cc.lead_run_id = lead.id AND cc.stage = 'code')
RETURNING *;

-- name: LockCodeCrossCheckLeadForVerdict :one
SELECT lead.* FROM runs lead JOIN cross_checks cc ON cc.lead_run_id = lead.id
JOIN runs child ON child.id = cc.checker_run_id
WHERE child.id = @child_id AND child.worker_id = @worker_id
 AND child.claim_generation = @claim_generation AND child.claim_released_at IS NULL
 AND child.kind = 'cross_check' AND child.status IN ('claimed','running')
 AND cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL
 AND now() < cc.deadline_at
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND lead.worker_id = child.worker_id AND lead.claim_generation = cc.lead_claim_generation
FOR UPDATE OF lead, cc;

-- name: DecideCodeCrossCheck :one
UPDATE cross_checks cc SET outcome = @outcome::text, reason_class = sqlc.narg('reason_class')::text,
 findings = @findings::jsonb, decided_at = now()
FROM runs child, runs lead
WHERE child.id = cc.checker_run_id AND lead.id = cc.lead_run_id
 AND child.id = @child_id AND child.worker_id = @worker_id
 AND child.claim_generation = @claim_generation AND child.claim_released_at IS NULL
 AND child.kind = 'cross_check' AND child.status IN ('claimed','running')
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND lead.claim_generation = cc.lead_claim_generation AND lead.worker_id = child.worker_id
 AND cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL
 AND now() < cc.deadline_at
RETURNING cc.*;

-- name: RecordCodeCrossCheckClaim :one
UPDATE cross_checks cc SET checker_model = sqlc.narg('checker_model')::text,
 checker_effort = sqlc.narg('checker_effort')::text,
 checker_model_source = sqlc.narg('checker_model_source')::text,
 checker_effort_source = sqlc.narg('checker_effort_source')::text
FROM runs child, runs lead
WHERE child.id = cc.checker_run_id AND lead.id = cc.lead_run_id
 AND child.id = @child_id AND child.worker_id = @worker_id
 AND child.claim_generation = @claim_generation AND child.claim_released_at IS NULL
 AND child.kind = 'cross_check' AND child.harness = cc.checker_harness
 AND child.status IN ('claimed','running')
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND lead.claim_generation = cc.lead_claim_generation AND lead.worker_id = child.worker_id
 AND lead.harness <> cc.checker_harness
 AND cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL
 AND now() < cc.deadline_at
RETURNING cc.*;

-- name: ExpireCodeCrossCheck :one
WITH expired AS (
 UPDATE cross_checks cc SET outcome = 'failed', reason_class = 'timed_out',
 decided_at = cc.deadline_at
 FROM runs lead WHERE lead.id = cc.lead_run_id AND lead.id = @lead_run_id
 AND lead.worker_id = @worker_id AND lead.claim_generation = @claim_generation
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND cc.lead_claim_generation = lead.claim_generation AND cc.stage = 'code'
 AND cc.outcome = 'pending' AND cc.deadline_at <= now()
 RETURNING cc.*
), cancelled AS (
 UPDATE runs child SET status = 'cancelled', finished_at = now(), updated_at = now(),
 claim_released_at = now() FROM expired cc WHERE child.id = cc.checker_run_id
 AND child.status NOT IN ('completed','failed','cancelled')
)
SELECT * FROM expired;

-- name: BankCodeCrossCheckWait :execrows
WITH credited AS (
 UPDATE cross_checks cc SET wait_credited = true
 WHERE cc.id = @check_id AND cc.stage = 'code' AND cc.outcome <> 'pending'
 AND cc.decided_at IS NOT NULL AND NOT cc.wait_credited RETURNING cc.*
)
UPDATE runs lead SET budget_paused_seconds = lead.budget_paused_seconds +
 GREATEST(0, CEIL(EXTRACT(EPOCH FROM (cc.decided_at - cc.created_at)))::int)
FROM credited cc WHERE lead.id = cc.lead_run_id;

-- name: InsertFailedCodeCrossCheck :one
INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation,
 base_commit, head_commit, candidate_digest, verdict, outcome, reason_class,
 code_context, guidance_snapshot, guidance_text, guidance_digest,
 repo_instructions_text, repo_instructions_digest, deadline_at, decided_at,
 wait_credited, plan_md, milestones, required_capabilities, required_tools, size_class)
SELECT lead.id, 'code', 1, lead.claim_generation,
 sqlc.narg('base_commit')::text, sqlc.narg('head_commit')::text,
 sqlc.narg('candidate_digest')::bytea, 'failed', 'failed', @reason_class::text,
 @code_context::jsonb, '', '', sha256(convert_to('', 'UTF8')),
 '', sha256(convert_to('', 'UTF8')), now(), now(), true,
 CASE WHEN sqlc.narg('head_commit')::text IS NOT NULL THEN lead.plan_md END,
 CASE WHEN sqlc.narg('head_commit')::text IS NOT NULL THEN lead.milestones_frozen END,
 CASE WHEN sqlc.narg('head_commit')::text IS NOT NULL THEN lead.required_capabilities ELSE '{}' END,
 CASE WHEN sqlc.narg('head_commit')::text IS NOT NULL THEN lead.required_tools ELSE '{}' END,
 CASE WHEN sqlc.narg('head_commit')::text IS NOT NULL THEN lead.size_class END
FROM runs lead WHERE lead.id = @lead_run_id AND lead.worker_id = @worker_id
 AND lead.user_id = @user_id AND lead.claim_generation = @claim_generation
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND lead.code_cross_check_required AND NOT lead.report_only
 AND lead.fix_verdict IS DISTINCT FROM 'not_code'
 AND lead.kind IN ('issue','prompt','self_improve','ci_fix','mr_rework','task')
RETURNING *;

-- name: FailDeadCodeCrossCheckChild :one
UPDATE cross_checks cc SET outcome = 'failed', reason_class = 'checker_unavailable',
 decided_at = LEAST(now(), cc.deadline_at)
FROM runs lead, runs child
WHERE lead.id = cc.lead_run_id AND child.id = cc.checker_run_id
 AND lead.id = @lead_run_id AND lead.worker_id = @worker_id
 AND lead.claim_generation = @claim_generation AND lead.claim_released_at IS NULL
 AND lead.status IN ('claimed','running') AND cc.lead_claim_generation = lead.claim_generation
 AND cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL
 AND child.status IN ('completed','failed','cancelled')
RETURNING cc.*;

-- name: FinalizeCodeCrossCheckDispositions :one
UPDATE cross_checks cc SET dispositions = @dispositions::jsonb, finalized_at = now()
FROM runs lead
WHERE lead.id = cc.lead_run_id AND lead.id = @lead_run_id
 AND lead.worker_id = @worker_id AND lead.claim_generation = @claim_generation
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND cc.lead_claim_generation = lead.claim_generation
 AND cc.stage = 'code' AND cc.outcome = 'completed' AND cc.decided_at IS NOT NULL
 AND cc.interrupted_at IS NULL AND cc.finalized_at IS NULL AND cc.dispositions IS NULL
RETURNING cc.*;

-- name: ListCodeCrossChecksToSettle :many
SELECT lead.id, lead.worker_id, lead.claim_generation FROM runs lead
JOIN cross_checks cc ON cc.lead_run_id = lead.id
LEFT JOIN runs child ON child.id = cc.checker_run_id
WHERE cc.stage = 'code' AND cc.outcome = 'pending' AND cc.interrupted_at IS NULL
 AND lead.status IN ('claimed','running') AND lead.claim_released_at IS NULL
 AND lead.claim_generation = cc.lead_claim_generation
 AND (cc.deadline_at <= now() OR child.status IN ('completed','failed','cancelled'))
ORDER BY cc.deadline_at LIMIT 100;
