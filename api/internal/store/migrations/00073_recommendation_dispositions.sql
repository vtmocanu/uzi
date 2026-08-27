-- +goose Up

-- PRD #94: the user's triage disposition on a judge recommendation — Done, or
-- Dismissed with a reason (wont_do / not_an_issue = false positive).
--
-- Keyed on the (review_id, category, target) COORDINATE, mirroring
-- recommendation_filed_issues (PRD #68, 00071) — deliberately NOT columns on
-- review_recommendations and NOT columns on recommendation_filed_issues (Decision 1).
-- review_recommendations rows are deleted-and-reinserted on every re-judge
-- (UpsertRunReviewWithRecommendations, judge.sql), so a per-row status would be wiped
-- on re-run; and (category, target) is not unique per review, so a carry-JOIN fans out.
-- The run_reviews row itself is STABLE across a re-judge (UNIQUE target_run_id, upserted
-- not replaced), so keying the disposition on the review makes it SURVIVE the
-- recommendation delete-reinsert untouched — dismiss a false positive, re-run the judge,
-- and it stays dismissed (Decision 3) — and enforces one-disposition-per-coordinate by
-- construction (the UNIQUE index below). recommendation_filed_issues is the wrong host
-- because a disposition routinely exists with no issue at all (dismiss-without-filing and
-- done-without-filing are the common cases) and its claim-first machinery is irrelevant
-- to a plain local state field (Decision 1).
--
-- status/dismiss_reason invariant: a dismissal MUST carry a reason and a done MUST NOT —
-- the table-level CHECK ties them so the enum can't drift out of sync (Decision 1/4).
--
-- rationale_hash is the sha256 of the recommendation's rationale_md at set-time
-- (Decision 3), an OPAQUE column here — the hash compare that drives the stale flag is
-- API-layer (M2/M5). Storing it (not a set_at < review.updated_at timestamp) is what
-- lets a byte-identical re-judge leave a dismissal quietly settled while a changed
-- rationale flags it stale; UpsertRunReview sets updated_at = now() on every conflict,
-- so a timestamp predicate would false-flag every re-judge.
--
-- FK delete rules (Decision 1): review_id CASCADE (the disposition dies with the review,
-- correct — a re-judge keeps the review, only run deletion cascades it away);
-- set_by_user_id SET NULL, so removing an unrelated user never destroys the disposition
-- row (matches produced_by_user_id / filed_by_user_id).
-- category has NO CHECK constraint here ON PURPOSE (identical to 00071): the
-- (category, target) coordinate is always copied from an already-validated
-- review_recommendations row (category CHECK-constrained to the six-enum set, 00059), so a
-- redundant CHECK would only couple this table to that enum for no added safety. The
-- handler never accepts a category from the request body — it reads it off the resolved
-- recommendation.
CREATE TABLE recommendation_dispositions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    review_id       uuid NOT NULL REFERENCES run_reviews ON DELETE CASCADE,
    category        text NOT NULL,
    target          text NOT NULL DEFAULT '',
    status          text NOT NULL CHECK (status IN ('done', 'dismissed')),
    dismiss_reason  text CHECK (dismiss_reason IN ('wont_do', 'not_an_issue')),
    -- A reason is present IFF the disposition is a dismissal (Decision 1/4).
    CHECK ((status = 'dismissed') = (dismiss_reason IS NOT NULL)),
    rationale_hash  text NOT NULL,
    set_by_user_id  uuid REFERENCES users ON DELETE SET NULL,
    set_at          timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- The disposition key: one disposition per coordinate per review. Serves the upsert
    -- ON CONFLICT, the correlated per-review + global lookups, and the Decision 9
    -- self-improve NOT EXISTS. No separate index needed (the UNIQUE serves them all).
    UNIQUE (review_id, category, target)
);

-- +goose Down
DROP TABLE recommendation_dispositions;
