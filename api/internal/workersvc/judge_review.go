package workersvc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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

// PostReview persists a judge's review of a target run (PRD #46 Decision 5) — the
// worker's write-back at judge-run completion. Authorization is judge-run-scoped
// (authorizeJudgeTrace): the caller's worker must own the active judge run reviewing
// targetID, and target/judge owner equality is re-asserted. The verdict and its
// recommendations are written in ONE atomic statement with UPSERT (replace) semantics,
// so a re-judge overwrites the prior review rather than 23505-ing (Decision 8).
// Provenance — the producing judge run + owner — is stamped on every recommendation.
func (s *Service) PostReview(ctx context.Context, wkr store.Worker, targetID uuid.UUID, sub ReviewSubmission) error {
	judge, target, err := s.authorizeJudgeTrace(ctx, wkr, targetID)
	if err != nil {
		return err
	}
	recs := sub.Recommendations
	if recs == nil {
		recs = []ReviewRecommendation{}
	}
	recsJSON, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("marshal recommendations: %w", err)
	}
	_, err = s.q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID:      target.ID,
		JudgeRunID:       pgUUID(judge.ID),
		UserID:           target.UserID,
		Verdict:          sub.Verdict,
		SummaryMd:        sub.SummaryMd,
		JudgeModel:       sub.JudgeModel,
		Status:           sub.Status,
		ProducedByRunID:  pgUUID(judge.ID),
		ProducedByUserID: pgUUID(target.UserID),
		Recommendations:  recsJSON,
	})
	return err
}
