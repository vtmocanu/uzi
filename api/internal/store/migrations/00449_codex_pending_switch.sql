-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs
    ADD COLUMN codex_switch_target_secret_id uuid,
    ADD COLUMN codex_switch_target_generation bigint,
    ADD COLUMN codex_switch_target_op_id uuid;

-- Runs' existing operation trigger fires first (triggers run by name). The
-- pending target is never a claim binding and survives only its exact stamp.
-- +goose StatementBegin
CREATE FUNCTION runs_codex_switch_target() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.credential_switch_requested_at IS NULL
       OR NEW.codex_switch_target_generation IS DISTINCT FROM NEW.credential_switch_generation
       OR NEW.codex_switch_target_op_id IS DISTINCT FROM NEW.credential_switch_op_id THEN
        NEW.codex_switch_target_secret_id := NULL;
        NEW.codex_switch_target_generation := NULL;
        NEW.codex_switch_target_op_id := NULL;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_z_codex_switch_target BEFORE UPDATE ON runs
FOR EACH ROW EXECUTE FUNCTION runs_codex_switch_target();

-- +goose Down
DROP TRIGGER runs_z_codex_switch_target ON runs;
DROP FUNCTION runs_codex_switch_target();
ALTER TABLE runs
    DROP COLUMN codex_switch_target_secret_id,
    DROP COLUMN codex_switch_target_generation,
    DROP COLUMN codex_switch_target_op_id;
