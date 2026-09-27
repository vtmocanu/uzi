-- PRD #1810: checkpoint-ref retention. A terminal run's published checkpoint ref
-- (refs/uzi-checkpoints/<branch>) is recorded in checkpoint_retentions and kept on origin
-- while the run has an open custody hold (D1); it is deleted, compare-and-swap on the
-- recorded tip, only once no hold is open. Every state transition below is guarded on the
-- expected current state, so a racing writer moves zero rows instead of clobbering a newer
-- state. Forge writes are serialised per run by the session advisory lock
-- (store.CheckpointRetentionLockClass) held by the caller; these statements do not lock.

-- name: InsertCheckpointRetentionIfHeld :execrows
-- D1: record a terminal run's checkpoint ref as RETAINED when the run published a checkpoint
-- (checkpoint_tip set), belongs to a repo, and has at least one OPEN custody hold (of any
-- generation). ON CONFLICT DO NOTHING: an existing row is NEVER reset, whatever its state; the
-- caller re-reads it (GetCheckpointRetention) when this moves zero rows.
INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
SELECT r.id, r.user_id, r.repo_id, @branch::text, r.checkpoint_tip, @ref::text, 'retained'
FROM runs r
WHERE r.id = @run_id
  AND r.repo_id IS NOT NULL
  AND r.checkpoint_tip IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = r.id AND h.state = 'open'
  )
ON CONFLICT (run_id) DO NOTHING;

-- name: InsertCheckpointRetentionSettling :execrows
-- D1: record a terminal run's checkpoint ref as owed a delete (SETTLING) when the run published
-- a checkpoint, belongs to a repo, and has NO open custody hold. The same never-reset rule as
-- InsertCheckpointRetentionIfHeld. The open-hold guard is repeated here so the two inserts
-- partition the runs: a run whose hold appears between the two statements is not recorded
-- settling (the caller re-reads and finds no row, and retains).
INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
SELECT r.id, r.user_id, r.repo_id, @branch::text, r.checkpoint_tip, @ref::text, 'settling'
FROM runs r
WHERE r.id = @run_id
  AND r.repo_id IS NOT NULL
  AND r.checkpoint_tip IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = r.id AND h.state = 'open'
  )
ON CONFLICT (run_id) DO NOTHING;

-- name: GetCheckpointRetention :one
SELECT * FROM checkpoint_retentions WHERE run_id = @run_id;

-- name: RunHasOpenCustodyHold :one
-- Whether any custody hold of any generation is still open for the run (D1: retention follows
-- custody).
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds h
    WHERE h.run_id = @run_id AND h.state = 'open'
)::bool AS held;

-- name: SetCheckpointRetentionSettlingIfUnheld :execrows
-- retained/superseded -> settling, only while NO custody hold of the run is open. The ref to
-- delete is whatever `ref` names (the branch ref, or the recovery ref once superseded).
UPDATE checkpoint_retentions
SET state = 'settling', next_attempt_at = now(), updated_at = now()
WHERE checkpoint_retentions.run_id = @run_id
  AND checkpoint_retentions.state IN ('retained', 'superseded')
  AND NOT EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = checkpoint_retentions.run_id AND h.state = 'open'
  );

-- name: SetCheckpointRetentionDeleted :execrows
-- settling -> deleted after the CAS delete of `ref` at `tip` succeeded (or found the ref gone
-- or moved). Guarded on the exact ref and tip the caller deleted, so a row that changed in
-- between moves zero rows. A deleted RECOVERY ref is re-verified after 10 minutes by the
-- reconciler (verify_after, M4); a branch ref needs no re-verification.
UPDATE checkpoint_retentions
SET state = 'deleted',
    settled_at = now(),
    updated_at = now(),
    last_error = NULL,
    verify_after = CASE WHEN recovery_ref IS NOT NULL THEN now() + interval '10 minutes' END
WHERE run_id = @run_id
  AND state = 'settling'
  AND ref = @ref::text
  AND tip = @tip::text;

-- name: RecordCheckpointRetentionFailure :execrows
-- A failed forge write: bump attempts, record the (already scrubbed) error, and push the next
-- retry out to the caller-computed backoff. Guarded on the state the caller acted in.
UPDATE checkpoint_retentions
SET attempts = attempts + 1,
    last_error = @last_error::text,
    next_attempt_at = @next_attempt_at::timestamptz,
    updated_at = now()
WHERE run_id = @run_id
  AND state = @expected_state::text;

-- name: SetCheckpointRetentionAbandoned :execrows
-- -> abandoned: the run's repo or forge connection is gone, so the delete can never be
-- brokered. Terminal; kept as an audit row. Guarded on the state the caller acted in.
UPDATE checkpoint_retentions
SET state = 'abandoned',
    last_error = @last_error::text,
    settled_at = now(),
    updated_at = now()
