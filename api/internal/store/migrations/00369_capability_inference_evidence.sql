-- +goose Up
-- NULL preserves unknown provenance on existing runs; new repo runs snapshot hints.
ALTER TABLE runs ADD COLUMN hinted_capabilities text[];
ALTER TABLE runs ADD COLUMN capability_inference_evidence jsonb;

-- +goose Down
ALTER TABLE runs DROP COLUMN capability_inference_evidence;
ALTER TABLE runs DROP COLUMN hinted_capabilities;
