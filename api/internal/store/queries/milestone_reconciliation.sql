-- name: LatestRunMilestoneReconciliationSession :many
-- Only the latest audit predecessor controls transcript reuse. Preserve JSON null
-- versus a missing/invalid key so claim assembly can refuse malformed history.
SELECT (before_state->'session_id')::jsonb AS session_id
FROM run_milestone_reconciliations
WHERE run_id = @run_id
ORDER BY gate_revision DESC
LIMIT 1;

-- name: ListRunMilestoneReconciliations :many
-- Read projection only: durable consent and snapshots are never rewritten.
SELECT id, run_id, owner_id, gate_revision, claim_generation,
       left(reason, 256)::text AS reason,
       (length(reason) > 256)::boolean AS reason_truncated,
       CASE WHEN octet_length(consent::text) <= 8192 THEN consent ELSE 'null'::jsonb END AS consent,
       CASE WHEN octet_length(before_state::text) <= 8192 THEN before_state ELSE 'null'::jsonb END AS before_state,
       CASE WHEN octet_length(after_state::text) <= 8192 THEN after_state ELSE 'null'::jsonb END AS after_state,
       (octet_length(consent::text) > 8192 OR octet_length(before_state::text) > 8192
        OR octet_length(after_state::text) > 8192)::boolean AS details_omitted,
       applied_input_id, created_at
FROM run_milestone_reconciliations
WHERE run_id = @run_id AND owner_id = @owner_id
  AND (@before_gate_revision::bigint = 0 OR gate_revision < @before_gate_revision::bigint)
ORDER BY gate_revision DESC
LIMIT 51;
