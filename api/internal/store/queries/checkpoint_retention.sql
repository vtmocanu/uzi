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
--
-- COOLING PERIOD (#1810 residual 2): the intent is refused (zero rows) until the run has been in
-- its current (terminal) status for at least @cooling, read on the database clock in the same
-- statement. A publish that was routed while the run was still live may be in flight when the run
-- turns terminal; waiting out the cooling period lets it land and be tracked (its tip advances this
-- record) before the record's tip is bound to a recovery ref. This is a DELAY that makes the
-- reconciliation path rare, not the correctness argument: a push whose outcome is unknown can
-- still land later, and checkpoint_publish_attempts plus the sweeper's attempts arm reconcile it.
-- runs.status_since is stamped by every statement that assigns runs.status (00163).
UPDATE checkpoint_retentions
SET state = 'superseding',
    recovery_ref = @recovery_ref::text,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = now(),
    updated_at = now()
WHERE checkpoint_retentions.run_id = @run_id
  AND checkpoint_retentions.state = 'retained'
  AND checkpoint_retentions.tip = @tip::text
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.id = checkpoint_retentions.run_id
        AND r.status_since <= now() - @cooling::interval
  );

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
-- session was lost after its fence may still land after this list. This close has no open-hold
-- guard, so the audit is custody-aware: a recovery ref it finds at the tip while a hold is still
-- open REOPENS the record as superseded (ReopenCheckpointRetentionSupersededIfHeld) and is
-- never deleted then.
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
-- Compare-and-set on what the publish observed immediately before its forge call, exactly as
-- TrackTerminalCheckpointPublish (see there): a record another writer moved since is never moved
-- backwards by a publish whose writes arrive late.
UPDATE checkpoint_retentions
SET tip = @tip::text,
    updated_at = now()
WHERE run_id = @run_id
  AND ref = @ref::text
  AND state IN ('retained', 'settling')
  AND (tip = @tip::text
       OR tip = sqlc.narg(expected_tip)::text
       OR (sqlc.narg(expected_tip)::text IS NULL AND tip = sqlc.narg(expected_run_tip)::text));

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

-- name: ListActiveCheckpointRetentionsForBranch :many
-- D2 (#1810 M1 rework): the OTHER runs' records that hold (retained) or are handing off
-- (superseding) a branch's checkpoint slot, whatever their backoff: Publish reads it BEFORE any
-- forge write, so a publish that DESCENDS from an older run's retained tip (a fast-forward the
-- broker would accept) still supersedes that record first instead of leaving it retained at a
-- tip that is no longer the branch tip. No next_attempt_at filter: a backed-off record still
-- holds the slot, and the caller refuses the push until it is freed. Newest first, bounded.
SELECT * FROM checkpoint_retentions
WHERE repo_id = @repo_id
  AND branch = @branch::text
  AND run_id <> @exclude_run_id
  AND ref = @ref::text
  AND state IN ('retained', 'superseding')
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

-- name: GetCheckpointRetentionBackfillNow :one
-- M4 backfill: the database's clock, read immediately BEFORE a backfill page is listed. A page
-- that was not full proves the watermark only up to this instant (a candidate that became one
-- after it may be missing from the page), never up to the later moment the watermark is advanced.
SELECT now()::timestamptz AS listed_at;

