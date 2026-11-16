-- Issue #4395: persisted memory-authority loss (workers.memory_authority_lost_at).
--
-- Both statements run inside a run-lane claim transaction that already holds the worker row
-- lock (GetWorkerForUpdate), after the claim snapshot's register nonce was validated against
-- that locked row. They repeat the nonce in the WHERE as the registration fence: a claim whose
-- registration was replaced (Register rotates snapshot_register_nonce and clears the column)
-- changes 0 rows, so a delayed held or lost report from an older registration can neither set
-- nor clear the new registration's value. The IS [NOT] NULL guard keeps the first-lost time and
-- makes a repeat a zero-row no-op.

-- name: SetWorkerMemoryAuthorityLost :execrows
UPDATE workers
   SET memory_authority_lost_at = @lost_at
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NULL;

-- name: ClearWorkerMemoryAuthorityLost :execrows
UPDATE workers
   SET memory_authority_lost_at = NULL
 WHERE id = @id
   AND snapshot_register_nonce = @register_nonce
   AND memory_authority_lost_at IS NOT NULL;
