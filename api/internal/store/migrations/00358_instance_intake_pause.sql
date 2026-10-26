-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #3454: instance-wide intake pause ("maintenance mode"). One authoritative row so the
-- claim SQL can read it per statement (app_settings can be env-sourced and TTL-cached).
CREATE TABLE instance_intake_pause (
 id boolean PRIMARY KEY DEFAULT true CHECK (id),
 active boolean NOT NULL DEFAULT false,
 since timestamptz,
 by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
 by_label text,
 reason text CHECK (reason IS NULL OR char_length(reason) <= 280),
 CONSTRAINT instance_intake_pause_coherent CHECK (active = (since IS NOT NULL))
);
INSERT INTO instance_intake_pause (id) VALUES (true);

-- Append-only audit, written in the same transaction as each transition.
CREATE TABLE instance_intake_pause_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 at timestamptz NOT NULL DEFAULT now(),
 by_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
 by_label text,
 action text NOT NULL CHECK (action IN ('on','off','reason')),
 reason text CHECK (reason IS NULL OR char_length(reason) <= 280)
);
CREATE INDEX instance_intake_pause_events_at_idx ON instance_intake_pause_events (at DESC);

-- +goose StatementBegin
-- A missing row means PAUSED (fail closed for new roots); admitted continuations stay
-- eligible through fn_run_admitted_during_intake_pause. Deliberately no exception
-- swallowing: a read error fails the claim statement (PRD #3454 D2).
CREATE FUNCTION fn_instance_intake_paused() RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS $$
 SELECT COALESCE((SELECT active FROM instance_intake_pause WHERE id), true);
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- BEGIN ATOMIC records column dependencies on runs.kind and runs.claim_generation: a future
-- type change to either must drop and recreate this function.
-- Admitted = claimed at least once, or a cross-check child (ClaimRun already binds it to a
-- claimed lead). Everything else is a new root (PRD #3454 D4). Kept separate from
-- fn_worker_accepts_new_intake so capacity and capability projections stay truthful (D2).
CREATE FUNCTION fn_run_admitted_during_intake_pause(r runs) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
BEGIN ATOMIC
 SELECT r.claim_generation > 0 OR r.kind = 'cross_check';
END;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rollback while an active pause or audit history exists.
-- +goose StatementBegin
DO $$
BEGIN
 LOCK TABLE instance_intake_pause, instance_intake_pause_events IN ACCESS EXCLUSIVE MODE;
 IF EXISTS (SELECT 1 FROM instance_intake_pause WHERE active)
    OR EXISTS (SELECT 1 FROM instance_intake_pause_events) THEN
  RAISE EXCEPTION 'instance intake pause is active or has audit history; rollback refused';
 END IF;
END;
$$;
-- +goose StatementEnd
DROP FUNCTION fn_run_admitted_during_intake_pause(runs);
DROP FUNCTION fn_instance_intake_paused();
DROP TABLE instance_intake_pause_events;
DROP TABLE instance_intake_pause;
