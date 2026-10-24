-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers
 ADD COLUMN manual_cordoned_at timestamptz,
 ADD COLUMN manual_cordoned_by uuid,
 ADD COLUMN manual_cordoned_by_label text,
 ADD COLUMN manual_cordon_reason text;
ALTER TABLE workers ADD CONSTRAINT workers_manual_cordon_coherent CHECK (
 (manual_cordoned_at IS NULL AND manual_cordoned_by IS NULL AND manual_cordoned_by_label IS NULL AND manual_cordon_reason IS NULL)
 OR (NOT ephemeral AND kind IN ('hosted','external') AND manual_cordoned_at IS NOT NULL
     AND manual_cordoned_by IS NOT NULL AND manual_cordoned_by_label IS NOT NULL
     AND char_length(manual_cordoned_by_label) BETWEEN 1 AND 200
     AND (manual_cordon_reason IS NULL OR char_length(manual_cordon_reason) BETWEEN 1 AND 500))
) NOT VALID;
-- Snapshot identifiers deliberately have no foreign keys: deletion must preserve history.
CREATE TABLE worker_cordon_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL,
 worker_id uuid NOT NULL,
 actor_id uuid NOT NULL,
 actor_label text NOT NULL CHECK (char_length(actor_label) BETWEEN 1 AND 200),
 action text NOT NULL CHECK (action IN ('cordon','uncordon')),
 occurred_at timestamptz NOT NULL,
 reason text CHECK (reason IS NULL OR char_length(reason) BETWEEN 1 AND 500)
);
CREATE INDEX worker_cordon_events_worker_time_idx ON worker_cordon_events (worker_id, occurred_at);

-- +goose StatementBegin
CREATE FUNCTION fn_worker_accepts_new_intake(w workers) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT w.manual_cordoned_at IS NULL AND w.draining_since IS NULL
  AND NOT w.maintenance_fenced
  AND w.maintenance_phase NOT IN ('requested','ready','stopping','recycling');
$$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rollback while intent or audit history exists.
-- +goose StatementBegin
DO $$
BEGIN
 LOCK TABLE workers, worker_cordon_events IN ACCESS EXCLUSIVE MODE;
 IF EXISTS (SELECT 1 FROM workers WHERE manual_cordoned_at IS NOT NULL)
    OR EXISTS (SELECT 1 FROM worker_cordon_events) THEN
  RAISE EXCEPTION 'manual cordon intent or audit history exists; rollback refused';
 END IF;
END;
$$;
-- +goose StatementEnd
DROP FUNCTION fn_worker_accepts_new_intake(workers);
DROP TABLE worker_cordon_events;
ALTER TABLE workers DROP CONSTRAINT workers_manual_cordon_coherent,
 DROP COLUMN manual_cordoned_at, DROP COLUMN manual_cordoned_by,
 DROP COLUMN manual_cordoned_by_label, DROP COLUMN manual_cordon_reason;
