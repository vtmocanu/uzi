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
--     Be precise about WHAT enforces that, because the obvious answer is wrong. The
--     mechanism is this JOIN plus the owner predicate: together they yield ZERO ROWS for a
--     coordinate that does not exist or is not the caller's, and a row that does not come
--     back cannot be written. Selecting `rr.category` rather than `want.category` is
--     DEFENCE IN DEPTH, not the mechanism — the join equates the two sides, so the swap is
--     behaviourally inert today (verified by mutation, PRD #98 M2 review).
--
--     The two stop being equivalent the moment the match is loosened — case-insensitive,
--     LIKE, trimming, collation-dependent equality — at which point `want.*` would write
--     the CALLER's spelling and `rr.*` the stored one. That is exactly when writing from
--     the resolved row earns its keep, and exactly when a reader who believed them
--     interchangeable would introduce the bug. So: keep the SELECT list on `rr`, and do not
--     loosen the JOIN. No test can distinguish the two forms today; that is inherent to an
--     equality join, not a coverage gap.
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

-- name: UpsertDispositionsForResolvedCoords :execrows
-- Write the whole fan-out in ONE statement (PRD #98 M2, audit NB-A).
--
-- WHY ONE STATEMENT, not a loop of #94's single-coordinate upsert. The item cap bounds
-- COORDINATES (100), but not MEMBERS: one coordinate matches every occurrence across all
-- the caller's reviews, and the resolve has no LIMIT. So ≤100 coordinates in a ~4 KB body
-- could drive tens of thousands of sequential round-trips, each holding a pool connection,
-- on a mount with no rate limiter. Own-data and idempotent, so not a vulnerability — but it
-- left this endpoint materially less bounded than the M1 read, which caps at 2000 rows.
-- Collapsing to one statement removes the amplification AND the partial-apply window
-- together: a single statement cannot half-succeed, so there is no "some writes landed but
-- the response says nothing" state to document or test around.
--
-- THE COORDINATES ARE THE RESOLVED ONES. The caller passes the review_id / category /
-- target / rationale_hash it read back from ListOwnedRecommendationsForCoords above — never
-- anything off the request body — so the 00071/00073 no-category-CHECK invariant holds
-- exactly as it did in the loop. review_ids is what makes that airtight: it names rows the
-- owner-scoped resolve already returned, so a caller cannot address a review it does not
-- own even if it forged the category and target.
--
-- The four arrays are zipped BY ORDINAL into one member per row — a pairwise list, never a
-- cross product (same shape sqlc forced on the read side above: it cannot type a
-- multi-argument unnest, so each array is unnested separately and joined on ordinality).
--
-- ON CONFLICT DO UPDATE is #94's own last-writer-wins upsert semantics, unchanged
-- (Decision 6) — deliberately NOT the DO NOTHING the Filed→Done sync uses, because this IS
-- the human speaking and their latest verdict must win. set_via is cleared for the same
-- reason it is cleared in dispositions.sql: this row is now a person's, not the sync's.
-- :execrows returns members actually written, which the handler reports as an aggregate
-- `updated` (never per-item — that would rebuild the existence oracle #94 Decision 5
-- forbids).
WITH members AS (
    SELECT r.val AS review_id, c.val AS category, t.val AS target, h.val AS rationale_hash
    FROM unnest(@review_ids::uuid[]) WITH ORDINALITY AS r(val, ord)
    JOIN unnest(@categories::text[]) WITH ORDINALITY AS c(val, ord) ON c.ord = r.ord
    JOIN unnest(@targets::text[]) WITH ORDINALITY AS t(val, ord) ON t.ord = r.ord
    JOIN unnest(@rationale_hashes::text[]) WITH ORDINALITY AS h(val, ord) ON h.ord = r.ord
)
INSERT INTO recommendation_dispositions
    (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
SELECT m.review_id, m.category, m.target, @status, sqlc.narg('dismiss_reason'), m.rationale_hash,
       sqlc.narg('set_by_user_id')
FROM members m
ON CONFLICT (review_id, category, target) DO UPDATE
    SET status         = EXCLUDED.status,
        dismiss_reason = EXCLUDED.dismiss_reason,
        rationale_hash = EXCLUDED.rationale_hash,
        set_by_user_id = EXCLUDED.set_by_user_id,
        set_via        = EXCLUDED.set_via, -- NULL: a human write clears the sync's provenance
        set_at         = now(),
        updated_at     = now();
