-- name: ListUserCrossCheckPins :many
-- Read both cells, pins and worker defaults in one statement snapshot.
SELECT cells.stage, cells.harness, p.model, p.effort,
    u.default_claude_model, u.default_codex_model, u.default_effort, u.default_codex_effort,
    template.model AS template_model
FROM users u CROSS JOIN (SELECT 'plan'::text AS stage, 'claude'::text AS harness UNION ALL SELECT 'plan', 'codex') cells
LEFT JOIN user_cross_check_pins p ON p.user_id = u.id AND p.stage = cells.stage AND p.harness = cells.harness
LEFT JOIN LATERAL (
    SELECT t.model FROM agent_templates t
    LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = u.id
    LEFT JOIN agent_template_allocations g ON g.template_id = t.id AND g.user_id IS NULL
    WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = u.id))
      AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
      AND NOT (t.scope = 'user' AND EXISTS (
          SELECT 1 FROM agent_templates s WHERE s.scope <> 'user' AND s.name = t.name))
      AND lower(t.name) IN ('lead', 'orchestrator')
    ORDER BY t.name LIMIT 1
) template ON true
WHERE u.id = @user_id
ORDER BY 1, 2;

-- name: PatchUserCrossCheckPin :exec
INSERT INTO user_cross_check_pins (user_id, stage, harness, model, effort)
VALUES (@user_id, @stage, @harness,
    CASE WHEN @set_model::boolean THEN sqlc.narg('model')::text ELSE NULL END,
    CASE WHEN @set_effort::boolean THEN sqlc.narg('effort')::text ELSE NULL END)
ON CONFLICT (user_id, stage, harness) DO UPDATE SET
    model = CASE WHEN @set_model::boolean THEN EXCLUDED.model ELSE user_cross_check_pins.model END,
    effort = CASE WHEN @set_effort::boolean THEN EXCLUDED.effort ELSE user_cross_check_pins.effort END;

-- name: GetCrossCheckPinSnapshot :one
-- Returns BOTH families' worker-default lanes so the caller picks the CHECKER's family
-- (PRD #2460): the Claude lanes for a claude checker, the Codex lanes for a codex checker,
-- exactly the columns each family's claim assembly reads. A CASE here would lose the
-- columns' nullability in sqlc's typing. template_model is the first delivered
-- lead/orchestrator template's model, the same lateral ListUserCrossCheckPins uses, so the
-- Default cell Settings shows is what the checker receives.
SELECT cc.stage, cc.checker_harness, p.model, p.effort,
    u.default_claude_model, u.default_codex_model, u.default_effort, u.default_codex_effort,
    template.model AS template_model
FROM cross_checks cc JOIN runs child ON child.id = cc.checker_run_id
JOIN users u ON u.id = child.user_id
LEFT JOIN user_cross_check_pins p ON p.user_id = child.user_id AND p.stage = cc.stage AND p.harness = cc.checker_harness
LEFT JOIN LATERAL (
    SELECT t.model FROM agent_templates t
    LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = u.id
    LEFT JOIN agent_template_allocations g ON g.template_id = t.id AND g.user_id IS NULL
    WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = u.id))
      AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
      AND NOT (t.scope = 'user' AND EXISTS (
          SELECT 1 FROM agent_templates s WHERE s.scope <> 'user' AND s.name = t.name))
      AND lower(t.name) IN ('lead', 'orchestrator')
    ORDER BY t.name LIMIT 1
) template ON true
WHERE child.id = @child_id AND child.user_id = @user_id
  AND child.worker_id = @worker_id AND child.claim_generation = @claim_generation
  AND child.status = 'claimed' AND child.claim_released_at IS NULL
  AND cc.verdict = 'pending' AND now() < cc.deadline_at;
