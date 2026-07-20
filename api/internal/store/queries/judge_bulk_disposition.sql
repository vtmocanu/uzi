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
-- DISTINCT ON is REQUIRED, and it is a correctness fix rather than a tidy-up.
-- review_recommendations has NO unique constraint on (review_id, category, target) — only
-- pkey(id), idx(review_id) and the partial improve_uzi index — so a judge may legitimately
-- emit the same coordinate twice in one review (M1's grouper comment says so, and
-- TestGroupJudgeRecommendationsRunCountIsDistinctRuns pins it). Without this, such a review
-- yields the same coordinate twice, and feeding both into the single multi-row upsert makes
-- Postgres raise SQLSTATE 21000, "ON CONFLICT DO UPDATE command cannot affect row a second
-- time" — at runtime, only for the users whose judge happened to duplicate, and invisible to
-- every fake. The per-member loop this replaced was immune because each upsert was its own
-- statement; changing the shape removed the reason it was safe.
--
-- One row per coordinate is also simply the right GRAIN: recommendation_dispositions is
-- keyed on (review_id, category, target), so two recommendations sharing a coordinate share
-- ONE disposition.
--
-- WHICH duplicate survives matters for exactly one of the two things the write stores, and
-- the distinction is worth stating because the convenient half is the one that is obvious:
--
--   * The LADDER verdict is genuinely unaffected. Both duplicates carry identical
--     disposition/filed state, because those LEFT JOINs are on the coordinate, not on rr.id
--     — there is only one disposition row and one filed row to find.
--   * The rationale_HASH is NOT. Duplicates are separate recommendations with their own
--     rationale_md, and the surviving row supplies the hash the caller stamps — which is
--     #94 Decision 3's staleness key, so it decides whether this coordinate later shows as
--     "the recommendation changed since you resolved it".
--
-- ORDER BY … rr.created_at ASC, rr.id ASC makes that deterministic: the OLDEST
-- recommendation on the coordinate wins, so the stale flag is measured against the text
-- that has been there longest rather than against whichever row the planner happened to
-- return first. A later re-judge that rewrites either duplicate still flips the flag; what
-- this ordering buys is that it flips for a stable reason.
-- NOTE there is deliberately no rec_id here. It used to be projected and read by nothing,
-- which was not merely dead weight: an unused per-recommendation id is a standing claim
-- that per-recommendation granularity still matters on this path, and that assumption is
-- exactly what made the duplicate-coordinate crash invisible. The write is keyed on the
-- COORDINATE, so the coordinate is what this returns. If a future change needs the
-- recommendation id, it must first answer what it means when two share a coordinate.
SELECT DISTINCT ON (rv.id, rr.category, rr.target)
    rv.id                          AS review_id,
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
ORDER BY rv.id, rr.category, rr.target, rr.created_at ASC, rr.id ASC;

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
        -- Literal NULL, not EXCLUDED.set_via — see dispositions.sql for why the EXCLUDED
        -- form is a latent trap. A human write always means human provenance.
        set_via        = NULL,
        set_at         = now(),
        updated_at     = now();
