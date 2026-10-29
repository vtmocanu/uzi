package apitypes

import "time"

// LeadConfigReportRequest is the worker's body for POST /api/worker/runs/{id}/lead-config (PRD #3244
// section 4): one full-snapshot row of what a lead agent was configured to use (requested side, each
// value with a source) and what the runtime reported (observed side, each value with evidence; the
// four observed fields are null until observed). The schema is closed: the handler decodes it with
// DisallowUnknownFields. ClaimGeneration is a pointer so a missing key is distinguishable from a
// legitimate zero. Identity (user, repo, harness) is never accepted from the body: the server copies
// harness from the claimed run. Model strings are UNTRUSTED worker-attested text, validated by
// leadcfg.ValidateReport before storage.
type LeadConfigReportRequest struct {
	ClaimGeneration        *int64  `json:"claim_generation"`
	Phase                  string  `json:"phase"`
	PhaseEntry             int     `json:"phase_entry"`
	Seq                    int     `json:"seq"`
	Truncated              bool    `json:"truncated"`
	RequestedModel         *string `json:"requested_model"`
	RequestedModelSource   string  `json:"requested_model_source"`
	RequestedEffort        *string `json:"requested_effort"`
	RequestedEffortMode    string  `json:"requested_effort_mode"`
	RequestedEffortSource  string  `json:"requested_effort_source"`
	ObservedModel          *string `json:"observed_model"`
	ObservedModelEvidence  *string `json:"observed_model_evidence"`
	ObservedEffort         *string `json:"observed_effort"`
	ObservedEffortEvidence *string `json:"observed_effort_evidence"`
}

// LeadPhaseConfigDTO is one recorded lead-config row as the run read shows it (PRD #3244 section 5).
// RequestedModelFixedAt / RequestedEffortFixedAt are derived from the source (leadcfg.FixedAt): "create"
// when the value was fixed when the run was created, "claim" when it resolves at claim. Model strings
// are UNTRUSTED worker-attested text: a renderer folds terminal-unsafe characters.
type LeadPhaseConfigDTO struct {
	Phase                  string    `json:"phase"`
	PhaseEntry             int       `json:"phase_entry"`
	Seq                    int       `json:"seq"`
	Harness                string    `json:"harness"`
	RequestedModel         *string   `json:"requested_model"`
	RequestedModelSource   string    `json:"requested_model_source"`
	RequestedModelFixedAt  string    `json:"requested_model_fixed_at"`
	RequestedEffort        *string   `json:"requested_effort"`
	RequestedEffortMode    string    `json:"requested_effort_mode"`
	RequestedEffortSource  string    `json:"requested_effort_source"`
	RequestedEffortFixedAt string    `json:"requested_effort_fixed_at"`
	ObservedModel          *string   `json:"observed_model"`
	ObservedModelEvidence  *string   `json:"observed_model_evidence"`
	ObservedEffort         *string   `json:"observed_effort"`
	ObservedEffortEvidence *string   `json:"observed_effort_evidence"`
	Truncated              bool      `json:"truncated"`
	ReportedAt             time.Time `json:"reported_at"`
}

// RunLeadConfigDTO is RunDTO.LeadConfig: present iff the run is a lead-lane run. Applicable is always
// true when present. ClaimGeneration is the highest generation that has rows (0 when none), Current
// says it equals the run's own claim_generation, and Phases holds the highest-seq row per phase of
// that generation, plan first.
type RunLeadConfigDTO struct {
	Applicable      bool                 `json:"applicable"`
	ClaimGeneration int64                `json:"claim_generation"`
	Current         bool                 `json:"current"`
	Phases          []LeadPhaseConfigDTO `json:"phases"`
}
