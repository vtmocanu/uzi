-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs
 ADD COLUMN policy_stop_resolution jsonb,
 ADD COLUMN policy_revision_source jsonb,
 ADD CONSTRAINT runs_policy_revision_source_shape CHECK
  (policy_revision_source IS NULL OR jsonb_typeof(policy_revision_source) = 'object'),
 ADD CONSTRAINT runs_policy_stop_resolution_shape CHECK
  (policy_stop_resolution IS NULL OR COALESCE(
    jsonb_typeof(policy_stop_resolution) = 'object'
    AND policy_stop_resolution->'version' = '1'::jsonb
    AND jsonb_typeof(policy_stop_resolution->'scope_input_id') = 'number'
    AND policy_stop_resolution->>'scope_input_id' ~ '^[1-9][0-9]*$', false));

ALTER TABLE run_user_inputs
 ADD COLUMN settled_at timestamptz,
 ADD COLUMN superseded_by_stop_input_id bigint REFERENCES run_user_inputs(id),
 DROP CONSTRAINT run_user_inputs_disposition_check,
 ADD CONSTRAINT run_user_inputs_disposition_check CHECK
  (disposition IN ('applied', 'declined', 'superseded', 'advisory', 'superseded_by_stop')),
 ADD CONSTRAINT run_user_inputs_stop_settlement_shape CHECK (
  ((disposition IS NOT DISTINCT FROM 'superseded_by_stop')
    = (settled_at IS NOT NULL AND superseded_by_stop_input_id IS NOT NULL))
  AND ((settled_at IS NOT NULL) = (superseded_by_stop_input_id IS NOT NULL))
  AND (settled_at IS NULL OR (kind = 'revise_plan' AND applied_at IS NULL AND included_at IS NULL)));

ALTER TABLE cross_checks
 ADD COLUMN revision_disposition text CHECK (revision_disposition = 'superseded_by_stop'),
 ADD COLUMN revision_settled_at timestamptz,
 ADD COLUMN revision_superseded_by_stop_input_id bigint REFERENCES run_user_inputs(id),
 ADD CONSTRAINT cross_checks_stop_settlement_shape CHECK (
  ((revision_disposition IS NOT NULL)
    = (revision_settled_at IS NOT NULL AND revision_superseded_by_stop_input_id IS NOT NULL))
  AND ((revision_settled_at IS NOT NULL) = (revision_superseded_by_stop_input_id IS NOT NULL))
  AND (revision_disposition IS NULL OR (stage = 'plan' AND verdict = 'revise')));

CREATE INDEX run_user_inputs_stop_scope ON run_user_inputs(superseded_by_stop_input_id)
 WHERE superseded_by_stop_input_id IS NOT NULL;
CREATE INDEX cross_checks_stop_scope ON cross_checks(revision_superseded_by_stop_input_id)
 WHERE revision_superseded_by_stop_input_id IS NOT NULL;

-- A settlement preserves even the original delivery receipt, including a NULL
-- consumed_at. It is neither application nor inclusion of the requested revision.
-- +goose StatementBegin
CREATE FUNCTION enforce_policy_stop_input_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  IF OLD.settled_at IS NOT NULL AND EXISTS (SELECT 1 FROM runs WHERE id = OLD.run_id) THEN
   RAISE EXCEPTION 'immutable policy stop input settlement';
  END IF;
  RETURN OLD;
 END IF;
 IF TG_OP = 'UPDATE' AND OLD.settled_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'immutable policy stop input settlement';
 END IF;
 IF NEW.settled_at IS NOT NULL THEN
  IF TG_OP = 'UPDATE' AND
    (to_jsonb(NEW) - ARRAY['disposition','settled_at','superseded_by_stop_input_id'])
      IS DISTINCT FROM
    (to_jsonb(OLD) - ARRAY['disposition','settled_at','superseded_by_stop_input_id']) THEN
   RAISE EXCEPTION 'policy stop settlement must preserve input evidence';
  END IF;
  PERFORM 1 FROM run_user_inputs scope
   WHERE scope.id = NEW.superseded_by_stop_input_id
    AND scope.run_id = NEW.run_id AND scope.kind = 'scope'
   FOR SHARE;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'policy stop settlement requires same-run scope input';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_user_inputs_policy_stop_settlement
 BEFORE INSERT OR UPDATE OR DELETE ON run_user_inputs
 FOR EACH ROW EXECUTE FUNCTION enforce_policy_stop_input_settlement();

