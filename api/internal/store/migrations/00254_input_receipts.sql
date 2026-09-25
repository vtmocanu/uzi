-- +goose Up
ALTER TABLE run_user_inputs ADD COLUMN consumed_claim_generation BIGINT;
ALTER TABLE run_user_inputs ADD COLUMN consumed_worker_id UUID;
ALTER TABLE run_user_inputs ADD COLUMN applied_at TIMESTAMPTZ;
-- Existing consume-on-read deliveries were already applied by legacy workers.
UPDATE run_user_inputs SET applied_at = consumed_at WHERE consumed_at IS NOT NULL;
CREATE INDEX idx_run_user_inputs_replay ON run_user_inputs (run_id, id)
    WHERE applied_at IS NULL AND kind NOT IN ('scope', 'resume', 'completion_decision', 'extend');

-- +goose Down
-- Re-pend ACKed but unapplied inputs, so the legacy consume-on-read drain delivers them.
UPDATE run_user_inputs SET consumed_at = NULL WHERE applied_at IS NULL AND consumed_at IS NOT NULL;
DROP INDEX idx_run_user_inputs_replay;
ALTER TABLE run_user_inputs DROP COLUMN applied_at;
ALTER TABLE run_user_inputs DROP COLUMN consumed_worker_id;
ALTER TABLE run_user_inputs DROP COLUMN consumed_claim_generation;
