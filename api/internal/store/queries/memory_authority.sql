-- Issue #4395: persisted memory-authority loss (workers.memory_authority_lost_at).
--
-- Both statements run inside a run-lane claim transaction that already holds the worker row
-- lock (GetWorkerForUpdate), after the claim snapshot's register nonce was validated against
-- that locked row. They repeat the nonce in the WHERE as the registration fence: a claim whose
-- registration was replaced (Register rotates snapshot_register_nonce and clears the column)
-- changes 0 rows, so a delayed held or lost report from an older registration can neither set
-- nor clear the new registration's value. The IS [NOT] NULL guard keeps the first-lost time and
-- makes a repeat a zero-row no-op.
--
-- workers.memory_authority_held_at (the last fresh held receipt) is written by
-- SetWorkerSnapshotState under the same nonce fence, cleared by Register, and cleared here by a
-- lost report: a loss invalidates any earlier held receipt. ClearWorkerMemoryAuthorityLost leaves
-- it alone; the same claim's snapshot apply stamps the new receipt.

-- name: SetWorkerMemoryAuthorityLost :execrows
UPDATE workers
   SET memory_authority_lost_at = @lost_at,
       memory_authority_held_at = NULL
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NULL;

-- name: ClearWorkerMemoryAuthorityLost :execrows
UPDATE workers
   SET memory_authority_lost_at = NULL
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NOT NULL;
