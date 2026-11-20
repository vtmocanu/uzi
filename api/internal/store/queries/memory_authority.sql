-- Issue #4395: persisted memory-authority loss (workers.memory_authority_lost_at).
-- Issue #4492: a locked protected-vault owner's polls record the report too.
--
-- Each statement runs inside a transaction that already holds the worker row lock
-- (GetWorkerForUpdate), after the claim snapshot's register nonce was validated against that
-- locked row: the run-lane claim transaction, or the vault-locked report transaction. They repeat
-- the nonce in the WHERE as the registration fence: a claim whose registration was replaced
-- (Register rotates snapshot_register_nonce and clears both columns) changes 0 rows, so a delayed
-- held or lost report from an older registration can neither set nor stamp the new registration's
-- values. The IS NULL guard on the loss column keeps the first-lost time and makes a repeat a
-- zero-row no-op.
--
-- A recorded loss is sticky for its registration: nothing here clears it, and only Register does
-- (an explicit lost report is one-way until the worker restarts; a loss recorded because a
-- capable worker omitted the member also stays, an accepted cost). A held report therefore never
-- clears the loss, and the receipt writers (StampWorkerMemoryAuthorityHeld here, SetWorkerSnapshotState) refuse to
-- stamp while the loss is set.
--
-- workers.memory_authority_held_at (the last fresh held receipt) is written by
-- SetWorkerSnapshotState on a run-lane claim and by StampWorkerMemoryAuthorityHeld on a
-- vault-locked poll, both under the same nonce fence; it is cleared by Register and by a lost
-- report: a loss invalidates any earlier held receipt.

-- name: SetWorkerMemoryAuthorityLost :execrows
UPDATE workers
   SET memory_authority_lost_at = @lost_at,
       memory_authority_held_at = NULL
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NULL;

-- name: StampWorkerMemoryAuthorityHeld :execrows
UPDATE workers SET memory_authority_held_at = @held_at
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NULL;
