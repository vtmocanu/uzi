-- Issue #4700: run_usage fold materializer and refold lock (migration 00543). The Go
-- driver is MaterializeRunUsageFolds (run_usage_folds.go); these statements are its steps
-- and are deliberately SEPARATE statements, see the invariants documented there.

-- name: LockRunForUsageRefold :one
-- First statement of RefoldRunUsage's transaction: takes the runs row FOR UPDATE (not FOR NO
-- KEY UPDATE) before the refold touches run_usage. Every run_usage writer holds the runs row
-- FOR KEY SHARE until its transaction ends (fn_run_usage_fold_lock, migration 00543), and FOR
-- UPDATE conflicts with that, so the refold waits out every in-flight writer HERE, before
-- DeleteRunUsage's statement snapshot is taken. A weaker FOR NO KEY UPDATE does not conflict
-- with KEY SHARE: the refold's delete would then block inside its own statement on the
-- writer's state row, resume with a snapshot that predates the writer's commit, and leave a
-- NEW-key row that writer inserted (invisible to that snapshot) behind. The same lock
-- serialises the refold against a run delete and against other refolds. No row means the run
-- was deleted while the refold waited: the caller returns nil.
SELECT id FROM runs WHERE id = @id FOR UPDATE;

-- name: SelectRunUsageFoldNewCandidates :many
-- Step (a0): eligible runs with no state row yet, key-share locked so a concurrent run
-- delete cannot slip between this read and the state insert; SKIP LOCKED drops runs a
-- deleting transaction holds.
SELECT r.id FROM runs r
WHERE r.id = ANY(@ids::uuid[])
  AND r.status IN ('completed', 'failed', 'cancelled')
  AND r.kind <> 'chat'
  AND r.usage_refolded
  AND NOT EXISTS (SELECT 1 FROM run_usage_fold_state s WHERE s.run_id = r.id)
ORDER BY r.id
FOR KEY SHARE OF r SKIP LOCKED;

-- name: InsertRunUsageFoldStates :exec
-- Step (a): create the missing state rows (stale, per the column default).
INSERT INTO run_usage_fold_state (run_id)
SELECT x FROM unnest(@p_new::uuid[]) x ORDER BY x
ON CONFLICT DO NOTHING;

-- name: LockRunUsageFoldStates :many
-- Step (b): lock the eligible runs' state rows FOR UPDATE SKIP LOCKED. A writer that holds a
-- state row (it marked it stale inside its open transaction) makes the run skip; once this
-- lock is held a writer blocks in fn_run_usage_fold_lock until the materializer commits.
SELECT s.run_id FROM run_usage_fold_state s
JOIN runs r ON r.id = s.run_id
WHERE s.run_id = ANY(@ids::uuid[])
  AND r.status IN ('completed', 'failed', 'cancelled')
  AND r.kind <> 'chat'
  AND r.usage_refolded
ORDER BY s.run_id
FOR UPDATE OF s SKIP LOCKED;

-- name: DeleteRunUsageModelFolds :exec
-- Step (c): drop the locked runs' previous fold rows.
DELETE FROM run_usage_model_folds WHERE run_id = ANY(@l::uuid[]);

-- name: InsertRunUsageModelFolds :execrows
-- Step (d): re-fold the locked runs from run_usage_model_totals. MUST be its own statement
-- after step (b): under READ COMMITTED its snapshot is taken now, after the state rows were
-- locked, so every write committed before the lock is visible and every later write blocks.
INSERT INTO run_usage_model_folds (run_id, model, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
SELECT run_id, model, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status
FROM run_usage_model_totals
WHERE run_id = ANY(@l::uuid[]);

-- name: MarkRunUsageFoldStatesFresh :exec
-- Step (e): flip the locked state rows fresh.
UPDATE run_usage_fold_state SET stale = false, folded_at = now() WHERE run_id = ANY(@l::uuid[]);
