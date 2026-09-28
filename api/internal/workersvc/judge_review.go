package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Judge review taxonomy (PRD #46 Decision 5). These sets mirror the run_reviews /
// review_recommendations CHECK constraints; the handler validates a worker's review
// POST against them at ingest and the DB CHECK is the backstop. Categories are the
// user's verbatim taxonomy (specs/human.md).
var (
	ReviewVerdicts = map[string]bool{"ideal": true, "ok": true, "issues": true}
	// ReviewStatuses: "complete" is a real LLM verdict; "failed" is the deterministic
	// fallback written when the model call failed (Decision 4) — recommendations
	// (e.g. the command-not-found hits) still land.
	ReviewStatuses           = map[string]bool{"complete": true, "failed": true}
	RecommendationCategories = map[string]bool{
		"enable_tool": true, "install_worker_tool": true, "adjust_template": true,
		"improve_agent": true, "add_agent": true, "improve_uzi": true,
		"cost_efficiency": true,
	}
	RecommendationConfidences = map[string]bool{"": true, "low": true, "medium": true, "high": true}
)

// Review free-text length caps (PRD #46 Decision 5, audit C1/L4). Generous for a
// verdict/recommendation, tight enough to bound an attacker-suppliable worker POST.
const (
	ReviewSummaryMaxBytes    = 8 * 1024
	ReviewTargetMaxBytes     = 255
	ReviewRationaleMaxBytes  = 4 * 1024
	ReviewModelMaxBytes      = 100
	ReviewMaxRecommendations = 50
)

// ReviewSubmission is a judge's verdict + recommendations, already VALIDATED and
// SCRUBBED by the handler (Decision 5 — enum-checked, length-capped, control-stripped,
// secret-scrubbed). The service persists it; the DB CHECK on verdict/category/
// confidence is the backstop.
type ReviewSubmission struct {
	Verdict         string
	SummaryMd       string
	JudgeModel      string
	Status          string
	Recommendations []ReviewRecommendation
}

// ReviewRecommendation is one structured recommendation. The json tags match the
// jsonb_to_recordset columns the atomic upsert query destructures.
type ReviewRecommendation struct {
	Category    string `json:"category"`
	Target      string `json:"target"`
	RationaleMd string `json:"rationale_md"`
	Confidence  string `json:"confidence"`
}

// ReviewResult identifies the persisted review after a PostReview — the reviewed
// run's owner and the review row id. The handler uses it to fire the "review ready"
// notification AFTER the review is durably persisted (M4, persist-first): the deep
// link is built from the target run id, the event-log row is anchored to both ids, and
// the notification is delivered only to the run's own owner (never cross-user).
type ReviewResult struct {
	OwnerID  uuid.UUID
	ReviewID uuid.UUID
}

// AdviceClaim is the claim a judge or task-review flight stamps on its advice POST (issue
// #1423). Both fields are nil (not stamped) for a legacy worker; an advice_claim_fence_v1
// worker must stamp BOTH, and omitting either is ErrMissingClaimGeneration. The requirement is
// keyed on advice_claim_fence_v1, not credential_switch_v1: the latter shipped before advice
// posts were stamped, so a credential_switch_v1-only worker omits both and takes the legacy path.
//   - Generation is the advice run's claim_generation the flight holds; the upsert is fenced
//     on it in the same statement as the write.
//   - RunID is the advice run the flight holds. Generations are PER-RUN counters, so a stale
//     flight of an earlier judge run A (gen 1) and a re-judge B of the same target claimed by
//     the same worker (also gen 1) would otherwise be indistinguishable: authorization
//     resolves (worker, target) to the ACTIVE run B, and A's generation would match B's.
//     Stamping the run id makes that post ErrStaleClaim instead of landing under B, which is
//     why an advice_claim_fence_v1 worker may not omit it.
type AdviceClaim struct {
	Generation *int64
	RunID      *uuid.UUID
}

// checkAdviceClaim applies the pre-write half of the advice-post fence, AFTER authorization
// (ErrRunNotFound wins over everything here): an advice_claim_fence_v1 worker that omits the
// generation or the advice run id is ErrMissingClaimGeneration, then a stamped advice run id
// that is not the authorized advice run is ErrStaleClaim. A worker without that capability
// (including a credential_switch_v1 worker from before advice posts were stamped) may omit
// both, so an api upgraded ahead of its workers keeps accepting their advice posts. The
// generation / unreleased-claim / owning-worker / non-terminal-status half runs inside the
// upsert itself, whether or not the fields were stamped.
func checkAdviceClaim(wkr store.Worker, adviceRunID uuid.UUID, claim AdviceClaim) error {
	if (claim.Generation == nil || claim.RunID == nil) && slices.Contains(wkr.ProtocolCapabilities, capability.AdviceClaimFenceV1) {
		return ErrMissingClaimGeneration
	}
	if claim.RunID != nil && *claim.RunID != adviceRunID {
		return ErrStaleClaim
	}
	return nil
}

// PostReview persists a judge's review of a target run (PRD #46 Decision 5) — the
// worker's write-back at judge-run completion. Authorization is judge-run-scoped
// (authorizeJudgeTrace): the caller's worker must own the active judge run reviewing
// targetID, and target/judge owner equality is re-asserted. The verdict and its
// recommendations are written in ONE atomic statement with UPSERT (replace) semantics,
// so a re-judge overwrites the prior review rather than 23505-ing (Decision 8).
// Provenance — the producing judge run + owner — is stamped on every recommendation.
// Returns the owner + review id so the caller can notify persist-first (the review is
// the durable source of truth; the notification is a best-effort surface layered on).
//
// Issue #1423: the write is fenced on the JUDGE run's claim, mirroring the message fence
// (see AdviceClaim and checkAdviceClaim; only an advice_claim_fence_v1 worker is required to
// stamp the claim). The upsert persists nothing when the judge run is
// no longer claimed by wkr, has reached a terminal status, its claim is released, or it is at
// a different generation, and
// that no-row outcome surfaces as ErrStaleClaim (the auto-dismiss net is skipped: there is
// no fresh review).
func (s *Service) PostReview(ctx context.Context, wkr store.Worker, targetID uuid.UUID, sub ReviewSubmission, claim AdviceClaim) (ReviewResult, error) {
	judge, target, err := s.authorizeJudgeTrace(ctx, wkr, targetID)
	if err != nil {
		return ReviewResult{}, err
	}
	if err := checkAdviceClaim(wkr, judge.ID, claim); err != nil {
		return ReviewResult{}, err
	}
	recs := sub.Recommendations
	if recs == nil {
		recs = []ReviewRecommendation{}
	}
	recsJSON, err := json.Marshal(recs)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("marshal recommendations: %w", err)
	}
	reviewID, err := s.q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID:      target.ID,
		JudgeRunID:       pgconv.UUID(judge.ID),
		UserID:           target.UserID,
		Verdict:          sub.Verdict,
		SummaryMd:        sub.SummaryMd,
		JudgeModel:       sub.JudgeModel,
		Status:           sub.Status,
		ProducedByRunID:  pgconv.UUID(judge.ID),
		ProducedByUserID: pgconv.UUID(target.UserID),
		Recommendations:  recsJSON,
		ClaimGeneration:  pgconv.Int8Ptr(claim.Generation),
		WorkerID:         wkr.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewResult{}, ErrStaleClaim
		}
		return ReviewResult{}, err
	}
	s.autoDismissDeniedCLIRecommendations(ctx, reviewID, recs)
	return ReviewResult{OwnerID: target.UserID, ReviewID: reviewID}, nil
}

