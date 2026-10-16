-- +goose Up
ALTER TABLE run_schedules ADD COLUMN ci_artifacts jsonb
 CHECK (ci_artifacts IS NULL OR (target = 'ci_watch' AND jsonb_typeof(ci_artifacts) = 'array' AND jsonb_array_length(ci_artifacts) <= 20 AND octet_length(ci_artifacts::text) <= 16384));
ALTER TABLE runs ADD COLUMN ci_artifact_manifest jsonb
 CHECK (ci_artifact_manifest IS NULL OR (kind = 'prompt' AND ci_config_guard AND jsonb_typeof(ci_artifact_manifest) = 'object' AND octet_length(ci_artifact_manifest::text) <= 131072));
-- +goose Down
ALTER TABLE runs DROP COLUMN ci_artifact_manifest;
ALTER TABLE run_schedules DROP COLUMN ci_artifacts;
