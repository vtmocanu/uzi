package apitypes

import "time"

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
	CheckerModelSource  *string                    `json:"checker_model_source"`
	CheckerEffortSource *string                    `json:"checker_effort_source"`
	Round               int32                      `json:"round"`
	Verdict             string                     `json:"verdict"`
	ReasonClass         *string                    `json:"reason_class"`
	Findings            *PlanCrossCheckFindingsDTO `json:"findings"`
	CheckerRunID        *string                    `json:"checker_run_id"`
	CheckerModel        *string                    `json:"checker_model"`
	CheckerEffort       *string                    `json:"checker_effort"`
	Usage               *UsageDTO                  `json:"usage"`
	Historical          bool                       `json:"historical"`
	// Rounds (PRD #3708) is the newest five rounds in chronological order, each with the
	// lead's bounded response to its findings. Older servers omit it.
	Rounds []PlanCrossCheckRoundDTO `json:"rounds,omitempty"`
	// DeadlineAt is the checker deadline of the latest round, set only while it is pending.
	DeadlineAt *time.Time `json:"deadline_at,omitempty"`
}

// Plan cross-check round response states (PRD #3708). The response is what the lead wrote
// about a round's findings, taken from the FOLLOWING candidate. No resolution is ever inferred.
const (
	PlanCrossCheckResponseReported                  = "reported"
	PlanCrossCheckResponseTruncated                 = "truncated"
	PlanCrossCheckResponseNotReported               = "not_reported"
	PlanCrossCheckResponseUnknown                   = "unknown"
	PlanCrossCheckResponseReplannedAfterInterrupted = "replanned_after_interruption"
	PlanCrossCheckResponseReportedInPresentedPlan   = "reported_in_presented_plan"
	PlanCrossCheckResponseNoFollowingCandidate      = "no_following_candidate"
)

// PlanCrossCheckRoundResponseDTO is untrusted lead prose (claims, not verified resolutions).
// Text is empty unless State is reported, truncated or reported_in_presented_plan.
type PlanCrossCheckRoundResponseDTO struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// PlanCrossCheckRoundDTO is one checked round of the gated plan loop. It carries no
// candidate plan text: the plan never reaches a client through it.
type PlanCrossCheckRoundDTO struct {
	Round       int32                          `json:"round"`
	Verdict     string                         `json:"verdict"`
	ReasonClass *string                        `json:"reason_class"`
	Findings    *PlanCrossCheckFindingsDTO     `json:"findings"`
	DecidedAt   *time.Time                     `json:"decided_at"`
	Response    PlanCrossCheckRoundResponseDTO `json:"response"`
}
