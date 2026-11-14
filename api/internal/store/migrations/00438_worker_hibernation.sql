-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4205 M1: worker hibernation storage. Additive only; the state machine,
-- controller arm and UI land in later milestones.
-- Lock order: this migration locks workers only. hibernation_wakes (which needs
-- a runs lock for its FK) lives in 00439 so Up never waits on runs while holding
-- ACCESS EXCLUSIVE on workers.
ALTER TABLE workers
 ADD COLUMN hibernation_state text NOT NULL DEFAULT 'none',
 ADD COLUMN hibernation_op_id uuid,
 ADD COLUMN hibernation_requested_at timestamptz,
 ADD COLUMN hibernated_at timestamptz,
 ADD COLUMN wake_requested_at timestamptz,
 ADD COLUMN wake_error text,
 ADD COLUMN hibernation_reason text,
 ADD COLUMN hibernation_admission_pod_uid uuid,
 ADD COLUMN hibernation_scaling_down_at timestamptz,
 ADD COLUMN wake_ready_op_id uuid,
 ADD COLUMN wake_ready_pod_uid uuid,
 ADD COLUMN wake_ready_resource_version text,
 ADD COLUMN wake_register_op_id uuid,
 ADD COLUMN wake_register_pod_uid uuid;

-- Staged CHECKs: added NOT VALID so no workers scan happens under the ACCESS EXCLUSIVE lock the
-- ADD COLUMN above holds for this transaction. 00441 validates them in its own transaction
-- (SHARE UPDATE EXCLUSIVE, which does not block reads or writes). Every column is new, so the
-- validation cannot fail on existing rows.
ALTER TABLE workers
 ADD CONSTRAINT workers_hibernation_state_check CHECK (hibernation_state IN ('none','preparing','hibernated','waking')) NOT VALID,
 ADD CONSTRAINT workers_hibernation_coherent CHECK (
  hibernation_state = 'none'
  OR (hibernation_op_id IS NOT NULL AND kind = 'hosted' AND NOT ephemeral)
 ) NOT VALID,
 ADD CONSTRAINT workers_hibernated_at_set CHECK (
  hibernation_state <> 'hibernated' OR hibernated_at IS NOT NULL
 ) NOT VALID,
 ADD CONSTRAINT workers_wake_requested_at_set CHECK (
  hibernation_state <> 'waking' OR wake_requested_at IS NOT NULL
 ) NOT VALID,
 ADD CONSTRAINT workers_hibernation_text_bounds CHECK (
  (wake_error IS NULL OR char_length(wake_error) <= 500)
  AND (hibernation_reason IS NULL OR char_length(hibernation_reason) <= 500)
 ) NOT VALID;

-- Audit history. Snapshot identifiers deliberately have no foreign keys: deletion
-- must preserve history. Down keeps this table, so Up must be re-entrant and must
-- refuse a leftover table whose shape no longer matches.
-- Remember (transaction-locally) whether the table already existed, so a fresh install
-- never depends on the text pg_get_constraintdef prints; only a leftover table is
-- compared against the exact definitions below.
SELECT set_config('uzi.hibernation_events_preexisting', (to_regclass('worker_hibernation_events') IS NOT NULL)::text, true);
CREATE TABLE IF NOT EXISTS worker_hibernation_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL,
 worker_id uuid NOT NULL,
 actor_id uuid NOT NULL,
 actor_label text NOT NULL,
 action text NOT NULL,
 op_id uuid NOT NULL,
 reason text,
 occurred_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS worker_hibernation_events_worker_time_idx
 ON worker_hibernation_events (worker_id, occurred_at);

-- Re-entrant validation of a LEFTOVER table (one that existed before this migration
-- ran). A table created above is only given its named CHECKs: restore each named
-- CHECK that is absent. For a leftover only, also require the exact column
-- shape, the exact set of CHECK and PRIMARY KEY constraint definitions and the
-- exact index definition. A constraint or index that exists with a different
-- definition RAISEs; it is never silently replaced.
-- +goose StatementBegin
DO $$
DECLARE
 expected_cols text[] := ARRAY[
  'id:uuid:NO', 'owner_id:uuid:NO', 'worker_id:uuid:NO', 'actor_id:uuid:NO',
  'actor_label:text:NO', 'action:text:NO', 'op_id:uuid:NO', 'reason:text:YES',
  'occurred_at:timestamp with time zone:NO'];
 actual_cols text[];
 expected_cons text[] := ARRAY[
  'worker_hibernation_events_pkey=PRIMARY KEY (id)',
  'worker_hibernation_events_actor_label_check=CHECK (((char_length(actor_label) >= 1) AND (char_length(actor_label) <= 200)))',
  'worker_hibernation_events_action_check=CHECK ((action = ANY (ARRAY[''hibernate''::text, ''wake''::text])))',
  'worker_hibernation_events_reason_check=CHECK (((reason IS NULL) OR ((char_length(reason) >= 1) AND (char_length(reason) <= 500))))'];
 actual_cons text[];
 idx text;
 leftover boolean;
