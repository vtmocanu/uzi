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
-- They stay triggerable once their author resolves eligible; bounded to the newest 200.
ALTER TABLE mr_rework_ledger
    ADD COLUMN pending_unknown_ids bigint[] NOT NULL DEFAULT '{}',
    ADD CONSTRAINT mr_rework_ledger_pending_unknown_ids_check CHECK (cardinality(pending_unknown_ids) <= 200);

-- The one definition of the pending-set update, shared by the automatic upsert and the
-- on-demand atomic create: the newest 200 (largest ids) of (existing UNION added ids above
-- the high-water mark the row had BEFORE this update) minus removed ids.
-- +goose StatementBegin
CREATE FUNCTION mr_rework_merge_pending(
    existing bigint[],
    added bigint[],
    removed bigint[],
    prior_high_water bigint
) RETURNS bigint[]
LANGUAGE sql IMMUTABLE
AS $$
    SELECT COALESCE(array_agg(t.x ORDER BY t.x), '{}'::bigint[])
    FROM (
        SELECT u.x
        FROM (
            SELECT unnest(COALESCE(existing, '{}'::bigint[])) AS x
            UNION
            SELECT a AS x FROM unnest(COALESCE(added, '{}'::bigint[])) AS a WHERE a > prior_high_water
        ) u
        WHERE u.x <> ALL (COALESCE(removed, '{}'::bigint[]))
        ORDER BY u.x DESC
        LIMIT 200
    ) t
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION mr_rework_merge_pending(bigint[], bigint[], bigint[], bigint);
ALTER TABLE mr_rework_ledger
    DROP CONSTRAINT mr_rework_ledger_pending_unknown_ids_check,
    DROP COLUMN pending_unknown_ids;
DROP TABLE mr_review_author_queue;
DROP SEQUENCE mr_review_author_queue_seq;
DROP TABLE mr_review_author_verdicts;
