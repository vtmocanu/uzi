-- name: UpsertRunDecisionMemoFenced :execrows
-- Run decisions memo write (issue #2083 M1). ONE statement: the run row is locked FOR NO KEY UPDATE and the
-- ownership/claim/status/kind predicates are evaluated under that lock, so a write racing the run's
-- completion (SetRunCompleted takes the same row lock) or a re-claim either lands before it or sees
-- the new state and writes nothing. 0 rows means the claim is not current (stale generation,
-- released claim, not running, a kind that carries no memo, repo-less, or not this worker's run).
-- Never a read followed by an unconditional upsert.
-- FOR NO KEY UPDATE, not FOR UPDATE: it conflicts with the plain UPDATEs of SetRunCompleted and ClaimRun
-- (which take the same lock mode), so the fence is identical, but it does not conflict with the KEY SHARE
-- lock a child-table foreign-key insert takes on the run row, so memo writes never stall those inserts.
WITH r AS (
    SELECT id FROM runs
     WHERE id = @run_id
       AND worker_id = @worker_id
       AND claim_generation = @claim_generation
       AND claim_released_at IS NULL
       AND status = 'running'
       AND kind IN ('issue', 'prompt', 'self_improve', 'mr_rework')
       AND repo_id IS NOT NULL
       FOR NO KEY UPDATE
)
INSERT INTO run_decision_memos (run_id, claim_generation, format_version, body)
SELECT r.id, @claim_generation::bigint, 1, @body::text FROM r
ON CONFLICT (run_id) DO UPDATE SET
    body             = EXCLUDED.body,
    format_version   = EXCLUDED.format_version,
    claim_generation = EXCLUDED.claim_generation,
    updated_at       = now();

-- name: GetLatestDecisionMemoForLineage :one
-- The newest completed, generation-compatible memo on one MR lineage (issue #2083 M1): same owner,
-- repo, branch (the rework run's pipeline_ref) and mr_iid. m.claim_generation = r.claim_generation
-- drops a memo written by a flight that was re-claimed afterwards and never re-saved (a completed
-- run keeps the generation it completed on). The caller's own run is excluded.
SELECT m.run_id, m.body, m.format_version
  FROM run_decision_memos m
  JOIN runs r ON r.id = m.run_id
 WHERE r.user_id = @user_id
   AND r.repo_id = @repo_id
   AND r.branch = @branch
   AND r.mr_iid = @mr_iid
   AND r.status = 'completed'
   AND r.kind IN ('issue', 'prompt', 'self_improve', 'mr_rework')
   AND m.format_version = 1
   AND m.claim_generation = r.claim_generation
   AND r.id <> @self_run_id
 ORDER BY r.finished_at DESC NULLS LAST, m.updated_at DESC
 LIMIT 1;
