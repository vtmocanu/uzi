-- name: GetLatestPlanCrossCheckSummary :one
-- Owner detail only; no locks, expiry writes, candidate/diff/context serialization,
-- or approval authority. Bound legacy text before transfer. Child metadata is
-- optional after deletion, and usage is joined only through the same-owner child.
SELECT cc.round, cc.verdict, cc.reason_class,
    cc.findings, cc.checker_run_id, cc.checker_model, cc.checker_effort, cc.checker_model_source, cc.checker_effort_source,
    (checker.id IS NOT NULL)::boolean AS has_child,
    (usage.run_id IS NOT NULL)::boolean AS has_usage,
    COALESCE(usage.input_tokens, 0)::bigint AS input_tokens,
    COALESCE(usage.cache_read_tokens, 0)::bigint AS cache_read_tokens,
    COALESCE(usage.cache_creation_tokens, 0)::bigint AS cache_creation_tokens,
    COALESCE(usage.output_tokens, 0)::bigint AS output_tokens,
    usage.cost_usd, usage.cost_status,
    -- Once a human requests revision, this one-round check is earlier-plan
    -- evidence even when the revised presentation retains identical plan fields.
    (CASE WHEN lead.revise_count > 0 THEN true
     WHEN lead.plan_md IS NULL THEN
        cc.lead_claim_generation <> lead.claim_generation OR lead.gate_revision > 0
     ELSE NOT COALESCE(
        cc.lead_claim_generation = lead.claim_generation
        AND cc.plan_md = lead.plan_md
        AND cc.milestones = CASE WHEN lead.status = 'awaiting_approval'
            THEN lead.milestones_candidate ELSE lead.milestones_frozen END
        AND cc.required_capabilities <@ lead.required_capabilities
        AND lead.required_capabilities <@ cc.required_capabilities
        AND cc.required_tools <@ lead.required_tools
        AND lead.required_tools <@ cc.required_tools
        AND cc.size_class = lead.size_class, false)
     END)::boolean AS historical
FROM cross_checks cc
JOIN runs lead ON lead.id = cc.lead_run_id
LEFT JOIN runs checker ON checker.id = cc.checker_run_id
    AND checker.user_id = lead.user_id AND checker.kind = 'cross_check'
    AND checker.target_run_id = lead.id AND checker.harness = cc.checker_harness
LEFT JOIN run_usage_totals usage ON usage.run_id = checker.id
WHERE lead.id = @lead_run_id AND lead.user_id = @user_id
    AND lead.kind <> 'cross_check' AND cc.stage = 'plan' AND cc.round = (SELECT max(latest.round) FROM cross_checks latest WHERE latest.lead_run_id = cc.lead_run_id AND latest.stage = cc.stage)
    AND (cc.findings IS NULL OR octet_length(cc.findings::text) <= 65536)
    AND (cc.checker_model IS NULL OR octet_length(cc.checker_model) <= 512)
    AND (cc.checker_effort IS NULL OR octet_length(cc.checker_effort) <= 512)
ORDER BY cc.round DESC
LIMIT 1;
