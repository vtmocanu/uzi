package apitypes

import "time"

// GuardrailImpactRepoDTO is one enabled repo's line in the pre-flight guardrail
// impact scan (PRD #66 M3). Its wire shape mirrors privcheck.ImpactRepo, which the
// admin endpoint emits directly. Blocked and Unevaluable are mutually exclusive:
// an unevaluable repo is "unknown" (a forge read errored, or it has no default
// branch), counted apart from blocked and never read as safe (R1).
type GuardrailImpactRepoDTO struct {
	RepoID       string `json:"repo_id"`
	Path         string `json:"path"`
	UserID       string `json:"user_id"`
	ConnectionID string `json:"connection_id"`
	Blocked      bool   `json:"blocked"`
	Unevaluable  bool   `json:"unevaluable"`
}

// GuardrailImpactDTO is the CLI/SPA view of the live, non-persisting guardrail
// impact scan (PRD #66 M3): how many enabled repos would be refused under the new
// guardrail. BlockedCount and UnevaluableCount are separate so an empty result is
// never read as "zero affected" when it is really "could not tell" (R1). Its wire
// shape mirrors privcheck.ImpactReport.
type GuardrailImpactDTO struct {
	CheckedAt        time.Time                `json:"checked_at"`
	EnabledRepoCount int                      `json:"enabled_repo_count"`
	BlockedCount     int                      `json:"blocked_count"`
	UnevaluableCount int                      `json:"unevaluable_count"`
	Repos            []GuardrailImpactRepoDTO `json:"repos"`
}