// autoDismissDeniedCLIRecommendations is the deterministic net behind the prompt-side fix
// (issue #167, backstop for MR !136). At the PostReview hook — AFTER the review + its
// recommendations are durably persisted — it auto-dismisses any recommendation whose
// target names a denylisted, credential-bearing CLI (glab/gh/aws/az/…), stamping the
// distinct, self-measuring provenance set_via='denied_cli' (dismiss_reason='wont_do',
// set_by_user_id NULL).
//
// CATEGORY SCOPE. Only 'enable_tool' and 'install_worker_tool' are in scope: those are the
// categories whose target IS a tool the rec proposes to add, which is precisely what can
// never be actioned for a barred CLI. Other categories (e.g. improve_uzi "improve aws
// integration") mention a denied name incidentally and must NOT be dismissed.
//
// BEST-EFFORT. A dispose failure logs and continues; it must NEVER fail the worker's review
// submission. The review is the durable source of truth; this net is a layered surface, and
// PostReview returns the same ReviewResult whether or not any dismissal landed.
//
// NON-CLOBBERING + THE ACCEPTED RESIDUAL. The store query is ON CONFLICT DO NOTHING, so a
// surviving human verdict on the coordinate is never overwritten. An Undo deletes the
// disposition row, so a later re-judge re-dismisses the coordinate — that re-dismissal is
// accepted: it is visible (set_via='denied_cli') and reversible (Undo again).
func (s *Service) autoDismissDeniedCLIRecommendations(ctx context.Context, reviewID uuid.UUID, recs []ReviewRecommendation) {
	for _, rec := range recs {
		if rec.Category != "enable_tool" && rec.Category != "install_worker_tool" {
			continue
		}
		if !recommendsDeniedExecutable(rec.Target) {
			continue
		}
		if _, err := s.q.SystemDismissDeniedCLIRecommendation(ctx, store.SystemDismissDeniedCLIRecommendationParams{
			ReviewID:      reviewID,
			Category:      rec.Category,
			Target:        rec.Target,
			RationaleHash: RationaleHash(rec.RationaleMd),
		}); err != nil {
			slog.Error("judge PostReview: auto-dismiss denied-CLI recommendation",
				"review", reviewID.String(), "category", rec.Category, "target", rec.Target, "error", err)
		}
	}
}
