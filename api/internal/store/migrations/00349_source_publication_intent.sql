-- +goose Up
SET LOCAL lock_timeout = '2s';
ALTER TABLE runs ADD COLUMN source_publication_intent jsonb;
ALTER TABLE runs ADD CONSTRAINT runs_source_publication_intent_check CHECK (
 source_publication_intent IS NULL OR (
 kind = 'issue'
 AND jsonb_typeof(source_publication_intent) = 'object'
 AND octet_length(source_publication_intent::text) <= 4096
 AND source_publication_intent ?& ARRAY['version','claim_generation','branch','mr_iid','published_head','reported_non_closing','intent']
 AND source_publication_intent - ARRAY['version','claim_generation','branch','mr_iid','published_head','reported_non_closing','intent'] = '{}'::jsonb
 AND source_publication_intent->'version' = '1'::jsonb
 AND jsonb_typeof(source_publication_intent->'claim_generation') = 'number'
 AND (source_publication_intent->>'claim_generation') ~ '^[1-9][0-9]{0,18}$'
 AND jsonb_typeof(source_publication_intent->'mr_iid') = 'number'
 AND (source_publication_intent->>'mr_iid') ~ '^[1-9][0-9]{0,18}$'
 AND jsonb_typeof(source_publication_intent->'branch') = 'string'
 AND length(source_publication_intent->>'branch') BETWEEN 1 AND 1024
 AND jsonb_typeof(source_publication_intent->'published_head') = 'string'
 AND (source_publication_intent->>'published_head') ~ '^[0-9a-fA-F]{40}$'
 AND jsonb_typeof(source_publication_intent->'reported_non_closing') = 'boolean'
 AND jsonb_typeof(source_publication_intent->'intent') = 'string'
 AND source_publication_intent->>'intent' IN ('closing','non_closing')
 AND (source_publication_intent->'reported_non_closing' = 'false'::jsonb OR source_publication_intent->>'intent' = 'non_closing')
 ) IS TRUE) NOT VALID;
-- +goose StatementBegin
CREATE FUNCTION guard_source_publication_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.source_publication_intent IS NOT NULL AND NEW.source_publication_intent IS DISTINCT FROM OLD.source_publication_intent THEN
  RAISE EXCEPTION 'source publication intent is immutable';
 END IF;
 IF OLD.source_publication_intent IS NULL AND NEW.source_publication_intent IS NOT NULL AND (
  NEW.kind <> 'issue' OR NEW.status <> 'completed' OR NEW.report_only
  OR NEW.branch IS DISTINCT FROM NEW.source_publication_intent->>'branch'
  OR NEW.mr_iid::text IS DISTINCT FROM NEW.source_publication_intent->>'mr_iid'
  OR NEW.claim_generation::text IS DISTINCT FROM NEW.source_publication_intent->>'claim_generation'
 ) THEN
  RAISE EXCEPTION 'source publication identity mismatch';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_source_publication_intent_guard BEFORE UPDATE OF source_publication_intent ON runs
 FOR EACH ROW EXECUTE FUNCTION guard_source_publication_intent();

-- +goose Down
SET LOCAL lock_timeout = '2s';
DROP TRIGGER runs_source_publication_intent_guard ON runs;
DROP FUNCTION guard_source_publication_intent();
ALTER TABLE runs DROP COLUMN source_publication_intent;
