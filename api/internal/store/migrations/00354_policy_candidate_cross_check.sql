-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Names cross_checks.id without a foreign key: run deletion remains the retention
-- boundary without introducing a cascade/immutability cycle.
ALTER TABLE runs ADD COLUMN policy_candidate_cross_check_id uuid;

DROP TRIGGER runs_policy_stop_resolution ON runs;
DROP FUNCTION enforce_policy_stop_resolution();
-- +goose StatementBegin
CREATE FUNCTION enforce_policy_stop_resolution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND OLD.policy_stop_resolution IS NOT NULL AND
   (NEW.policy_stop_resolution IS DISTINCT FROM OLD.policy_stop_resolution
    OR NEW.policy_revision_source IS DISTINCT FROM OLD.policy_revision_source
    OR NEW.policy_candidate_cross_check_id IS DISTINCT FROM OLD.policy_candidate_cross_check_id) THEN
  RAISE EXCEPTION 'immutable policy stop resolution';
 END IF;
 IF NEW.policy_stop_resolution IS NOT NULL THEN
  PERFORM 1 FROM run_user_inputs scope
   WHERE scope.run_id = NEW.id AND scope.kind = 'scope'
    AND to_jsonb(scope.id) = NEW.policy_stop_resolution->'scope_input_id'
   FOR SHARE;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'policy stop resolution requires same-run scope input';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER runs_policy_stop_resolution
 BEFORE INSERT OR UPDATE OF policy_stop_resolution, policy_revision_source, policy_candidate_cross_check_id ON runs
 FOR EACH ROW EXECUTE FUNCTION enforce_policy_stop_resolution();

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs, run_user_inputs, cross_checks IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM runs WHERE policy_candidate_cross_check_id IS NOT NULL
    OR policy_stop_resolution IS NOT NULL OR policy_revision_source IS NOT NULL)
  OR EXISTS (SELECT 1 FROM run_user_inputs WHERE settled_at IS NOT NULL OR superseded_by_stop_input_id IS NOT NULL
    OR disposition = 'superseded_by_stop')
  OR EXISTS (SELECT 1 FROM cross_checks WHERE revision_disposition IS NOT NULL
    OR revision_settled_at IS NOT NULL OR revision_superseded_by_stop_input_id IS NOT NULL) THEN
  RAISE EXCEPTION 'policy candidate or stop evidence exists; refusing lossy rollback';
 END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER runs_policy_stop_resolution ON runs;
DROP FUNCTION enforce_policy_stop_resolution();
ALTER TABLE runs DROP COLUMN policy_candidate_cross_check_id;

-- +goose StatementBegin
CREATE FUNCTION enforce_policy_stop_resolution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND OLD.policy_stop_resolution IS NOT NULL AND
   (NEW.policy_stop_resolution IS DISTINCT FROM OLD.policy_stop_resolution
    OR NEW.policy_revision_source IS DISTINCT FROM OLD.policy_revision_source) THEN
  RAISE EXCEPTION 'immutable policy stop resolution';
 END IF;
 IF NEW.policy_stop_resolution IS NOT NULL THEN
  PERFORM 1 FROM run_user_inputs scope
   WHERE scope.run_id = NEW.id AND scope.kind = 'scope'
    AND to_jsonb(scope.id) = NEW.policy_stop_resolution->'scope_input_id'
   FOR SHARE;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'policy stop resolution requires same-run scope input';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER runs_policy_stop_resolution
 BEFORE INSERT OR UPDATE OF policy_stop_resolution, policy_revision_source ON runs
 FOR EACH ROW EXECUTE FUNCTION enforce_policy_stop_resolution();