BEGIN
 leftover := current_setting('uzi.hibernation_events_preexisting', true) = 'true';
 SELECT array_agg(column_name || ':' || data_type || ':' || is_nullable ORDER BY column_name)
  INTO actual_cols
  FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'worker_hibernation_events';
 IF leftover AND actual_cols IS DISTINCT FROM (SELECT array_agg(e ORDER BY split_part(e, ':', 1)) FROM unnest(expected_cols) e) THEN
  RAISE EXCEPTION 'worker_hibernation_events has an unexpected shape: %', actual_cols;
 END IF;

 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'worker_hibernation_events'::regclass
   AND conname = 'worker_hibernation_events_actor_label_check') THEN
  ALTER TABLE worker_hibernation_events ADD CONSTRAINT worker_hibernation_events_actor_label_check
   CHECK (char_length(actor_label) BETWEEN 1 AND 200);
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'worker_hibernation_events'::regclass
   AND conname = 'worker_hibernation_events_action_check') THEN
  ALTER TABLE worker_hibernation_events ADD CONSTRAINT worker_hibernation_events_action_check
   CHECK (action IN ('hibernate','wake'));
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'worker_hibernation_events'::regclass
   AND conname = 'worker_hibernation_events_reason_check') THEN
  ALTER TABLE worker_hibernation_events ADD CONSTRAINT worker_hibernation_events_reason_check
   CHECK (reason IS NULL OR char_length(reason) BETWEEN 1 AND 500);
 END IF;

 IF NOT leftover THEN
  RETURN;
 END IF;

 SELECT array_agg(conname || '=' || pg_get_constraintdef(oid) ORDER BY conname)
  INTO actual_cons
  FROM pg_constraint
  WHERE conrelid = 'worker_hibernation_events'::regclass AND contype IN ('c', 'p');
 IF actual_cons IS DISTINCT FROM (SELECT array_agg(e ORDER BY split_part(e, '=', 1)) FROM unnest(expected_cons) e) THEN
  RAISE EXCEPTION 'worker_hibernation_events has unexpected constraints: %', actual_cons;
 END IF;

 SELECT pg_get_indexdef(indexrelid) INTO idx FROM pg_index
  WHERE indrelid = 'worker_hibernation_events'::regclass
    AND indexrelid = to_regclass('worker_hibernation_events_worker_time_idx');
 IF idx IS NULL OR idx NOT IN (
   'CREATE INDEX worker_hibernation_events_worker_time_idx ON worker_hibernation_events USING btree (worker_id, occurred_at)',
   'CREATE INDEX worker_hibernation_events_worker_time_idx ON ' || quote_ident(current_schema()) || '.worker_hibernation_events USING btree (worker_id, occurred_at)') THEN
  RAISE EXCEPTION 'worker_hibernation_events_worker_time_idx is missing or wrong: %', idx;
 END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE controller_report_status
 ADD COLUMN hibernation_version int,
 ADD COLUMN hibernation_advertised_at timestamptz,
 ADD COLUMN poll_interval_seconds int;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_worker_accepts_new_intake(w workers) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT w.manual_cordoned_at IS NULL AND w.draining_since IS NULL
  AND NOT w.maintenance_fenced
  AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling')
  AND w.hibernation_state = 'none';
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rollback while any worker is mid-hibernation.
-- +goose StatementBegin
DO $$
BEGIN
 LOCK TABLE workers IN ACCESS EXCLUSIVE MODE;
 IF EXISTS (SELECT 1 FROM workers WHERE hibernation_state <> 'none')
  THEN
  RAISE EXCEPTION 'worker hibernation state exists; rollback refused';
 END IF;
END;
$$;
-- +goose StatementEnd

-- Restore the previous body before the column it reads is dropped.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_worker_accepts_new_intake(w workers) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT w.manual_cordoned_at IS NULL AND w.draining_since IS NULL
  AND NOT w.maintenance_fenced
  AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling');
$$;
-- +goose StatementEnd

ALTER TABLE controller_report_status
 DROP COLUMN hibernation_version,
 DROP COLUMN hibernation_advertised_at,
 DROP COLUMN poll_interval_seconds;
ALTER TABLE workers
 DROP CONSTRAINT workers_hibernation_coherent,
 DROP CONSTRAINT workers_hibernated_at_set,
 DROP CONSTRAINT workers_wake_requested_at_set,
 DROP CONSTRAINT workers_hibernation_text_bounds,
 DROP CONSTRAINT workers_hibernation_state_check,
 DROP COLUMN hibernation_state,
 DROP COLUMN hibernation_op_id,
 DROP COLUMN hibernation_requested_at,
 DROP COLUMN hibernated_at,
 DROP COLUMN wake_requested_at,
 DROP COLUMN wake_error,
 DROP COLUMN hibernation_reason,
 DROP COLUMN hibernation_admission_pod_uid,
 DROP COLUMN hibernation_scaling_down_at,
 DROP COLUMN wake_ready_op_id,
 DROP COLUMN wake_ready_pod_uid,
 DROP COLUMN wake_ready_resource_version,
 DROP COLUMN wake_register_op_id,
 DROP COLUMN wake_register_pod_uid;
-- worker_hibernation_events is kept on purpose: it is audit history.