-- name: ListCheckpointRetentionBackfill :many
-- M4 backfill: terminal runs that published a checkpoint and own a checkpoint branch (an issue
-- run with an issue iid, or a self_improve run) but have NO record. Since migration 00266 every
-- terminal transition inserts its record in the same transaction (the runs.status trigger,
-- whichever writer made it), so what is left here is a terminal run whose first checkpoint tip was
-- persisted only AFTER the transition (the trigger saw no tip) and whose own track did not record
-- it: its TrackTerminalCheckpointPublish insert failed, or retention was not wired in the api
-- that served the publish. Bounded to
-- runs whose current status began at or after retention was enabled
-- (checkpoint_retention_meta.enabled_at), so it never reaches back to runs the old
-- delete-on-terminal path already handled.
--
-- Paged by the candidate's BACKFILL KEY, GREATEST(status_since, checkpoint_tip_at): the later of
-- when the run went terminal and when it last published (GREATEST ignores a NULL
-- checkpoint_tip_at). A run becomes a candidate at its terminal transition or at its first
-- publish after it, whichever is later, and its key is that moment, so a run that first
-- publishes long after it went terminal is keyed at the publish, not left below the watermark.
-- The key never decreases (checkpoint_tip_at only moves forward, and a terminal status does not
-- change). The scan starts at the persisted watermark (backfilled_through) less a 10-minute
-- overlap and uses idx_runs_checkpoint_backfill (00265, on exactly this expression), so a
-- steady-state pass reads only recent candidates. The overlap covers commit-order skew: both
-- columns are the writer's transaction time, so a run committed after a pass advanced the
-- watermark past its key is still inside the window on the next pass. The caller derives the
-- branch in Go (checkpointBranch), records the run through the terminal-time inserts, and
-- advances the watermark (AdvanceCheckpointRetentionBackfillWatermark) from what the page proved.
SELECT r.id, r.kind, r.issue_iid,
       GREATEST(r.status_since, r.checkpoint_tip_at)::timestamptz AS backfill_key
FROM runs r
WHERE r.status IN ('completed', 'failed', 'cancelled')
  AND r.checkpoint_tip IS NOT NULL
  AND r.repo_id IS NOT NULL
  AND ((r.kind = 'issue' AND r.issue_iid IS NOT NULL) OR r.kind = 'self_improve')
  AND r.status_since >= (SELECT m.enabled_at FROM checkpoint_retention_meta m)
  -- The scan's lower bound, as a scalar subquery (an InitPlan) so the planner uses it as the
  -- index condition on idx_runs_checkpoint_backfill: a join filter against the meta row would
  -- walk the index from its start. GREATEST ignores a NULL watermark (enabled_at alone).
  AND GREATEST(r.status_since, r.checkpoint_tip_at) >= (
      SELECT GREATEST(m.enabled_at, m.backfilled_through - interval '10 minutes') FROM checkpoint_retention_meta m
  )
  AND NOT EXISTS (SELECT 1 FROM checkpoint_retentions c WHERE c.run_id = r.id)
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR r.id = sqlc.narg(only_run_id)::uuid)
ORDER BY GREATEST(r.status_since, r.checkpoint_tip_at), r.id
LIMIT @max_rows::int;

-- name: AdvanceCheckpointRetentionBackfillWatermark :execrows
-- M4 backfill watermark: `through` is what one backfill page proved: every candidate whose
-- backfill key (GREATEST(status_since, checkpoint_tip_at)) is below it and that was committed
-- when the page was listed has been recorded. Monotonic (GREATEST), so a pass computed from an
-- older snapshot never moves the watermark back past a newer pass's proof; the 10-minute overlap
-- in ListCheckpointRetentionBackfill is what re-scans candidates committed late.
UPDATE checkpoint_retention_meta
SET backfilled_through = GREATEST(
        COALESCE(backfilled_through, '-infinity'::timestamptz),
        @through::timestamptz
    );

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

