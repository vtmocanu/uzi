package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ErrRecommendationNotFound — the recID does not resolve to a recommendation in the run's
// CURRENT review (an unknown id, or one re-judged away). The handler maps it to the SAME
// 404 as ErrRunNotFound so a disposition write leaks no ownership/existence oracle (PRD #94
// Decision 5).
var ErrRecommendationNotFound = errors.New("recommendation not found")

// RationaleHash is the stale-flag key (PRD #94 Decision 3): sha256 of a recommendation's
// rationale_md, hex-encoded, stamped on the disposition at set-time. A later re-judge that
// leaves the rationale byte-identical hashes the same (no stale flag); a changed rationale
// hashes differently (stale). Exported so the per-review DTO can recompute it for the
// current recommendation and compare against the stored hash.
func RationaleHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// resolveOwnedRecommendation resolves (runID, recID) to its review + recommendation under
// STRICT caller-ownership (PRD #94 Decision 5): isAdmin is HARDCODED false, so GetReviewForTarget
// degrades to the owner-scoped GetRun — a non-owner (including a uza_ admin_ro token that keeps
// IsAdmin) gets ErrRunNotFound, never an admin-reaching write. A recID absent from the current
// review (unjudged run, or re-judged away) is ErrRecommendationNotFound; both map to one 404.
func (s *Service) resolveOwnedRecommendation(ctx context.Context, ownerUserID, runID, recID uuid.UUID) (store.RunReview, store.ReviewRecommendation, error) {
	res, err := s.GetReviewForTarget(ctx, ownerUserID, false, runID)
	if err != nil {
		return store.RunReview{}, store.ReviewRecommendation{}, err // ErrRunNotFound when not owner-visible
	}
	if res == nil {
		return store.RunReview{}, store.ReviewRecommendation{}, ErrRecommendationNotFound // visible, unjudged
	}
	rec, found := findRecommendationByID(res.Recommendations, recID)
	if !found {
		return store.RunReview{}, store.ReviewRecommendation{}, ErrRecommendationNotFound
	}
	return res.Review, rec, nil
}

// findRecommendationByID looks a recommendation up by id within a review's current set.
func findRecommendationByID(recs []store.ReviewRecommendation, id uuid.UUID) (store.ReviewRecommendation, bool) {
	for _, rc := range recs {
		if rc.ID == id {
			return rc, true
		}
	}
	return store.ReviewRecommendation{}, false
}

// SetDisposition records the owner's triage verdict on one recommendation — an idempotent
// upsert on the coordinate (PRD #94 Decision 6): no token spend, no forge write. OWNER-ONLY
// (Decision 5): resolved by strict caller-ownership, so a non-owner gets a typed not-found.
// reason is stored NULL when empty (a 'done'); the status/reason enum is validated by the
// handler and backstopped by the table CHECK, not here. rationale_hash is re-stamped from
// the current recommendation's rationale_md (Decision 3) so the stale flag stays honest.
func (s *Service) SetDisposition(ctx context.Context, ownerUserID, runID, recID uuid.UUID, status, reason string) error {
	review, rec, err := s.resolveOwnedRecommendation(ctx, ownerUserID, runID, recID)
	if err != nil {
		return err
	}
	_, err = s.q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID:      review.ID,
		Category:      rec.Category,
		Target:        rec.Target,
		Status:        status,
		DismissReason: pgText(reason), // "" → NULL (a 'done' carries no reason)
		RationaleHash: RationaleHash(rec.RationaleMd),
		SetByUserID:   pgUUID(ownerUserID),
	})
	return err
}

// DeleteDisposition is Undo (PRD #94 Decision 6): delete the coordinate row, returning the
// recommendation to whatever the settled-filed axis says. OWNER-ONLY (Decision 5). A recID
// with no disposition (0 rows deleted) is ErrRecommendationNotFound so the handler 404s.
func (s *Service) DeleteDisposition(ctx context.Context, ownerUserID, runID, recID uuid.UUID) error {
	review, rec, err := s.resolveOwnedRecommendation(ctx, ownerUserID, runID, recID)
	if err != nil {
		return err
	}
	rows, err := s.q.DeleteRecommendationDisposition(ctx, store.DeleteRecommendationDispositionParams{
		ReviewID: review.ID,
		Category: rec.Category,
		Target:   rec.Target,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrRecommendationNotFound
	}
	return nil
}

// JudgeTriageStats is the global "across all your runs" strip (PRD #94 Decision 8): a flat
// join over the caller's reviews yielding one row per recommendation, bucketed through the
// SAME ladder as the per-review DTO. Owner-scoped by the query's user_id filter.
func (s *Service) JudgeTriageStats(ctx context.Context, ownerUserID uuid.UUID) (apitypes.TriageDTO, error) {
	rows, err := s.q.ListJudgeTriageRowsForUser(ctx, ownerUserID)
	if err != nil {
		return apitypes.TriageDTO{}, err
	}
	tr := make([]TriageRow, 0, len(rows))
	for _, r := range rows {
		tr = append(tr, TriageRow{
			Status:       r.DispositionStatus.String, // "" when the LEFT JOIN found no disposition
			Reason:       r.DismissReason.String,
			FiledSettled: r.FiledSettled,
		})
	}
	return BucketTriage(tr), nil
}
