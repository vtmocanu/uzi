-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION checkpoint_retention_on_terminal() RETURNS trigger AS $$
DECLARE
    coordinates jsonb;
BEGIN
    IF NEW.repo_id IS NULL OR NEW.checkpoint_tip IS NULL THEN RETURN NULL; END IF;
    -- Live graph resolution authorizes enrollment, not task retention.
    IF NEW.kind = 'task' AND NEW.checkpoint_coordinates IS NULL THEN RETURN NULL; END IF;
    coordinates := fn_checkpoint_coordinates(NEW);
    IF coordinates IS NULL THEN RETURN NULL; END IF;
    INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
    VALUES (NEW.id, NEW.user_id, NEW.repo_id, coordinates->>'checkpoint_key', NEW.checkpoint_tip,
        'refs/uzi-checkpoints/' || (coordinates->>'checkpoint_key'),
        CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h
                          WHERE h.run_id = NEW.id AND h.state = 'open')
             THEN 'retained' ELSE 'settling' END)
    ON CONFLICT (run_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- Match migration 00352's enrollment exclusion before restoring its function.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$ BEGIN
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'checkpoint schema downgrade requires READ COMMITTED';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE checkpoint_coordinates IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot downgrade with enrolled checkpoint coordinates';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION checkpoint_retention_on_terminal() RETURNS trigger AS $$
DECLARE
    coordinates jsonb;
BEGIN
    IF NEW.repo_id IS NULL OR NEW.checkpoint_tip IS NULL THEN RETURN NULL; END IF;
    coordinates := fn_checkpoint_coordinates(NEW);
    IF coordinates IS NULL THEN RETURN NULL; END IF;
    INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
    VALUES (NEW.id, NEW.user_id, NEW.repo_id, coordinates->>'checkpoint_key', NEW.checkpoint_tip,
        'refs/uzi-checkpoints/' || (coordinates->>'checkpoint_key'),
        CASE WHEN EXISTS (SELECT 1 FROM recovery_custody_holds h
                          WHERE h.run_id = NEW.id AND h.state = 'open')
             THEN 'retained' ELSE 'settling' END)
    ON CONFLICT (run_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
