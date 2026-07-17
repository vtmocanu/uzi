package apitypes

import "time"

// RecommendationDTO is one structured judge recommendation for the run-page panel
// (PRD #46 M4). category is the taxonomy enum; target/rationale are the scrubbed
// free-text fields (validated + capped + secret-scrubbed at the review POST), which
// the SPA renders as escaped text (never markdown/HTML).
type RecommendationDTO struct {
	ID          string    `json:"id"`
	Category    string    `json:"category"`
	Target      string    `json:"target"`
	RationaleMd string    `json:"rationale_md"`
	Confidence  string    `json:"confidence"`
	CreatedAt   time.Time `json:"created_at"`
}

// ReviewDTO is the run's judge verdict + recommendations for the run page. summary_md
// and each rationale_md were scrubbed at ingest; the SPA renders them as escaped text.
type ReviewDTO struct {
	ID              string              `json:"id"`
	TargetRunID     string              `json:"target_run_id"`
	Verdict         string              `json:"verdict"`
	SummaryMd       string              `json:"summary_md"`
	JudgeModel      string              `json:"judge_model"`
	Status          string              `json:"status"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Recommendations []RecommendationDTO `json:"recommendations"`
}