-- Checker run deletion may still clear the existing live FK; the accepted
-- manifest retains checker identity, and all other checker provenance stays put.
-- +goose StatementBegin
CREATE FUNCTION enforce_policy_stop_cross_check_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'DELETE' THEN
  IF OLD.revision_settled_at IS NOT NULL AND EXISTS (SELECT 1 FROM runs WHERE id = OLD.lead_run_id) THEN
   RAISE EXCEPTION 'immutable policy stop cross-check settlement';
  END IF;
  RETURN OLD;
 END IF;
 IF TG_OP = 'UPDATE' AND OLD.revision_settled_at IS NOT NULL THEN
  IF (to_jsonb(NEW) - ARRAY['wait_credited','interrupted_at','checker_run_id'])
    IS DISTINCT FROM
     (to_jsonb(OLD) - ARRAY['wait_credited','interrupted_at','checker_run_id'])
    OR (NEW.checker_run_id IS DISTINCT FROM OLD.checker_run_id AND
      (NEW.checker_run_id IS NOT NULL OR EXISTS (SELECT 1 FROM runs WHERE id = OLD.checker_run_id))) THEN
   RAISE EXCEPTION 'immutable policy stop cross-check settlement';
  END IF;
 END IF;
 IF NEW.revision_settled_at IS NOT NULL THEN
  IF TG_OP = 'UPDATE' AND OLD.revision_settled_at IS NULL AND
    (to_jsonb(NEW) - ARRAY['revision_disposition','revision_settled_at','revision_superseded_by_stop_input_id'])
      IS DISTINCT FROM
    (to_jsonb(OLD) - ARRAY['revision_disposition','revision_settled_at','revision_superseded_by_stop_input_id']) THEN
   RAISE EXCEPTION 'policy stop settlement must preserve cross-check evidence';
  END IF;
  PERFORM 1 FROM run_user_inputs scope
   WHERE scope.id = NEW.revision_superseded_by_stop_input_id
    AND scope.run_id = NEW.lead_run_id AND scope.kind = 'scope'
   FOR SHARE;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'policy stop settlement requires same-run scope input';
  END IF;
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER cross_checks_policy_stop_settlement
 BEFORE INSERT OR UPDATE OR DELETE ON cross_checks
 FOR EACH ROW EXECUTE FUNCTION enforce_policy_stop_cross_check_settlement();

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

-- A scope audit can receive its ordinary disposition; it cannot be rewritten or
-- removed while an accepted resolution or a settlement still references it.
-- Run deletion remains the retention boundary (the parent is already absent
-- during its input cascade).
-- +goose StatementBegin
CREATE FUNCTION protect_policy_stop_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.kind = 'scope' AND EXISTS (
   SELECT 1 FROM runs r WHERE r.id = OLD.run_id AND (
    r.policy_stop_resolution->'scope_input_id' = to_jsonb(OLD.id)
    OR EXISTS (SELECT 1 FROM run_user_inputs u WHERE u.superseded_by_stop_input_id = OLD.id)
    OR EXISTS (SELECT 1 FROM cross_checks cc WHERE cc.revision_superseded_by_stop_input_id = OLD.id))
 ) THEN
  IF TG_OP = 'DELETE' THEN
   RAISE EXCEPTION 'policy stop scope input is referenced';
  END IF;
  IF (NEW.id,NEW.run_id,NEW.kind,NEW.body,NEW.created_at,NEW.gate_binding,NEW.gate_revision)
    IS DISTINCT FROM (OLD.id,OLD.run_id,OLD.kind,OLD.body,OLD.created_at,OLD.gate_binding,OLD.gate_revision) THEN
   RAISE EXCEPTION 'immutable policy stop scope evidence';
  END IF;
 END IF;
 IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER run_user_inputs_policy_stop_scope
 BEFORE UPDATE OR DELETE ON run_user_inputs
 FOR EACH ROW EXECUTE FUNCTION protect_policy_stop_scope();

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs, run_user_inputs, cross_checks IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM runs WHERE policy_stop_resolution IS NOT NULL OR policy_revision_source IS NOT NULL)
  OR EXISTS (SELECT 1 FROM run_user_inputs WHERE settled_at IS NOT NULL OR superseded_by_stop_input_id IS NOT NULL
    OR disposition = 'superseded_by_stop')
  OR EXISTS (SELECT 1 FROM cross_checks WHERE revision_disposition IS NOT NULL
    OR revision_settled_at IS NOT NULL OR revision_superseded_by_stop_input_id IS NOT NULL) THEN
  RAISE EXCEPTION 'policy stop evidence exists; refusing lossy rollback';
 END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER run_user_inputs_policy_stop_scope ON run_user_inputs;
DROP FUNCTION protect_policy_stop_scope();
DROP TRIGGER runs_policy_stop_resolution ON runs;
DROP FUNCTION enforce_policy_stop_resolution();
DROP TRIGGER cross_checks_policy_stop_settlement ON cross_checks;
DROP FUNCTION enforce_policy_stop_cross_check_settlement();
DROP TRIGGER run_user_inputs_policy_stop_settlement ON run_user_inputs;
DROP FUNCTION enforce_policy_stop_input_settlement();
DROP INDEX cross_checks_stop_scope;
DROP INDEX run_user_inputs_stop_scope;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_stop_settlement_shape,
 DROP COLUMN revision_disposition, DROP COLUMN revision_settled_at, DROP COLUMN revision_superseded_by_stop_input_id;
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_stop_settlement_shape,
 DROP CONSTRAINT run_user_inputs_disposition_check,
 ADD CONSTRAINT run_user_inputs_disposition_check CHECK (disposition IN ('applied','declined','superseded','advisory')),
 DROP COLUMN settled_at, DROP COLUMN superseded_by_stop_input_id;
ALTER TABLE runs DROP CONSTRAINT runs_policy_stop_resolution_shape,
 DROP CONSTRAINT runs_policy_revision_source_shape,
 DROP COLUMN policy_stop_resolution, DROP COLUMN policy_revision_source;
