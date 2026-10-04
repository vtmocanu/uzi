-- name: HasLivePlanCrossCheck :one
SELECT EXISTS (
    SELECT 1 FROM cross_checks cc
    JOIN runs lead ON lead.id = cc.lead_run_id
    WHERE lead.id = @lead_run_id
      AND cc.stage = 'plan' AND cc.round = 1 AND cc.verdict = 'pending'
      AND cc.lead_claim_generation = lead.claim_generation
      AND lead.status IN ('claimed', 'running') AND lead.claim_released_at IS NULL
      AND cc.deadline_at > now()
)::boolean AS has_live_plan_cross_check;

-- name: HasPendingPlanCrossCheck :one
-- Strict settlement proof: an expired or stale pending row still requires settlement.
SELECT EXISTS (
    SELECT 1 FROM cross_checks cc
    WHERE cc.lead_run_id = @lead_run_id AND cc.stage = 'plan' AND cc.verdict = 'pending'
)::boolean AS has_pending_plan_cross_check;