WHERE run_id = @run_id
  AND state = @expected_state::text;

-- name: BeginCheckpointSupersession :execrows
-- D2 step 1 (M3): retained -> superseding, persisting the recovery ref name BEFORE any forge
-- write, guarded on the tip the caller will CAS against.
UPDATE checkpoint_retentions
SET state = 'superseding',
    recovery_ref = @recovery_ref::text,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = now(),
    updated_at = now()
WHERE run_id = @run_id
  AND state = 'retained'
  AND tip = @tip::text;

-- name: MarkCheckpointSuperseded :one
-- D2 step 4 (M3): superseding -> superseded once the recovery ref exists at the tip and the
-- branch ref is gone; from here `ref` names the recovery ref, which settlement deletes. In the
-- SAME statement (so no window between the two), a run with NO open custody hold goes straight
-- to settling: its recovery ref is owed a CAS delete now, not after a later settle trigger.
-- Returns the state written; no row when the record was not superseding.
UPDATE checkpoint_retentions
SET state = CASE
        WHEN EXISTS (
            SELECT 1 FROM recovery_custody_holds h
            WHERE h.run_id = checkpoint_retentions.run_id AND h.state = 'open'
        ) THEN 'superseded'
        ELSE 'settling'
    END,
    ref = recovery_ref,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = now(),
    updated_at = now()
WHERE checkpoint_retentions.run_id = @run_id
  AND checkpoint_retentions.state = 'superseding'
  AND checkpoint_retentions.recovery_ref IS NOT NULL
RETURNING checkpoint_retentions.state;

-- name: SetCheckpointSupersessionTipGone :execrows
-- D2 (M3): superseding -> deleted when origin holds the recorded tip under NEITHER the branch
-- ref nor the recovery ref (checked by listing both after CreateRef reported the source
-- missing). Nothing uzi owns references the tip any more, so there is nothing to preserve or
-- delete; the note is kept in last_error for audit. A superseding record names its recovery
-- ref, so the post-settlement audit (M4) re-verifies it after 10 minutes: a create whose
-- session was lost after its fence may still land after this list.
UPDATE checkpoint_retentions
SET state = 'deleted',
    last_error = @last_error::text,
    settled_at = now(),
    updated_at = now(),
    verify_after = CASE WHEN recovery_ref IS NOT NULL THEN now() + interval '10 minutes' END
WHERE run_id = @run_id
  AND state = 'superseding';

-- name: SetCheckpointSupersessionExited :execrows
-- M4 (stuck superseding exit): superseding -> deleted for a record whose supersession stopped
-- (tip lag, or the recovery ref at another tip) once the run has NO open custody hold, after
-- the caller CAS-deleted whichever of its refs origin still held at the recorded tip. The
-- recovery ref is re-verified by the audit after 10 minutes, like any deleted recovery ref.
-- Guarded on the state and on the open-hold predicate, so a racing writer or a hold that
-- appeared moves zero rows.
UPDATE checkpoint_retentions
SET state = 'deleted',
    last_error = @last_error::text,
    settled_at = now(),
    updated_at = now(),
    verify_after = CASE WHEN recovery_ref IS NOT NULL THEN now() + interval '10 minutes' END
WHERE checkpoint_retentions.run_id = @run_id
  AND checkpoint_retentions.state = 'superseding'
  AND NOT EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = checkpoint_retentions.run_id AND h.state = 'open'
  );

-- name: AdvanceCheckpointRetentionTip :execrows
-- Tip lag (M3): a successful publish by a run that already has a record (the checkpoint_tip
-- persisted at its terminal transition lags a later publish) moves a record that still names
-- the branch ref to the tip just published, so a later CAS delete or supersession binds to
-- the tip origin actually holds. Only retained/settling records naming exactly that ref move;
-- a superseding/superseded record has already bound its recovery ref to its recorded tip.
UPDATE checkpoint_retentions
SET tip = @tip::text,
    updated_at = now()
WHERE run_id = @run_id
  AND ref = @ref::text
  AND state IN ('retained', 'settling');

-- name: ListCheckpointRetentionsForBranch :many
-- D2 (M3): the OTHER runs' active records still holding a branch's checkpoint slot (`ref` is the
-- branch ref), due for work now, newest first: the most recent terminal run is the likeliest
-- to own the tip origin currently advertises. Bounded: a branch holds one slot.
SELECT * FROM checkpoint_retentions
WHERE repo_id = @repo_id
  AND branch = @branch::text
  AND run_id <> @exclude_run_id
  AND ref = @ref::text
  AND state IN ('retained', 'superseding', 'settling')
  AND next_attempt_at <= now()
