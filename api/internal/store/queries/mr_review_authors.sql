-- MR review-comment author eligibility (issue #2347 M1) -----------------------
-- The verdict cache and the fair-progress queue behind the review-comment
-- eligibility assessment (workersvc.ReviewAssessor). Every queue MUTATION
-- (admit, requeue, prune, stale eviction) runs in its own short transaction that
-- first takes LockReviewAuthorQueue; the reads and the verdict writes do not.

-- name: LockReviewAuthorQueue :exec
-- Serializes every mutation of one (repo, ref) review-author queue. Two-int advisory
-- lock: class 1970958961 = 0x757A7271 ("uzrq"), the value of store.ReviewAuthorQueueLockClass
-- in migrate.go; TestReviewAuthorQueueLockClassMatchesSQL pins this literal to it. The objid
-- is hashtext of the repo and ref, so a collision merely serializes two unrelated queues for
-- a moment. XACT-scoped: released on commit or rollback, so it must run on a
-- transaction-bound Queries, and it is never held across a forge call.
SELECT pg_advisory_xact_lock(
    1970958961,
    hashtext(sqlc.arg(repo_id)::uuid::text || '/' || sqlc.arg(ref)::text)
);

-- name: ListFreshNotEligibleAuthors :many
-- The authors with a not-eligible verdict recorded at or after @since.
SELECT forge_user_id
FROM mr_review_author_verdicts
WHERE repo_id = @repo_id::uuid
  AND not_eligible_at >= @since::timestamptz;

-- name: UpsertReviewAuthorVerdict :exec
-- Record a not-eligible answer. GREATEST keeps the newest time if two writers race.
INSERT INTO mr_review_author_verdicts (repo_id, forge_user_id, not_eligible_at)
VALUES (@repo_id::uuid, @forge_user_id, @not_eligible_at::timestamptz)
ON CONFLICT (repo_id, forge_user_id) DO UPDATE
SET not_eligible_at = GREATEST(mr_review_author_verdicts.not_eligible_at, EXCLUDED.not_eligible_at);

-- name: DeleteExpiredReviewAuthorVerdicts :execrows
DELETE FROM mr_review_author_verdicts
WHERE repo_id = @repo_id::uuid
  AND not_eligible_at < @before::timestamptz;

-- name: ListReviewAuthorQueue :many
-- The queue in attempt order. An unlocked read: the caller uses the max queue_seq it
-- observes as the bound of its later prune.
SELECT forge_user_id, queue_seq
FROM mr_review_author_queue
WHERE repo_id = @repo_id::uuid AND ref = @ref
ORDER BY queue_seq;

-- name: AdmitReviewAuthors :exec
-- Admit missing authors at the back of the queue, in the order given. queue_seq is nextval of
-- mr_review_author_queue_seq, drawn in array order: the MATERIALIZED CTE computes each
-- sequence value exactly once per row, from a subquery sorted by array position (a sorted
-- subquery is not flattened into its parent, so the projection runs in sorted order). An
-- author already queued keeps its place (the conflicting row only burns a sequence value).
-- Run under LockReviewAuthorQueue.
WITH numbered AS MATERIALIZED (
    SELECT o.id, nextval('mr_review_author_queue_seq') AS seq
    FROM (
        SELECT u.id, u.ord
        FROM unnest(@forge_user_ids::bigint[]) WITH ORDINALITY AS u(id, ord)
        ORDER BY u.ord
    ) o
)
INSERT INTO mr_review_author_queue (repo_id, ref, forge_user_id, queue_seq)
SELECT @repo_id::uuid, @ref, n.id, n.seq
FROM numbered n
ORDER BY n.seq
ON CONFLICT (repo_id, ref, forge_user_id) DO NOTHING;

-- name: RequeueReviewAuthors :exec
-- Move the given authors to the back, in the order given, stamping the attempt time. Fresh
-- sequence values (drawn exactly as AdmitReviewAuthors does) are always above every existing
-- one. An author no longer queued is skipped. Run under LockReviewAuthorQueue.
WITH numbered AS MATERIALIZED (
    SELECT o.id, nextval('mr_review_author_queue_seq') AS seq
    FROM (
        SELECT u.id, u.ord
        FROM unnest(@forge_user_ids::bigint[]) WITH ORDINALITY AS u(id, ord)
        ORDER BY u.ord
    ) o
)
UPDATE mr_review_author_queue q
SET queue_seq = n.seq,
    last_attempt_at = now()
FROM numbered n
WHERE q.repo_id = @repo_id::uuid
  AND q.ref = @ref
  AND q.forge_user_id = n.id;

-- name: PruneReviewAuthorQueue :execrows
-- Drop the authors that are no longer candidates, but only rows the caller could have
-- observed: queue_seq <= @observed_max_seq. Because queue_seq comes from a sequence that is
-- never reset, a row admitted after the observation always sits above it, so a stale pruner
-- can never delete it. Run under LockReviewAuthorQueue.
DELETE FROM mr_review_author_queue
WHERE repo_id = @repo_id::uuid
  AND ref = @ref
  AND queue_seq <= @observed_max_seq::bigint
  AND NOT EXISTS (
      SELECT 1 FROM unnest(COALESCE(@keep_ids::bigint[], '{}'::bigint[])) AS k(id)
      WHERE k.id = mr_review_author_queue.forge_user_id
  );

-- name: ListStaleReviewAuthorQueueRefs :many
-- The refs of one repo that hold queue rows untouched for a long time.
SELECT DISTINCT ref
FROM mr_review_author_queue
WHERE repo_id = @repo_id::uuid
  AND GREATEST(admitted_at, last_attempt_at) < @before::timestamptz;

-- name: DeleteStaleReviewAuthorQueue :execrows
-- Evict rows untouched since @before. Run under LockReviewAuthorQueue.
DELETE FROM mr_review_author_queue
WHERE repo_id = @repo_id::uuid
  AND ref = @ref
  AND GREATEST(admitted_at, last_attempt_at) < @before::timestamptz;
