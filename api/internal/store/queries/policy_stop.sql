-- name: InitializePolicyRevisionSource :one
-- Only createRunAtomic calls this, immediately after a fresh INSERT in its
-- transaction. Closed coverage is server truth, not a candidate approval.
UPDATE runs
SET policy_revision_source = '{"version":1,"gate":null,"primary_revision":null,"associated_cross_checks":[]}'::jsonb
WHERE id = sqlc.arg(run_id)::uuid AND user_id = sqlc.arg(user_id)::uuid
 AND harness = 'codex' AND NOT interactive AND target_run_id IS NULL
 AND review_target_run_id IS NULL AND then_fix_of_run_id IS NULL
 AND kind IN ('issue','task','prompt','self_improve')
 AND status = 'queued' AND claim_generation = 0 AND worker_id IS NULL
 AND claim_released_at IS NULL AND policy_stop_resolution IS NULL
 AND policy_revision_source IS NULL AND policy_refusal_count = 0
 AND gate_revision = 0 AND gate_presentation_id IS NULL
 AND gate_payload_digest IS NULL AND gate_presented_payload IS NULL
RETURNING *;

-- name: SetPolicyRevisionSource :execrows
-- Caller holds the worker/run claim transaction and authenticates finite coverage.
UPDATE runs SET policy_revision_source = sqlc.narg(source)::jsonb
WHERE id = sqlc.arg(run_id)::uuid AND worker_id = sqlc.arg(worker_id)::uuid
 AND claim_generation = sqlc.arg(claim_generation)::bigint AND claim_generation > 0
 AND claim_released_at IS NULL AND status IN ('claimed','running','awaiting_approval')
 AND harness='codex' AND NOT interactive AND target_run_id IS NULL
 AND kind IN ('issue','task','prompt','self_improve') AND policy_stop_resolution IS NULL;

-- name: ListPolicyStopRevisionInputs :many
SELECT * FROM run_user_inputs
WHERE run_id = $1 AND kind = 'revise_plan'
  AND applied_at IS NULL AND settled_at IS NULL
  AND disposition IS DISTINCT FROM 'superseded'
  AND disposition IS DISTINCT FROM 'superseded_by_stop'
ORDER BY id;

-- name: HasPolicyRevisionHistory :one
-- The caller holds the parent run lock. Recorded revision history prevents an
-- unknown source from regaining legacy approval after a plan frame or reclaim.
-- This is negative evidence only; source acceptance remains an explicit writer.
SELECT (EXISTS (
 SELECT 1 FROM run_messages
 WHERE run_id = sqlc.arg(run_id)::uuid AND kind = 'plan_revising'
))::boolean AS has_revision;

-- name: HasPolicyCurrentGateApproval :one
-- The caller authenticates the current source, presentation and candidate, and
-- proves absence of applicable pending revision before using this receipt.
SELECT (EXISTS (
    SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id AND kind = 'approve_plan'
      AND gate_binding = 'bound' AND gate_revision = @gate_revision::bigint
      AND applied_at IS NOT NULL
      AND disposition IS DISTINCT FROM 'superseded'
      AND disposition IS DISTINCT FROM 'superseded_by_stop'
))::boolean AS approved;

-- name: SetPolicyCandidateCrossCheckAssociation :execrows
UPDATE runs
SET policy_candidate_cross_check_id = @cross_check_id::uuid
WHERE id = @lead_run_id::uuid AND worker_id = @worker_id::uuid
  AND claim_generation = @claim_generation::bigint
  AND claim_released_at IS NULL AND policy_stop_resolution IS NULL;

-- name: GetPolicyStopCrossCheck :one
SELECT * FROM cross_checks WHERE id = $1;

-- name: SettlePolicyStopRevisionInput :execrows
-- Caller holds the parent run lock and visits manifest input IDs in ascending order.
UPDATE run_user_inputs
SET disposition = 'superseded_by_stop', settled_at = now(),
    superseded_by_stop_input_id = @scope_input_id::bigint
WHERE run_id = @run_id AND id = @input_id::bigint AND kind = 'revise_plan'
  AND applied_at IS NULL AND included_at IS NULL AND settled_at IS NULL
  AND disposition IS NULL AND superseded_by_stop_input_id IS NULL;

-- name: SettlePolicyStopCrossCheck :execrows
-- Approve association witnesses are deliberately excluded.
UPDATE cross_checks
SET revision_disposition = 'superseded_by_stop', revision_settled_at = now(),
    revision_superseded_by_stop_input_id = @scope_input_id::bigint
WHERE lead_run_id = @run_id AND id = @cross_check_id AND stage = 'plan'
  AND verdict = 'revise' AND decided_at IS NOT NULL
  AND revision_disposition IS NULL AND revision_settled_at IS NULL
  AND revision_superseded_by_stop_input_id IS NULL;

-- name: AcceptPolicyStopResolution :one
-- Source evidence and candidate approval are deliberately not updated.
UPDATE runs SET policy_stop_resolution = @resolution::jsonb, updated_at = now()
WHERE id = @run_id AND user_id = @owner_id
  AND status = 'paused' AND kind = 'issue' AND harness = 'codex' AND NOT interactive
  AND policy_last_outcome_id = @outcome_id
  AND policy_last_source_generation = @source_generation::bigint
  AND claim_generation = @claim_generation::bigint AND claim_generation > 0
  AND policy_last_source_generation > 0
  AND policy_stop_resolution IS NULL
RETURNING *;
