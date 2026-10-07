-- +goose Up

-- Issue #2347 M1: author eligibility for the MR review-comment rework lane.
--
-- mr_review_author_verdicts remembers a NOT-ELIGIBLE answer for one author on one repo for a
-- short TTL (applied by the reader, 6 h), so an outsider that keeps commenting costs one
-- forge lookup per TTL rather than one per tick. Only not-eligible answers are stored: an
-- eligible or unknown answer is always re-asked.
CREATE TABLE mr_review_author_verdicts (
    repo_id         uuid        NOT NULL REFERENCES repos ON DELETE CASCADE,
    forge_user_id   bigint      NOT NULL,
    not_eligible_at timestamptz NOT NULL,
    PRIMARY KEY (repo_id, forge_user_id)
);

-- queue_seq of mr_review_author_queue comes from this sequence and from nothing else. It is
-- bigint, never cycles, and nextval is not rolled back or reused when rows are deleted, so a
-- sequence value is never handed out twice. That is what makes the "observed max" stale-prune
-- guard safe: a pruner that observed max M can only delete rows with queue_seq <= M, and a
-- row admitted after the observation always has queue_seq > M, even when every earlier row
-- was deleted in between. A max(queue_seq) + 1 scheme would restart at 1 after such a delete
-- and let the stale pruner remove a waiting author.
--
-- The guard needs values that are MONOTONIC IN LOCK ORDER: a mutation that takes the
-- per-(repo, ref) advisory lock later must draw a larger value than one that took it earlier.
-- That is why the sequence is pinned CACHE 1 NO CYCLE instead of left to defaults. With a
-- larger CACHE each session preallocates a private block, so a later lock holder can draw a
-- value below an earlier holder's (session A caches 1..20, B caches 21..40, B admits at 21,
-- then A admits at 2) and a pruner that observed 21 would delete the row at 2. NO CYCLE keeps
-- a wrapped value from ever repeating. Do not raise CACHE or add CYCLE;
-- TestReviewAuthorQueueStalePruneNeverDeletesReadmittedLiveDB asserts both settings.
CREATE SEQUENCE mr_review_author_queue_seq AS bigint CACHE 1 NO CYCLE;

-- The fair-progress queue of authors whose eligibility is still to be looked up, one
-- ordered queue per (repo, ref). Mutations run under the per-(repo, ref) advisory lock
-- (LockReviewAuthorQueue); the UNIQUE (repo_id, ref, queue_seq) key is a backstop.
CREATE TABLE mr_review_author_queue (
    repo_id         uuid        NOT NULL REFERENCES repos ON DELETE CASCADE,
    ref             text        NOT NULL,
    forge_user_id   bigint      NOT NULL,
    queue_seq       bigint      NOT NULL,
    admitted_at     timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz NULL,
    PRIMARY KEY (repo_id, ref, forge_user_id),
    UNIQUE (repo_id, ref, queue_seq)
);

-- Comment ids whose author was permission-unknown when the high-water mark moved past them.
-- They stay triggerable once their author resolves eligible. The set holds one representative
-- id per unverified author (the Go planner, workersvc ReviewPlan, chooses it), so it is bounded
-- by distinct authors; the cap of 10000 is a backstop, and on overflow the OLDEST ids are kept
-- so a flood of newer comments can never evict an earlier, possibly eligible author's id.
ALTER TABLE mr_rework_ledger
    ADD COLUMN pending_unknown_ids bigint[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT mr_rework_ledger_pending_unknown_ids_check CHECK (cardinality(pending_unknown_ids) <= 10000);

-- The one definition of the pending-set update, shared by the automatic upsert and the
-- on-demand atomic create. The base set is (existing UNION added ids above the high-water mark
-- the row had BEFORE this update) EXCEPT removed ids. Supersession is CONDITIONAL: the parallel
-- arrays superseded / superseded_by name pairs (an author's older pending id, that author's newer
-- representative). A pair applies only when BOTH ids are in the base set, i.e. the replacement
-- survived this statement's own add filter (a stale writer's add can be rejected by
-- prior_high_water) and was not removed. An older id is dropped only when it is not itself the
-- replacement of another applied pair. The kept ids are then ranked by SLOT: a replacement
-- takes the slot of the smallest older id it replaces, so replacing an id never moves its author
-- to the back of the oldest-first order, and the oldest 10000 slots survive. When the cap cuts,
-- the replacement is therefore never dropped in favour of keeping the older id, and an older
-- author is never evicted by a newer one. EXCEPT is a set operation (hashed or sorted), so a
-- 10000-id removal stays O(n log n) where an ALL(array) comparison would be quadratic.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_merge_pending(
    existing bigint[],
    added bigint[],
    removed bigint[],
    superseded bigint[],
    superseded_by bigint[],
    prior_high_water bigint
) RETURNS bigint[]
LANGUAGE sql IMMUTABLE
AS $$
  WITH base AS (
    (SELECT unnest(COALESCE(existing,'{}'::bigint[])) AS x
     UNION
     SELECT a FROM unnest(COALESCE(added,'{}'::bigint[])) AS a WHERE a > prior_high_water)
    EXCEPT SELECT r FROM unnest(COALESCE(removed,'{}'::bigint[])) AS r
  ),
  pairs AS (
    SELECT DISTINCT p.o, p.n
    FROM unnest(COALESCE(superseded,'{}'::bigint[]), COALESCE(superseded_by,'{}'::bigint[])) AS p(o,n)
    WHERE p.o IS NOT NULL AND p.n IS NOT NULL AND p.o <> p.n
  ),
  eff AS (SELECT pairs.o, pairs.n FROM pairs
          JOIN base bo ON bo.x = pairs.o JOIN base bn ON bn.x = pairs.n),
  dropped AS (SELECT o AS x FROM eff EXCEPT SELECT n FROM eff),
  kept AS (SELECT x FROM base EXCEPT SELECT x FROM dropped),
  slotted AS (SELECT k.x, COALESCE(s.slot, k.x) AS slot FROM kept k
              LEFT JOIN (SELECT n, min(o) AS slot FROM eff GROUP BY n) s ON s.n = k.x)
  SELECT COALESCE(array_agg(t.x ORDER BY t.x), '{}'::bigint[])
  FROM (SELECT x FROM slotted ORDER BY LEAST(slot, x), x LIMIT 10000) t
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION mr_rework_merge_pending(bigint[], bigint[], bigint[], bigint[], bigint[], bigint);
ALTER TABLE mr_rework_ledger
    DROP CONSTRAINT mr_rework_ledger_pending_unknown_ids_check,
    DROP COLUMN pending_unknown_ids;
DROP TABLE mr_review_author_queue;
DROP SEQUENCE mr_review_author_queue_seq;
DROP TABLE mr_review_author_verdicts;
