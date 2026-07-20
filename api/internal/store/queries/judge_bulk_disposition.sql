-- The Judge menu's bulk-disposition resolve (PRD #98 M2, Decision 3). The group is a
-- display construct, so "dispose a group" means "upsert a disposition on each member
-- coordinate the caller owns" — and THIS query is the security boundary of that fan-out.

-- name: ListOwnedRecommendationsForCoords :many
-- Resolve the caller's member recommendations for a set of (category, target) coordinates.
--
-- SECURITY, not convenience — this is the ONLY place the bulk endpoint learns which rows
-- exist. Two properties it must keep:
--
--  1. `rv.user_id = @user_id` makes the resolve strictly owner-scoped, so a coordinate that
--     belongs to another user resolves to ZERO members and the caller cannot tell it apart
--     from one that does not exist (PRD #94 Decision 5's one-404 rule — no existence
--     oracle). `IsAdmin` is never consulted anywhere on this path.
--  2. Only real recommendation coordinates come back. 00071 and 00073 both omit a category
--     CHECK *on purpose*, stating that "the handler never accepts a category from the
--     request body — it reads it off the resolved recommendation", so a bogus or oversized
--     body coordinate must match nothing rather than reach the coordinate columns.
--
--     Credit where it is due: the guarantee comes from the `JOIN want ON want.category =
--     rr.category AND want.target = rr.target`, NOT from the SELECT list naming rr.*. The
--     join equates the two sides, so selecting want.category instead would be behaviourally
--     identical (verified by mutation, PRD #98 M2 review). The SELECT list is written off
--     `rr` because that is the honest source and it keeps the Go caller obviously correct —
--     but do not mistake it for the enforcement, and do not "simplify" the JOIN on the
--     theory that the SELECT list is what protects this.
--
-- The requested coordinates arrive as two PARALLEL arrays zipped by ordinal into `want`,
-- so it is the exact list of (category, target) pairs, never their cross product. The
-- caller de-duplicates before binding, so a repeated coordinate cannot inflate the fan-out.
--
-- disposition_status + filed_settled feed the Go BucketOf ladder so scope=open can skip
-- members that are not `todo` (PRD #98 Decision 2 — a FILED member is not open). As
-- everywhere else in this PRD there is deliberately NO SQL CASE: the ladder is one Go
-- helper. rationale_md is projected because the upsert re-stamps rationale_hash from the
-- CURRENT rationale (#94 Decision 3, the stale flag's key). Both side-table joins are
-- UNIQUE on the coordinate, so neither fans out.
WITH want AS (
    -- The two arrays are zipped BY ORDINAL, so row i is exactly (categories[i],
    -- targets[i]) — a pairwise list, never the cross product a naive two-array join would
    -- produce. (Postgres's multi-argument unnest(a, b) says the same thing in one call,
    -- but sqlc's analyzer cannot type its arguments, so this is the portable spelling.)
    SELECT c.val AS category, t.val AS target
    FROM unnest(@categories::text[]) WITH ORDINALITY AS c(val, ord)
    JOIN unnest(@targets::text[]) WITH ORDINALITY AS t(val, ord) ON t.ord = c.ord
)
SELECT
    rv.id                          AS review_id,
    rr.id                          AS rec_id,
    rr.category                    AS category,
    rr.target                      AS target,
    rr.rationale_md                AS rationale_md,
    d.status                       AS disposition_status,
    (f.filed_at IS NOT NULL)::bool AS filed_settled
FROM run_reviews rv
JOIN review_recommendations rr ON rr.review_id = rv.id
JOIN want ON want.category = rr.category AND want.target = rr.target
LEFT JOIN recommendation_dispositions d
    ON d.review_id = rv.id AND d.category = rr.category AND d.target = rr.target
LEFT JOIN recommendation_filed_issues f
    ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target
WHERE rv.user_id = @user_id
ORDER BY rr.category ASC, rr.target ASC, rv.created_at DESC, rr.id ASC;
