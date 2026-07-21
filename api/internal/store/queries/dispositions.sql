-- Recommendation-disposition queries (PRD #94). The user's triage verdict on a judge
-- recommendation — Done, or Dismissed with a reason — keyed on the
-- (review_id, category, target) coordinate, mirroring recommendation_filed_issues
-- (PRD #68). Setting a disposition is a single local upsert: no token spend, no forge
-- write, no claim-first dance (there is no external side effect to make exactly-once,
-- so concurrent writes converge on the last value and a delete is idempotent,
-- Decision 6). See 00073_recommendation_dispositions.sql for the table rationale.

-- name: UpsertRecommendationDisposition :one
-- Idempotent upsert on the coordinate (Decision 6): set/switch status, set/clear the
-- dismiss reason, and RE-STAMP rationale_hash + set_at + updated_at at set-time (Decision 3,
-- the hash is what the stale flag compares against). Re-clicking is safe, last-writer-wins.
-- set_at is re-stamped on conflict too so the panel's "resolved Xago" reflects the LATEST
-- disposition action (a done→dismissed switch or reason change reads as "just now", not the
-- first-ever set); the row is a single triage verdict, not an append log.
-- dismiss_reason and set_by_user_id are nullable (sqlc.narg) — a 'done' carries no reason
-- and a SET-NULL'd setter leaves the row. The status/reason invariant is enforced by the
-- table CHECK, not here.
INSERT INTO recommendation_dispositions
    (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
VALUES
    (@review_id, @category, @target, @status, sqlc.narg('dismiss_reason'), @rationale_hash,
     sqlc.narg('set_by_user_id'))
ON CONFLICT (review_id, category, target) DO UPDATE
    SET status         = EXCLUDED.status,
        dismiss_reason = EXCLUDED.dismiss_reason,
        rationale_hash = EXCLUDED.rationale_hash,
        set_by_user_id = EXCLUDED.set_by_user_id,
        -- set_via is CLEARED, not carried over (PRD #98 M6). This path is a HUMAN write,
        -- so the row's provenance becomes "a person set this". Without the reset, a
        -- coordinate previously auto-resolved by the Filed→Done sync would keep
        -- set_via='issue_close' after the user overrode it, and the panel would label
        -- their own verdict "done via #IID" — attributing a human decision to the system,
        -- the exact mirror of what PF-4 prevents.
        --
        -- Written as a literal NULL, NOT as EXCLUDED.set_via. The two are equivalent only
        -- because the INSERT column list above omits set_via; if anyone ever adds it there,
        -- the EXCLUDED form would silently start carrying system provenance through a human
        -- write with NO edit to this line. NULL states the invariant itself — a human write
        -- always means human provenance — instead of depending on a column list elsewhere
        -- in the same statement staying as it is.
        set_via        = NULL,
        set_at         = now(),
        updated_at     = now()
RETURNING *;

-- name: DeleteRecommendationDisposition :execrows
-- Undo (Decision 6): delete the coordinate row, returning the recommendation to whatever
-- the settled-filed axis says (settled link → "Filed", else "To do"). Idempotent — a
-- second delete affects 0 rows. Returns rows affected so the handler can 404 a coordinate
-- that carried no disposition.
DELETE FROM recommendation_dispositions
WHERE review_id = @review_id AND category = @category AND target = @target;

-- name: ListDispositionsForReview :many
-- Every disposition for a review (M2 per-review DTO). Stable order (set_at, id) so the
-- panel's dispByCoord map is built deterministically.
SELECT * FROM recommendation_dispositions
WHERE review_id = @review_id
ORDER BY set_at ASC, id ASC;

-- name: ListJudgeTriageRowsForUser :many
-- The GLOBAL flat join feeding the Go bucketer (Decision 8): from the caller's reviews,
-- ONE ROW PER RECOMMENDATION carrying its coordinate's disposition status + dismiss_reason
-- (both nullable — dismiss_reason feeds the not_an_issue false-positive sub-count) and
-- whether it has a SETTLED filed link (filed_at IS NOT NULL — an in-flight/unsettled claim
-- is NOT "filed", matching the per-review path). There is deliberately NO SQL CASE and no
-- bucketing here: the ladder (dismissed > done > filed > open) is one Go helper consumed by
-- both this global handler and the per-review DTO, so the two counters can't drift. Both
-- side-table joins are UNIQUE on the coordinate, so neither fans out. The ::bool cast is
-- REQUIRED — without it sqlc types the boolean expression column as interface{}.
SELECT
    d.status AS disposition_status,
    d.dismiss_reason AS dismiss_reason,
    (f.filed_at IS NOT NULL)::bool AS filed_settled
FROM run_reviews rv
JOIN review_recommendations rr ON rr.review_id = rv.id
LEFT JOIN recommendation_dispositions d
    ON d.review_id = rv.id AND d.category = rr.category AND d.target = rr.target
LEFT JOIN recommendation_filed_issues f
    ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target
WHERE rv.user_id = @user_id;
