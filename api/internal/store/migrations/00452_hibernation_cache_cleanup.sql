-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4206: the hibernation cache prune attempt record. One attempt per hibernation, kept
-- on the worker row so it is locked and written with the hibernation columns. Additive only;
-- every default is a constant, so no table rewrite. This migration locks workers only.
ALTER TABLE workers
 ADD COLUMN hibernation_cleanup text,
 ADD COLUMN hibernation_cleanup_reason text,
 ADD COLUMN cleanup_op_id uuid,
 ADD COLUMN cleanup_phase text NOT NULL DEFAULT 'none',
 ADD COLUMN cleanup_register_nonce text,
 ADD COLUMN cleanup_pod_uid uuid,
 ADD COLUMN cleanup_requested_at timestamptz,
 ADD COLUMN cleanup_started_at timestamptz,
 ADD COLUMN cleanup_unknown_at timestamptz,
 ADD COLUMN cleanup_settled_at timestamptz,
 ADD COLUMN cleanup_cancel_requested boolean NOT NULL DEFAULT false,
 ADD COLUMN cleanup_reconcile_seq int NOT NULL DEFAULT 0,
 ADD COLUMN cleanup_reconciled_at timestamptz;

-- Staged CHECKs: NOT VALID so no workers scan runs under the ACCESS EXCLUSIVE lock above; the
-- next migration validates them (every column is new, so validation cannot fail on old rows).
-- The two fences are the backstop against an older api image writing a hibernation state
-- change while an attempt is unsettled: the write is refused rather than reopening claims.
ALTER TABLE workers
 ADD CONSTRAINT workers_cleanup_phase_check CHECK (cleanup_phase IN ('none','requested','started','unknown','settled')) NOT VALID,
 ADD CONSTRAINT workers_hibernation_cleanup_check CHECK (hibernation_cleanup IS NULL OR hibernation_cleanup IN ('pruned','skipped','failed')) NOT VALID,
 ADD CONSTRAINT workers_hibernation_cleanup_reason_bounds CHECK (hibernation_cleanup_reason IS NULL OR char_length(hibernation_cleanup_reason) <= 500) NOT VALID,
 ADD CONSTRAINT workers_cleanup_reconcile_seq_check CHECK (cleanup_reconcile_seq >= 0) NOT VALID,
 ADD CONSTRAINT workers_cleanup_coherent CHECK (
  cleanup_phase = 'none'
  OR (cleanup_op_id IS NOT NULL AND cleanup_register_nonce IS NOT NULL AND cleanup_requested_at IS NOT NULL)
 ) NOT VALID,
 ADD CONSTRAINT workers_cleanup_started_coherent CHECK (
  cleanup_phase NOT IN ('started','unknown')
  OR (cleanup_started_at IS NOT NULL AND cleanup_pod_uid IS NOT NULL)
 ) NOT VALID,
 ADD CONSTRAINT workers_cleanup_unknown_coherent CHECK (
  cleanup_phase <> 'unknown' OR cleanup_unknown_at IS NOT NULL
 ) NOT VALID,
 ADD CONSTRAINT workers_cleanup_settled_coherent CHECK (
  cleanup_phase <> 'settled' OR (cleanup_settled_at IS NOT NULL AND hibernation_cleanup IS NOT NULL)
 ) NOT VALID,
 ADD CONSTRAINT workers_cleanup_fence_none CHECK (
  hibernation_state <> 'none' OR cleanup_phase IN ('none','settled')
 ) NOT VALID,
 ADD CONSTRAINT workers_cleanup_fence_unsettled CHECK (
  cleanup_phase NOT IN ('requested','started','unknown')
  OR (hibernation_state IN ('preparing','waking') AND hibernation_scaling_down_at IS NULL)
 ) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rollback while any attempt is unsettled: an older image would not honour the claim fence.
-- +goose StatementBegin
DO $$
BEGIN
 LOCK TABLE workers IN ACCESS EXCLUSIVE MODE;
 IF EXISTS (SELECT 1 FROM workers WHERE cleanup_phase IN ('requested','started','unknown'))
  THEN
  RAISE EXCEPTION 'hibernation cache cleanup attempt unsettled; rollback refused';
 END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE workers
 DROP CONSTRAINT workers_cleanup_fence_unsettled,
 DROP CONSTRAINT workers_cleanup_fence_none,
 DROP CONSTRAINT workers_cleanup_settled_coherent,
 DROP CONSTRAINT workers_cleanup_unknown_coherent,
 DROP CONSTRAINT workers_cleanup_started_coherent,
 DROP CONSTRAINT workers_cleanup_coherent,
 DROP CONSTRAINT workers_cleanup_reconcile_seq_check,
 DROP CONSTRAINT workers_hibernation_cleanup_reason_bounds,
 DROP CONSTRAINT workers_hibernation_cleanup_check,
 DROP CONSTRAINT workers_cleanup_phase_check,
 DROP COLUMN hibernation_cleanup,
 DROP COLUMN hibernation_cleanup_reason,
 DROP COLUMN cleanup_op_id,
 DROP COLUMN cleanup_phase,
 DROP COLUMN cleanup_register_nonce,
 DROP COLUMN cleanup_pod_uid,
 DROP COLUMN cleanup_requested_at,
 DROP COLUMN cleanup_started_at,
 DROP COLUMN cleanup_unknown_at,
 DROP COLUMN cleanup_settled_at,
 DROP COLUMN cleanup_cancel_requested,
 DROP COLUMN cleanup_reconcile_seq,
 DROP COLUMN cleanup_reconciled_at;
