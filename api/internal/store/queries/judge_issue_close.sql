-- Filed→Done sync (PRD #98 Decision 6, M6). Rides the existing poller tick: after a repo's
-- issue cache is refreshed, any recommendation whose filed issue has just been observed
-- closed moves to Done, exactly once, without ever overwriting a human's own verdict.

-- name: ListFiledIssueCloseEdges :many
-- The pass's working set for one repo: SETTLED filed links whose cached issue is closed and
-- whose open→closed EDGE has not yet been consumed.
--
-- THE JOIN KEY IS (repo_id, forge_issue_iid), NEVER forge_issue_iid ALONE. A forge issue
-- iid is per-project, not global — `issues` is itself keyed `ON CONFLICT (repo_id,
-- forge_issue_iid)` (forge.sql). Joining on iid alone would let closing issue #7 in repo X
-- auto-Done a recommendation filed as #7 into repo Y: cross-repo, and since reviews are
-- owner-scoped while filed rows are not (#68 Decision 8), possibly cross-USER. The
-- `f.filed_repo_id = @repo_id` equality also excludes NULL-repo rows (filed_repo_id is
-- ON DELETE SET NULL), which is what makes the documented "a filed issue whose repo was
-- disabled or disconnected won't auto-Done" limitation a SAFE no-op rather than merely a
-- silent one — such a row can never be matched to some other repo's issue of the same iid.
--
-- The other filters, each load-bearing:
--   * f.filed_at IS NOT NULL — only SETTLED links (an in-flight #68 claim is not filed, the
--     same rung the shared BucketOf ladder uses).
--   * f.close_synced_at IS NULL — the EDGE. Without it the pass is level-triggered and
--     re-fires every tick for as long as the issue stays closed, which would re-apply after
--     a human Undo.
--   * i.state = 'closed' — the cached snapshot. A reopen is NOT handled here on purpose:
--     close_synced_at stays stamped, so there is no auto-reopen and a flapping issue cannot
--     ping-pong a user's backlog.
--
-- rationale_md comes from a scalar SUBQUERY, not a join, so a review that carries the same
-- coordinate twice (review_recommendations has no unique constraint on it) cannot fan this
-- out into duplicate edges for one filed link.
--
-- The COALESCE(…, '')::text is REQUIRED, not cosmetic. The subquery yields NULL when the
-- recommendation was re-judged away while its filed link survived, but sqlc types a bare
-- scalar subquery as a NON-nullable string — so the generated Scan would fail at runtime on
-- exactly that row, and only against a real database. Coalescing makes the Go type honest;
-- the caller then hashes the empty string, which is harmless because the hash only ever
-- lands on an INSERT that no human verdict is competing with.
SELECT
    f.id              AS filed_id,
    f.review_id       AS review_id,
    f.category        AS category,
    f.target          AS target,
    f.filed_issue_iid AS filed_issue_iid,
    COALESCE((
        SELECT rr.rationale_md
        FROM review_recommendations rr
        WHERE rr.review_id = f.review_id
          AND rr.category = f.category
          AND rr.target = f.target
        ORDER BY rr.created_at ASC, rr.id ASC
        LIMIT 1
    ), '')::text AS rationale_md
FROM recommendation_filed_issues f
JOIN issues i
    ON i.repo_id = f.filed_repo_id
   AND i.forge_issue_iid = f.filed_issue_iid
-- The ::uuid cast keeps the parameter NON-nullable in Go. filed_repo_id is nullable, so
-- without it sqlc would hand the caller a pgtype.UUID and invite someone to pass an
-- absent/zero value here — which, per the pgUUID contract, would silently match nothing.
-- The repo id always exists at every call site (it comes from the poller's repo row).
WHERE f.filed_repo_id = @repo_id::uuid
  AND f.filed_at IS NOT NULL
  AND f.close_synced_at IS NULL
  AND i.state = 'closed'
ORDER BY f.id ASC
-- Bounded per tick (audit B9). Edges are near-zero in practice and the partial index is
-- exactly this working set, but a mass issue closure would otherwise make one repo's tick
-- run a statement per edge serially and delay every repo queued behind it. The remainder is
-- picked up next tick: nothing is lost, because an unconsumed edge stays unconsumed. This is
-- the third enumeration in this PRD to need a bound (M1's rows, M2's fan-out, now this) —
-- bound them where they are written.
--
-- Note what the ORDER BY does and does not give. f.id is a random uuid (gen_random_uuid),
-- so this is stable and arbitrary — NOT oldest-close-first. There is no ordering by close
-- time here, and none is needed: every pending edge is applied eventually, and the order
-- among them carries no meaning. Do not read "FIFO" as "the earliest close wins".
--
-- The bound is FIFO by that arbitrary id, and a FAILING edge is not consumed — so a persistent poison edge
-- HOLDS ITS SLOT in every subsequent batch. The usual drain argument ("applied edges leave
-- the working set permanently, so the set strictly shrinks") is true and has exactly this
-- exception: if a full batch of low-id edges failed on every tick, newer edges would never
-- enter the window. Left as-is deliberately — it needs @lim edges failing simultaneously and
-- indefinitely, which the CTE'''s realistic failure modes (a dead DB, which stops the whole
-- tick anyway) do not produce. Written down so the exception is known rather than
-- rediscovered.
LIMIT @lim;

-- name: ApplyFiledIssueCloseEdge :one
-- Apply ONE close edge atomically: write the automatic Done and consume the edge in a
-- SINGLE statement.
--
-- WHY ONE STATEMENT rather than two calls (even in the right order). The insert and the
-- stamp used to be separate round-trips with no transaction. If the process died — or the
-- stamp errored — in between, the edge stayed open, which is the safe direction ONLY as
-- long as nothing else changed meanwhile. But if the user hit **Undo before the next tick**,
-- the retry's insert was NOT a no-op: the row was gone, so it re-applied Done and DEFEATED
-- THE UNDO. Narrow (needs a crash or DB error in that window plus an Undo before the next
-- tick) but real, and it is exactly the guarantee M6 exists to provide. In one statement the
-- window does not exist: either both happen or neither does.
--
-- Postgres executes data-modifying CTEs exactly once and always to completion, independently
-- of whether the primary query reads their output — so `disposed` running dry does not stop
-- `stamped`. That is deliberate: the edge is consumed even when the insert wrote nothing
-- (the user already had a verdict), otherwise the pass would re-examine that row on every
-- future tick forever.
--
-- The insert is ON CONFLICT DO NOTHING, emphatically NOT #94's DO-UPDATE upsert: it is the
-- entire "never overwrite a human verdict" guarantee. Provenance is fixed in the query text
-- rather than parameterised — status 'done', set_via 'issue_close', set_by_user_id NULL —
-- so no call site can pass filed_by_user_id (who may be an admin acting on someone else's
-- review, #68 Decision 8) by mistake.
--
-- The stamp is itself guarded `close_synced_at IS NULL`, so two concurrent pollers cannot
-- both consume one edge. Returns both counts so the caller can log an actual auto-resolve
-- without a second read.
WITH ins AS (
    INSERT INTO recommendation_dispositions
        (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id, set_via)
    VALUES
        (@review_id, @category, @target, 'done', NULL, @rationale_hash, NULL, 'issue_close')
    ON CONFLICT (review_id, category, target) DO NOTHING
    RETURNING 1
),
stamped AS (
    UPDATE recommendation_filed_issues f
    SET close_synced_at = now()
    WHERE f.id = @filed_id AND f.close_synced_at IS NULL
    RETURNING 1
)
SELECT
    (SELECT count(*) FROM ins)::bigint     AS disposed,
    (SELECT count(*) FROM stamped)::bigint AS stamped;