-- name: ReopenCheckpointRetentionSupersededIfHeld :execrows
-- M4 audit (custody guard): deleted -> superseded when the audit finds the run's recovery ref on
-- origin at the recorded tip (a create whose session was lost after its fence landed after the
-- record closed) while the run STILL has an open custody hold. That ref is now the only copy of
-- the tip custody protects, so it is never deleted here: the record names it again (ref =
-- recovery_ref) and ordinary settlement owns it, deleting it CAS on the tip once the last hold
-- settles (the settle trigger, or the sweeper's unheld arm). settled_at/verify_after/verified_at
-- are cleared: the reopened record is live again, and its own later settle re-arms the audit.
-- Guarded on the audit's preconditions and the open-hold predicate in one statement.
UPDATE checkpoint_retentions
SET state = 'superseded',
    ref = recovery_ref,
    attempts = 0,
    last_error = @last_error::text,
    next_attempt_at = now(),
    settled_at = NULL,
    verify_after = NULL,
    verified_at = NULL,
    updated_at = now()
WHERE checkpoint_retentions.run_id = @run_id
  AND checkpoint_retentions.state = 'deleted'
  AND checkpoint_retentions.verified_at IS NULL
  AND checkpoint_retentions.recovery_ref = @recovery_ref::text
  AND checkpoint_retentions.tip = @tip::text
  AND EXISTS (
      SELECT 1 FROM recovery_custody_holds h
      WHERE h.run_id = checkpoint_retentions.run_id AND h.state = 'open'
  );

-- name: TrackTerminalCheckpointPublish :one
-- A successful checkpoint publish by a TERMINAL run (a worker still bound to it: a shutdown
-- checkpoint of a just-cancelled run may be its latest work) makes the run's record track the
-- branch-ref tip it just published, in ONE guarded statement, so no ref outlives its record:
--
--   - no record: insert one, retained if the run has an open custody hold, else settling;
--   - retained/settling naming the branch ref: advance the tip (state, attempts and backoff kept);
--   - deleted/abandoned naming the branch ref: REOPEN it as retained (held) or settling (not),
--     with the retry bookkeeping and the settle/audit stamps reset.
--
-- A record that ever named a recovery ref (recovery_ref set: its branch slot was handed, or was
-- being handed, to a newer run) is never touched: the conflict WHERE moves zero rows and no row
-- is returned. Publish refuses such a run before its forge call; this guard is the backstop.
-- The terminal-status predicate is on the SELECT, so for a live run nothing is proposed and the
-- ON CONFLICT arm cannot fire either: a live run's record is never reopened.
--
-- COMPARE-AND-SET (PRD #1810 D2, residual 2): the conflict arm moves an existing record only when
-- its tip is what the publish observed immediately before its forge call (expected_tip; NULL: no
-- record was observed), or is already the published tip. A publish routed LIVE writes without the
-- run's retention lock, and its writes may arrive long after its push landed: by then a newer
-- publish, or the sweeper's attempts arm re-recording a newer late push, may have moved the record,
-- and an unconditional write would move it BACKWARDS to a tip origin no longer carries. When no
-- record was observed, the record may since have been inserted by the run's terminal transaction
-- from runs.checkpoint_tip (migration 00266), so a record at the runs.checkpoint_tip the publish
-- observed (expected_run_tip) also matches: that tip predates this push. The insert arm (no record
-- at all) is unconditional. No row moved: the caller treats the publish as untracked and keeps its
-- attempt row for the sweeper.
-- Returns the record's state after the statement; no row when nothing moved.
INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
SELECT r.id, r.user_id, r.repo_id, @branch::text, @tip::text, @ref::text,
       CASE WHEN EXISTS (
           SELECT 1 FROM recovery_custody_holds h
           WHERE h.run_id = r.id AND h.state = 'open'
       ) THEN 'retained' ELSE 'settling' END
FROM runs r
WHERE r.id = @run_id
  AND r.repo_id IS NOT NULL
  AND r.status IN ('completed', 'failed', 'cancelled')
ON CONFLICT (run_id) DO UPDATE
SET tip = EXCLUDED.tip,
    state = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                 THEN checkpoint_retentions.state ELSE EXCLUDED.state END,
    attempts = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                    THEN checkpoint_retentions.attempts ELSE 0 END,
    last_error = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                      THEN checkpoint_retentions.last_error END,
    next_attempt_at = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                           THEN checkpoint_retentions.next_attempt_at ELSE now() END,
    settled_at = NULL,
    verify_after = NULL,
    verified_at = NULL,
    updated_at = now()
WHERE checkpoint_retentions.recovery_ref IS NULL
  AND checkpoint_retentions.ref = EXCLUDED.ref
  AND checkpoint_retentions.state IN ('retained', 'settling', 'deleted', 'abandoned')
  AND (checkpoint_retentions.tip = EXCLUDED.tip
       OR checkpoint_retentions.tip = sqlc.narg(expected_tip)::text
       OR (sqlc.narg(expected_tip)::text IS NULL
           AND checkpoint_retentions.tip = sqlc.narg(expected_run_tip)::text))
RETURNING checkpoint_retentions.state;

-- name: GetRunCheckpointTipForRetention :one
-- The run's server-persisted latest published tip (runs.checkpoint_tip, advanced on every
-- successful publish). The stuck-supersession exit uses it to prove a branch ref at a tip the
-- record does not name is still this run's own publish.
SELECT checkpoint_tip FROM runs WHERE id = @run_id;

-- PRD #1810 D2 (residual 2): checkpoint_publish_attempts, the durable record of a checkpoint push
-- written BEFORE the forge call (migration 00267).

-- name: RecordCheckpointPublishAttempt :one
-- Written immediately before one push. The caller refuses the push when this fails: no push is
-- ever sent without its row.
INSERT INTO checkpoint_publish_attempts (run_id, branch, ref, tip)
VALUES (@run_id, @branch::text, @ref::text, @tip::text)
RETURNING id;

-- name: DeleteCheckpointPublishAttempt :execrows
-- The push's outcome is accounted for (refused by the forge, or landed and tracked), or the
-- sweeper reconciled or retired it.
DELETE FROM checkpoint_publish_attempts WHERE id = @id;

-- name: GetCheckpointPublishAttempt :one
SELECT * FROM checkpoint_publish_attempts WHERE id = @id;

-- name: RunHasCheckpointPublishAttempt :one
-- Whether the run has an outstanding push of exactly this tip: a branch ref at that tip may be
-- the run's own (possibly late) publish, even when runs.checkpoint_tip never recorded it.
SELECT EXISTS (
    SELECT 1 FROM checkpoint_publish_attempts a
    WHERE a.run_id = @run_id AND a.tip = @tip::text
)::bool AS attempted;

-- name: ListDueCheckpointPublishAttempts :many
-- The sweeper's attempts arm: outstanding attempts, due for a comparison, whose run is TERMINAL
-- (or gone). A live run's own next publish fetches origin and builds on whatever landed; once it
-- is terminal nothing else of the run will. A live run's rows are excluded here, in SQL, so they
-- never occupy the bounded page. A terminal run's rows are listed only once the run has been
-- terminal for @cooling (the supersession cooling period, on the database clock): until then a
-- push routed while it was live may still be returning and track its own tip. That is a delay
-- only; the arm's writes are compare-and-set on what it read, so a concurrent publish is never
-- overwritten either way. only_run_id NULL lists every run (production); a run id confines the
-- page (the LiveDB tests' isolation from a reused database).
SELECT a.* FROM checkpoint_publish_attempts a
LEFT JOIN runs r ON r.id = a.run_id
WHERE a.next_check_at <= now()
  AND (r.id IS NULL OR (r.status IN ('completed', 'failed', 'cancelled')
                        AND r.status_since <= now() - @cooling::interval))
  AND (sqlc.narg(only_run_id)::uuid IS NULL OR a.run_id = sqlc.narg(only_run_id)::uuid)
ORDER BY a.next_check_at, a.id
LIMIT @max_rows::int;

-- name: DeferCheckpointPublishAttempt :execrows
-- The comparison found nothing to reconcile yet (the ref is not at the attempted tip) or could not
-- run; check again after the caller-computed backoff. A non-NULL note replaces last_error.
UPDATE checkpoint_publish_attempts
SET checks = checks + 1,
    next_check_at = @next_check_at::timestamptz,
    last_error = COALESCE(sqlc.narg(note)::text, last_error)
WHERE id = @id;

-- name: TrackReconciledCheckpointPublish :one
-- The attempts arm's re-record of a late-landed tip: TrackTerminalCheckpointPublish, but
-- COMPARE-AND-SET on the record the arm observed BEFORE it listed origin. expected_tip is that
-- record's tip, or NULL when the run had no record: then only the insert may happen, and a record
-- that appeared meanwhile (a newer publish of the run, tracked while the arm was listing) moves
-- nothing. A newer publish never takes the retention lock the arm holds when it was routed live,
-- so without this guard the arm could move the record's tip BACKWARDS over it. Returns the
-- record's state after the statement; no row when nothing moved (the caller re-checks later).
INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
SELECT r.id, r.user_id, r.repo_id, @branch::text, @tip::text, @ref::text,
       CASE WHEN EXISTS (
           SELECT 1 FROM recovery_custody_holds h
           WHERE h.run_id = r.id AND h.state = 'open'
       ) THEN 'retained' ELSE 'settling' END
FROM runs r
WHERE r.id = @run_id
  AND r.repo_id IS NOT NULL
  AND r.status IN ('completed', 'failed', 'cancelled')
ON CONFLICT (run_id) DO UPDATE
SET tip = EXCLUDED.tip,
    state = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                 THEN checkpoint_retentions.state ELSE EXCLUDED.state END,
    attempts = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                    THEN checkpoint_retentions.attempts ELSE 0 END,
    last_error = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                      THEN checkpoint_retentions.last_error END,
    next_attempt_at = CASE WHEN checkpoint_retentions.state IN ('retained', 'settling')
                           THEN checkpoint_retentions.next_attempt_at ELSE now() END,
    settled_at = NULL,
    verify_after = NULL,
    verified_at = NULL,
    updated_at = now()
WHERE checkpoint_retentions.recovery_ref IS NULL
  AND checkpoint_retentions.ref = EXCLUDED.ref
  AND checkpoint_retentions.state IN ('retained', 'settling', 'deleted', 'abandoned')
  AND checkpoint_retentions.tip = sqlc.narg(expected_tip)::text
RETURNING checkpoint_retentions.state;

-- name: SetRunCheckpointTipIf :execrows
-- SetRunCheckpointTip, but COMPARE-AND-SET on the runs.checkpoint_tip the writer observed (NULL:
-- none was persisted): Publish's persist (observed immediately before its forge call) and the
-- attempts arm's persist of a late-landed tip (observed before it listed origin). A newer tip
-- persisted meanwhile moves nothing here, so the tip never moves backwards. Idempotent: a row
-- already at @checkpoint_tip matches too (the same tip persisted meanwhile, by a retry or the
-- attempts arm), so the writer tracks its push rather than reporting a moved tip; the tip itself
-- does not change in that arm.
UPDATE runs SET checkpoint_tip = @checkpoint_tip::text, checkpoint_tip_at = now()
WHERE id = @id
  AND (checkpoint_tip IS NOT DISTINCT FROM sqlc.narg(expected_tip)::text
       OR checkpoint_tip = @checkpoint_tip::text);

-- name: CheckpointTipClaimedByOtherRun :one
-- Whether any OTHER run of the repo claims this tip (its persisted checkpoint tip, an outstanding
-- publish attempt, or a live retention record): a branch ref at such a tip is never deleted as
-- this run's late publish.
SELECT (
    EXISTS (
        SELECT 1 FROM runs r
        WHERE r.repo_id = @repo_id AND r.id <> @run_id AND r.checkpoint_tip = @tip::text
    )
    OR EXISTS (
        SELECT 1 FROM checkpoint_publish_attempts a
        JOIN runs r ON r.id = a.run_id
        WHERE r.repo_id = @repo_id AND a.run_id <> @run_id AND a.tip = @tip::text
    )
    OR EXISTS (
        SELECT 1 FROM checkpoint_retentions c
        WHERE c.repo_id = @repo_id AND c.run_id <> @run_id AND c.tip = @tip::text
          AND c.state IN ('retained', 'superseding', 'superseded', 'settling')
    )
)::bool AS claimed;
