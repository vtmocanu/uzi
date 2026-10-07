package apitypes

// PlanCrossCheckFindingsDTO contains bounded, scrubbed checker prose. Renderers
// still treat it as untrusted Markdown or terminal text.
type PlanCrossCheckFindingsDTO struct {
	Summary string                     `json:"summary"`
	Items   []PlanCrossCheckFindingDTO `json:"items"`
}

type PlanCrossCheckFindingDTO struct {
	File      string `json:"file"`
	Severity  string `json:"severity"`
	Summary   string `json:"summary"`
	Rationale string `json:"rationale"`
}

// PlanCrossCheckSummaryDTO is owner-only detail metadata for the single plan check.
// Recorded model/effort survive child deletion; a missing child or usage row has
// no usage total. Historical findings do not certify the current plan.
type PlanCrossCheckSummaryDTO struct {
	Round         int32                      `json:"round"`
	Verdict       string                     `json:"verdict"`
	ReasonClass   *string                    `json:"reason_class"`
	Findings      *PlanCrossCheckFindingsDTO `json:"findings"`
	CheckerRunID  *string                    `json:"checker_run_id"`
	CheckerModel  *string                    `json:"checker_model"`
	CheckerEffort *string                    `json:"checker_effort"`
	Usage         *UsageDTO                  `json:"usage"`
	Historical    bool                       `json:"historical"`
}
