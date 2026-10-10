-- +goose Up
ALTER TABLE user_cross_check_pins DROP CONSTRAINT user_cross_check_pins_stage_check;
ALTER TABLE user_cross_check_pins ADD CONSTRAINT user_cross_check_pins_stage_check CHECK (stage IN ('plan','code'));

ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_dispositions_shape CHECK (
 (dispositions IS NULL AND finalized_at IS NULL)
 OR (stage = 'code' AND finalized_at IS NOT NULL AND dispositions IS NOT NULL
     AND jsonb_typeof(dispositions) = 'array' AND jsonb_array_length(dispositions) <= 20
     AND decided_at IS NOT NULL AND outcome <> 'pending')
);

-- Finalization survives interruption, but its evidence cannot be overwritten.
-- +goose StatementBegin
CREATE FUNCTION enforce_code_cross_check_dispositions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.finalized_at IS NOT NULL AND
   (NEW.finalized_at IS DISTINCT FROM OLD.finalized_at
    OR NEW.dispositions IS DISTINCT FROM OLD.dispositions
    OR NEW.findings IS DISTINCT FROM OLD.findings) THEN
  RAISE EXCEPTION 'immutable code cross-check dispositions';
 END IF;
 IF NEW.finalized_at IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.finalized_at IS NULL) AND
   (NEW.stage <> 'code' OR NEW.outcome <> 'completed'
    OR NEW.interrupted_at IS NOT NULL OR NEW.decided_at IS NULL) THEN
  RAISE EXCEPTION 'code cross-check is not completed' USING ERRCODE = '23514';
 END IF;
 IF NEW.finalized_at IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.finalized_at IS NULL) THEN
  IF jsonb_typeof(NEW.findings) IS DISTINCT FROM 'array'
    OR jsonb_typeof(NEW.dispositions) IS DISTINCT FROM 'array' THEN
   RAISE EXCEPTION 'invalid code dispositions array';
  END IF;
  IF jsonb_array_length(NEW.dispositions) > 20
    OR jsonb_array_length(NEW.dispositions) <> jsonb_array_length(NEW.findings)
    OR EXISTS (
      SELECT 1 FROM jsonb_array_elements(NEW.dispositions) d
      WHERE jsonb_typeof(d) IS DISTINCT FROM 'object'
       OR jsonb_typeof(d->'id') IS DISTINCT FROM 'string'
       OR (d->>'id') !~ '^[A-Za-z0-9_-]{1,64}$'
       OR jsonb_typeof(d->'reason') IS DISTINCT FROM 'string'
       OR octet_length(d->>'reason') > 1024
       OR jsonb_typeof(d->'disposition') IS DISTINCT FROM 'string'
       OR (d->>'disposition') NOT IN ('addressed','declined','not_reported')
       OR ((d->>'disposition') = 'not_reported' AND (d->>'reason') <> '')
       OR ((d->>'disposition') IN ('addressed','declined') AND btrim(d->>'reason') = '')
       OR NOT EXISTS (SELECT 1 FROM jsonb_array_elements(NEW.findings) f WHERE f->>'id' = d->>'id')
    )
    OR (SELECT count(DISTINCT d->>'id') FROM jsonb_array_elements(NEW.dispositions) d)
       <> jsonb_array_length(NEW.dispositions) THEN
   RAISE EXCEPTION 'invalid code dispositions';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER cross_checks_dispositions_write_once BEFORE INSERT OR UPDATE ON cross_checks
 FOR EACH ROW EXECUTE FUNCTION enforce_code_cross_check_dispositions();
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER cross_checks_dispositions_write_once ON cross_checks;
DROP FUNCTION enforce_code_cross_check_dispositions();
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_dispositions_shape;
DELETE FROM user_cross_check_pins WHERE stage = 'code';
ALTER TABLE user_cross_check_pins DROP CONSTRAINT user_cross_check_pins_stage_check;
ALTER TABLE user_cross_check_pins ADD CONSTRAINT user_cross_check_pins_stage_check CHECK (stage = 'plan');
