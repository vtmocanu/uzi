-- Recommendation-filed-issue queries (PRD #68). The link from a judge recommendation
-- to the forge issue filed from it, keyed on the (review_id, category, target)
-- coordinate. The filing flow is claim-first (Decision 7, mirror ConfirmProposal +
-- 00054): Claim BEFORE the forge write, then Settle on success / Revert on failure,
-- with Sweep reaping claims stranded by a crash between the two. See
-- 00071_recommendation_filed_issues.sql for the table rationale.

-- name: ClaimRecommendationFiledIssue :one
-- Atomic claim-first (Decision 7). INSERT the claim row stamping filing_since; ON
-- CONFLICT on the coordinate DO NOTHING, so a second concurrent POST — or a coordinate
-- that is already filed OR mid-filing — returns NO row (pgx.ErrNoRows), which the
-- handler maps to 409. The winner's row is settled after a successful CreateIssue
-- (SettleRecommendationFiledIssue) or reverted on forge failure
-- (RevertRecommendationFiledIssue). filed_by_user_id records the claimant now; it
-- survives to settle and is dropped with the row on revert/sweep.
INSERT INTO recommendation_filed_issues (review_id, category, target, filed_by_user_id, filing_since)
VALUES (@review_id, @category, @target, @filed_by_user_id, now())
ON CONFLICT (review_id, category, target) DO NOTHING
RETURNING id;

-- name: RevertRecommendationFiledIssue :exec
-- Forge CreateIssue failed after we won the claim: delete the claim row so the
-- coordinate is fileable again (Decision 7/9, mock state E). Guarded on filed_at IS
-- NULL so a settled row is never destroyed by a late/duplicate revert.
DELETE FROM recommendation_filed_issues WHERE id = @id AND filed_at IS NULL;

-- name: SettleRecommendationFiledIssue :execrows
-- The forge CreateIssue succeeded: stamp the issue coordinates onto the claim we won
-- and clear filing_since (Decision 7/9). BY ID and guarded filed_at IS NULL. Returns
-- rows affected: 0 means the claim was swept out from under a slow-but-successful
-- CreateIssue (Decision 7) — the handler treats that as CREATED-WITH-WARNING, never a
-- forge retry.
UPDATE recommendation_filed_issues
SET filed_repo_id    = @filed_repo_id,
    filed_issue_iid  = @filed_issue_iid,
    filed_issue_url  = @filed_issue_url,
    filed_at         = now(),
    filing_since     = NULL
WHERE id = @id AND filed_at IS NULL;

-- name: ListFiledIssuesForReview :many
-- The filed-issue links for a review (M2/M4 panel). Returns every coordinate that has
-- been filed OR is being filed under this review, so the panel can render the filed
-- state, the stale-filed flag (filed_at < review.updated_at), and block sibling
-- coordinates (the target='' collapse, Decision 6). Settled rows first, then any live
-- claim; stable id tiebreak.
SELECT * FROM recommendation_filed_issues
WHERE review_id = @review_id
ORDER BY filed_at ASC NULLS LAST, id ASC;

-- name: SweepStrandedRecommendationClaims :execrows
-- Boot/interval sweeper (Decision 7, mirror SweepClaimedNeverStarted for stale run
-- claims): delete claim rows stranded by a crash between claim and settle — filed_at
-- still NULL and filing_since older than @cutoff. @cutoff MUST be clamped
-- >= 2x ForgeHTTPTimeout by the caller (config.go clamp precedent) so a slow-but-alive
-- CreateIssue is never reverted mid-flight: the revert is a DELETE, so a premature
-- sweep would let a retry / concurrent POST re-INSERT and file a SECOND forge issue —
-- the exact duplicate the claim-first design exists to prevent. Returns rows deleted
-- (for the sweeper log).
DELETE FROM recommendation_filed_issues
WHERE filed_at IS NULL AND filing_since IS NOT NULL AND filing_since < @cutoff;