ORDER BY created_at DESC, run_id
LIMIT 10;

-- name: ListCheckpointRetentionWork :many
-- The reconciliation sweeper's candidates in the given states, oldest-due first, bounded.
-- The caller names the states its arms handle (superseding, settling), so rows no arm acts on
-- never crowd the bounded page. only_run_id NULL lists every run (production); a run id
-- confines the page to that run (the LiveDB tests' isolation from a reused database).
SELECT * FROM checkpoint_retentions
WHERE state = ANY(@states::text[])
  AND state IN ('superseding', 'settling', 'retained', 'superseded')
  AND next_attempt_at <= now()
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR run_id = sqlc.narg(only_run_id)::uuid)
ORDER BY next_attempt_at, run_id
LIMIT @max_rows::int;

-- name: ListUnheldCheckpointRetentions :many
-- M4: retained/superseded records whose run has NO open custody hold: their settle trigger was
-- lost (a crash between a hold's release and its trigger, a busy lock, a full retention slot)
-- or a hold settled between the terminal-time insert and its settle. The open-hold predicate is
-- in the query so the (normal, possibly many) records still held never crowd the bounded page.
SELECT * FROM checkpoint_retentions
WHERE checkpoint_retentions.state IN ('retained', 'superseded')
  AND checkpoint_retentions.next_attempt_at <= now()
  AND NOT EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = checkpoint_retentions.run_id AND h.state = 'open'
  )
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR checkpoint_retentions.run_id = sqlc.narg(only_run_id)::uuid)
ORDER BY checkpoint_retentions.next_attempt_at, checkpoint_retentions.run_id
LIMIT @max_rows::int;

-- name: ListCheckpointRetentionBackfill :many
-- M4 backfill: terminal runs that published a checkpoint and own a checkpoint branch (an issue
-- run with an issue iid, or a self_improve run) but have NO record: a terminal writer that
-- never calls the retention path (the sweeper's worker-loss and cap fails, the auto-stop, the
-- claim-assembly and Codex account-wait fails), or a best-effort insert that failed. Bounded to
-- runs whose current status began at or after retention was enabled
-- (checkpoint_retention_meta.enabled_at), so it never reaches back to runs the old
-- delete-on-terminal path already handled. The caller derives the branch in Go
-- (checkpointBranch) and records the run through the terminal-time inserts.
SELECT r.id, r.kind, r.issue_iid
FROM runs r
CROSS JOIN checkpoint_retention_meta m
WHERE r.status IN ('completed', 'failed', 'cancelled')
  AND r.checkpoint_tip IS NOT NULL
  AND r.repo_id IS NOT NULL
  AND ((r.kind = 'issue' AND r.issue_iid IS NOT NULL) OR r.kind = 'self_improve')
  AND r.status_since >= m.enabled_at
  AND NOT EXISTS (SELECT 1 FROM checkpoint_retentions c WHERE c.run_id = r.id)
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR r.id = sqlc.narg(only_run_id)::uuid)
ORDER BY r.status_since, r.id
LIMIT @max_rows::int;

-- name: ListCheckpointRetentionAudit :many
-- M4 post-settlement audit: deleted records that named a recovery ref, due for their one
-- re-verification (verify_after passed) and not yet verified. Closes the residual window of a
-- recovery-ref create whose session was lost after its fence but which landed after another
-- instance settled the record.
SELECT * FROM checkpoint_retentions
WHERE state = 'deleted'
  AND recovery_ref IS NOT NULL
  AND verify_after <= now()
  AND verified_at IS NULL
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR run_id = sqlc.narg(only_run_id)::uuid)
ORDER BY verify_after, run_id
LIMIT @max_rows::int;

-- name: SetCheckpointRetentionVerified :execrows
-- M4 audit: the deleted record's recovery ref is verified (absent, deleted now, or at another
-- tip that is not ours to delete). A non-NULL note replaces last_error. Guarded so a record is
-- verified once.
UPDATE checkpoint_retentions
SET verified_at = now(),
    last_error = COALESCE(sqlc.narg(note)::text, last_error),
    updated_at = now()
WHERE run_id = @run_id
  AND state = 'deleted'
  AND verified_at IS NULL;

-- name: DeferCheckpointRetentionVerify :execrows
-- M4 audit: the verification could not complete (forge or connection failure); push it out to
-- the caller-computed backoff and record the (already scrubbed) error. verified_at stays NULL.
UPDATE checkpoint_retentions
SET attempts = attempts + 1,
    last_error = @last_error::text,
    verify_after = @verify_after::timestamptz,
    updated_at = now()
WHERE run_id = @run_id
  AND state = 'deleted'
  AND verified_at IS NULL;
