-- +goose Up
-- No backfill: historical and plan accounts remain NULL.
ALTER TABLE cross_checks ADD COLUMN coverage jsonb;
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_coverage_shape CHECK (
 coverage IS NULL OR (stage = 'code' AND decided_at IS NOT NULL
  AND (outcome = 'completed' OR interrupted_at IS NOT NULL)
  AND jsonb_typeof(coverage) = 'object'
  AND octet_length(coverage::text) <= 262144)
);

-- Separate from snapshot identity and dispositions: coverage is fixed at verdict,
-- even when dispositions have not yet been finalized.
-- +goose StatementBegin
CREATE FUNCTION enforce_code_cross_check_coverage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'INSERT' THEN
  IF NEW.coverage IS NOT NULL THEN
   RAISE EXCEPTION 'coverage requires a code verdict transition';
  END IF;
 ELSIF NEW.coverage IS DISTINCT FROM OLD.coverage THEN
  IF OLD.coverage IS NOT NULL OR OLD.stage <> 'code' OR NEW.stage <> 'code'
    OR OLD.outcome <> 'pending' OR OLD.decided_at IS NOT NULL
    OR NEW.outcome <> 'completed' OR NEW.decided_at IS NULL
    OR OLD.interrupted_at IS NOT NULL OR NEW.interrupted_at IS NOT NULL
    OR NEW.findings IS NULL OR NEW.coverage IS NULL THEN
   RAISE EXCEPTION 'immutable code cross-check coverage';
  END IF;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER cross_checks_coverage_write_once BEFORE INSERT OR UPDATE ON cross_checks
 FOR EACH ROW EXECUTE FUNCTION enforce_code_cross_check_coverage();
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER cross_checks_coverage_write_once ON cross_checks;
DROP FUNCTION enforce_code_cross_check_coverage();
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_coverage_shape, DROP COLUMN coverage;
