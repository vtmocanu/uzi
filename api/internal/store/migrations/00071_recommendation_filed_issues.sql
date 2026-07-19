-- +goose Up

-- PRD #68: the link from a judge recommendation to the forge issue filed from it.
--
-- Keyed on the (review_id, category, target) COORDINATE, not on a review_recommendations
-- row id — deliberately (Decision 6). review_recommendations rows are deleted-and-
-- reinserted on every re-judge (UpsertRunReviewWithRecommendations, judge.sql), so a
-- per-row link would have to be carried forward across the churn, and (category, target)
-- is not unique per review (the judge can emit two improve_agent rows for one agent),
-- so a carry-JOIN fans out. The run_reviews row itself is stable across a re-judge
-- (UNIQUE target_run_id, upserted not replaced), so keying the link on the review makes
-- it survive the recommendation delete-reinsert untouched and enforces
-- one-issue-per-coordinate-per-review by construction (the UNIQUE index below).
--
-- filing_since is the transient claim marker (Decision 7, mirror issue_proposals.
-- confirming_since / 00054_proposal_confirming.sql). The filing flow claims the
-- coordinate with an atomic INSERT ... ON CONFLICT DO NOTHING that stamps filing_since
-- BEFORE the forge CreateIssue, so of two concurrent POSTs exactly one wins; on forge
-- failure the winner's row is DELETEd (the claim reverts, coordinate fileable again),
-- on success filed_* is stamped and filing_since cleared. A crash between claim and
-- settle strands filing_since; the boot/interval sweeper deletes claims older than a
-- cutoff clamped >= 2x ForgeHTTPTimeout (Decision 7) — the revert being a DELETE, a
-- premature sweep during a live CreateIssue would let a retry re-INSERT and file a
-- SECOND forge issue, which the clamp prevents.
--
-- FK delete rules (Decision 6): review_id CASCADE (the link dies with the review,
-- correct — a re-judge keeps the review, only run deletion cascades it away);
-- filed_repo_id and filed_by_user_id SET NULL, so disconnecting an unrelated repo or
-- removing a user never destroys another run's filed link — filed_issue_url stays as
-- the durable pointer (matches produced_by_user_id's existing SET-NULL shape).
-- category has NO CHECK constraint here ON PURPOSE: the (category, target) coordinate is
-- always copied from an already-validated review_recommendations row (whose category IS
-- CHECK-constrained to the six-enum set, 00059), so a redundant CHECK would only couple
-- this table to that enum for no added safety. The handler never accepts a category from
-- the request body — it reads it off the resolved recommendation.
CREATE TABLE recommendation_filed_issues (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    review_id        uuid NOT NULL REFERENCES run_reviews ON DELETE CASCADE,
    category         text NOT NULL,
    target           text NOT NULL DEFAULT '',
    filed_repo_id    uuid REFERENCES repos ON DELETE SET NULL,
    filed_issue_iid  bigint,
    filed_issue_url  text NOT NULL DEFAULT '',
    filed_by_user_id uuid REFERENCES users ON DELETE SET NULL,
    filed_at         timestamptz,
    filing_since     timestamptz,
    -- The claim key: one issue per coordinate per review. Serves both the claim-first
    -- ON CONFLICT and the Decision 12 claimed-or-filed NOT EXISTS below.
    UNIQUE (review_id, category, target)
);

-- Decision 12: a filed (or mid-filing) improve_uzi recommendation drops out of the
-- self-improvement backlog, so a hand-filed issue and the engine's aggregated tracking
-- issue never cover the same coordinate twice. The backlog predicate in
-- selfimprove.sql (ListOpenImproveUziRecommendations) gains a CLAIMED-OR-FILED
-- NOT EXISTS against this table — the row EXISTING is the exclusion (not
-- filed_at IS NOT NULL), so an in-flight claim also excludes and a reverted (deleted)
-- claim re-includes the coordinate next cycle. That NOT EXISTS is the only mechanism
-- (a partial index on review_recommendations cannot reference another table); the
-- UNIQUE coordinate index above is what serves it. The predicate change ships as a
-- query edit compiled into the binary, not as DDL here — this note documents where it
-- lives so the two are read together.

-- +goose Down
DROP TABLE recommendation_filed_issues;
