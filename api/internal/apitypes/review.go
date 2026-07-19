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

// IssueDraftDTO is the templated, human-editable draft the "file an issue from a
// recommendation" flow serves (PRD #68 M2, Decision 2). The body is deterministically
// rendered from rows uzi already holds (no LLM call); every untrusted field is fenced +
// stripped + secret-scanned server-side, but this draft is NOT the security boundary —
// M3 re-applies the write-boundary controls to the client's POST body. default_repo_id
// is a pre-selection ("" when no default resolves, mock state D); labels are assembled
// server-side (PRD + PRDLESS, never autopilot); provenance names whose worker produced
// the (attacker-influencable) text so an admin filing another user's review sees it.
type IssueDraftDTO struct {
	DefaultRepoID string   `json:"default_repo_id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Labels        []string `json:"labels"`
	Provenance    string   `json:"provenance"`
	// DefaultNote explains the default repo (mock B hint) or its absence (mock D info
	// alert) — the picker copy the category→repo resolution produced.
	DefaultNote string `json:"default_note"`
}

// FiledIssueDTO is a SETTLED recommendation→forge-issue link for the run-page panel
// (PRD #68 M4): the coordinate it covers, the created issue, and when it was filed so the
// panel can render a filed row (with an issue link) instead of the File-issue button and
// flag a stale link (filed_at < review.updated_at → "filed for an earlier version"). Only
// settled links appear; an in-flight claim is transient and omitted.
type FiledIssueDTO struct {
	Category string    `json:"category"`
	Target   string    `json:"target"`
	IssueIID int64     `json:"issue_iid"`
	IssueURL string    `json:"issue_url"`
	FiledAt  time.Time `json:"filed_at"`
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
	// FiledIssues are the settled recommendation→issue links for this review (PRD #68).
	FiledIssues []FiledIssueDTO `json:"filed_issues"`
}
