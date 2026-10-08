-- name: ReadWorkerExhaustionEvidence :many
-- Called only after worker, parent and target row locks have completed.
-- Owner/run scope includes every generation, including capture-less predecessor custody.
SELECT r.id, r.checkpoint_tip,
    EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = r.id AND c.user_id = r.user_id AND c.state = 'available')::boolean AS available_capture,
    EXISTS (SELECT 1 FROM checkpoint_publish_attempts p WHERE p.run_id = r.id)::boolean AS publication_uncertain,
    EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = r.id AND c.user_id = r.user_id AND c.state NOT IN ('available','expired','discarded'))::boolean AS capture_uncertain,
    EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.state NOT IN ('released','discarded'))::boolean AS custody_uncertain,
    (EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = r.id AND c.user_id = r.user_id AND c.state NOT IN ('preparing','uploading','available','needs_action','expired','discarded'))
     OR EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = r.id AND h.user_id = r.user_id AND h.state NOT IN ('open','released','discarded')))::boolean AS unknown_evidence
FROM runs r WHERE r.id = ANY(@run_ids::uuid[]);
