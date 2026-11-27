package apitypes

import "time"

// Issue #4729: wire DTOs of the scoped run Flow analytics API
// (GET /api/analytics/flow and GET /api/admin/analytics/flow).
//
// Conventions shared by every type below, on top of those in analytics.go:
//   - Every block names its time_basis (now, created_at or finished_at) and a population
//     label. Blocks are independent populations and must not be summed against each other.
//   - "index" in a bucket cell is the zero-based position in the response's buckets array;
//     every bucket is present, including empty ones.
//   - A percentile is a {value, reason} pair: null always travels with a reason
//     (insufficient_observations, no_observations).
//   - Durations are seconds as JSON numbers.
//   - Every list is a JSON array, never null.
//   - The response names no repo id or path. The only repo that appears is the caller's own
//     validated repo selector, echoed in filters.

// AnalyticsFlowFiltersDTO echoes the caller's authorized selectors. Repo and Harness are null
// when not given; Kinds is the effective kind list.
type AnalyticsFlowFiltersDTO struct {
	Repo    *string  `json:"repo"`
	Harness *string  `json:"harness"`
	Kinds   []string `json:"kinds"`
}

// AnalyticsFlowDTO is the Flow response: the calendar envelope plus every Flow block.
type AnalyticsFlowDTO struct {
	AnalyticsEnvelopeDTO
	// SelectedUser matches the options response: "all" for an admin response spanning every
	// user, else the user id.
	SelectedUser    string                       `json:"selected_user"`
	Filters         AnalyticsFlowFiltersDTO      `json:"filters"`
	WaitingNow      AnalyticsFlowWaitingDTO      `json:"waiting_now"`
	Activity        AnalyticsFlowActivityDTO     `json:"activity"`
	FirstStartDelay AnalyticsFlowFirstStartDTO   `json:"first_start_delay"`
	PlanResponse    AnalyticsFlowPlanResponseDTO `json:"plan_response"`
	DeliveryFunnel  AnalyticsFlowFunnelDTO       `json:"delivery_funnel"`
	Approval        AnalyticsFlowApprovalDTO     `json:"approval"`
	IssueElapsed    AnalyticsFlowElapsedDTO      `json:"issue_elapsed"`
}

// AnalyticsFlowWaitingDTO counts the selected-scope runs currently in a waiting status. States
// always lists all eight states in a fixed order, zero rows included; the window does not apply.
type AnalyticsFlowWaitingDTO struct {
	TimeBasis  string                      `json:"time_basis"`
	Population string                      `json:"population"`
	States     []AnalyticsFlowWaitStateDTO `json:"states"`
	Total      int64                       `json:"total"`
}

// AnalyticsFlowWaitStateDTO is one waiting status. The oldest fields are null when count is 0.
type AnalyticsFlowWaitStateDTO struct {
	State             string     `json:"state"`
	Count             int64      `json:"count"`
	OldestStatusSince *time.Time `json:"oldest_status_since"`
	OldestAgeSeconds  *float64   `json:"oldest_age_seconds"`
}

// AnalyticsFlowActivityDTO holds the created and finished series, each with its own time basis.
type AnalyticsFlowActivityDTO struct {
	Created  AnalyticsFlowCreatedDTO  `json:"created"`
	Finished AnalyticsFlowFinishedDTO `json:"finished"`
}

// AnalyticsFlowCreatedDTO counts runs created in the window.
type AnalyticsFlowCreatedDTO struct {
	TimeBasis  string                        `json:"time_basis"`
	Population string                        `json:"population"`
	Total      int64                         `json:"total"`
	Buckets    []AnalyticsFlowCreatedCellDTO `json:"buckets"`
}

// AnalyticsFlowCreatedCellDTO is one bucket's created count.
type AnalyticsFlowCreatedCellDTO struct {
	Index int   `json:"index"`
	Count int64 `json:"count"`
}

// AnalyticsFlowFinishedDTO counts terminal runs by end time and outcome class. Chat has no
// outcome class and is never counted; excluded_kinds says so.
type AnalyticsFlowFinishedDTO struct {
	TimeBasis     string                         `json:"time_basis"`
	Population    string                         `json:"population"`
	ExcludedKinds []string                       `json:"excluded_kinds"`
	Total         AnalyticsFlowOutcomeCountsDTO  `json:"total"`
	Buckets       []AnalyticsFlowFinishedCellDTO `json:"buckets"`
}

// AnalyticsFlowOutcomeCountsDTO are finished-run outcome classes and their sum.
type AnalyticsFlowOutcomeCountsDTO struct {
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	Failed       int64 `json:"failed"`
	Total        int64 `json:"total"`
}

// AnalyticsFlowFinishedCellDTO is one bucket's finished counts, bucketed by end time.
type AnalyticsFlowFinishedCellDTO struct {
	Index int `json:"index"`
	AnalyticsFlowOutcomeCountsDTO
}

// AnalyticsFlowFirstStartDTO is the approximate first-start delay over the creation cohort.
// Approximate is always true; limitations says why.
type AnalyticsFlowFirstStartDTO struct {
	TimeBasis   string                     `json:"time_basis"`
	Population  string                     `json:"population"`
	Approximate bool                       `json:"approximate"`
	Limitations []string                   `json:"limitations"`
	Observed    int64                      `json:"observed"`
	NotStarted  int64                      `json:"not_started"`
	Invalid     int64                      `json:"invalid"`
	P50Seconds  AnalyticsNullableNumberDTO `json:"p50_seconds"`
	P90Seconds  AnalyticsNullableNumberDTO `json:"p90_seconds"`
}

// AnalyticsFlowPlanResponseDTO is the approximate human plan-response time. n counts timed
// pairs; not_approved counts presentations with no applied approval and is neither timed nor
// unknown.
type AnalyticsFlowPlanResponseDTO struct {
	TimeBasis   string                     `json:"time_basis"`
	Population  string                     `json:"population"`
	Approximate bool                       `json:"approximate"`
	N           int64                      `json:"n"`
	P50Seconds  AnalyticsNullableNumberDTO `json:"p50_seconds"`
	P90Seconds  AnalyticsNullableNumberDTO `json:"p90_seconds"`
	Unknown     AnalyticsFlowUnknownDTO    `json:"unknown"`
	NotApproved int64                      `json:"not_approved"`
}

// AnalyticsFlowUnknownDTO is the unknown total and its split.
type AnalyticsFlowUnknownDTO struct {
	Total   int64                           `json:"total"`
	Reasons AnalyticsFlowResponseReasonsDTO `json:"reasons"`
}

// AnalyticsFlowResponseReasonsDTO are the plan-response unknown reasons.
type AnalyticsFlowResponseReasonsDTO struct {
	Ambiguous           int64 `json:"ambiguous"`
	NoPresentation      int64 `json:"no_presentation"`
	InvalidChronology   int64 `json:"invalid_chronology"`
	ExcludedDisposition int64 `json:"excluded_disposition"`
	ReconciliationOnly  int64 `json:"reconciliation_only"`
	LegacyUnbound       int64 `json:"legacy_unbound"`
}

// AnalyticsFlowFunnelDTO is the delivery funnel over the issue / self_improve creation
// cohort. applicable is false (reason not_applicable, every count zero) when the kind selector
// excludes both.
type AnalyticsFlowFunnelDTO struct {
	TimeBasis  string                           `json:"time_basis"`
	Population string                           `json:"population"`
	Applicable bool                             `json:"applicable"`
	Reason     *string                          `json:"reason"`
	Stages     AnalyticsFlowStagesDTO           `json:"stages"`
	Planning   AnalyticsFlowPlanningDTO         `json:"planning"`
	Outcomes   AnalyticsFlowOutcomesDTO         `json:"outcomes"`
	Disclosure AnalyticsFlowFunnelDisclosureDTO `json:"disclosure"`
	PRUnits    AnalyticsFlowPRUnitsDTO          `json:"pr_units"`
}

// AnalyticsFlowStagesDTO are the funnel's run-level stage counts.
type AnalyticsFlowStagesDTO struct {
	Created     int64 `json:"created"`
	PlanSettled int64 `json:"plan_settled"`
	Completed   int64 `json:"completed"`
	PRPresent   int64 `json:"pr_present"`
	Merged      int64 `json:"merged"`
}

// AnalyticsFlowPlanningDTO are the exclusive planning branches; they sum to stages.created.
type AnalyticsFlowPlanningDTO struct {
	SettledHuman int64 `json:"settled_human"`
	SettledAuto  int64 `json:"settled_auto"`
	Skipped      int64 `json:"skipped"`
	Rejected     int64 `json:"rejected"`
	InProgress   int64 `json:"in_progress"`
	Failed       int64 `json:"failed"`
	Cancelled    int64 `json:"cancelled"`
	Unknown      int64 `json:"unknown"`
}

// AnalyticsFlowOutcomesDTO are the exclusive outcome branches; they sum to stages.created.
type AnalyticsFlowOutcomesDTO struct {
	Completed    int64 `json:"completed"`
	Failed       int64 `json:"failed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	InProgress   int64 `json:"in_progress"`
}

// AnalyticsFlowFunnelDisclosureDTO counts runs whose approval evidence is incomplete.
type AnalyticsFlowFunnelDisclosureDTO struct {
	HistoricalApprovalOnly int64 `json:"historical_approval_only"`
	AmbiguousCurrent       int64 `json:"ambiguous_current"`
	LegacyEvidence         int64 `json:"legacy_evidence"`
}

// AnalyticsFlowPRUnitsDTO are the distinct PR units, bucketed by the creation bucket of the
// earliest creation-cohort run that references them.
type AnalyticsFlowPRUnitsDTO struct {
	Present int64                        `json:"present"`
	Merged  int64                        `json:"merged"`
	Buckets []AnalyticsFlowPRUnitCellDTO `json:"buckets"`
}

// AnalyticsFlowPRUnitCellDTO is one creation bucket's PR units.
type AnalyticsFlowPRUnitCellDTO struct {
	Index   int   `json:"index"`
	Present int64 `json:"present"`
	Merged  int64 `json:"merged"`
}

// AnalyticsFlowApprovalDTO classifies how the plan cohort's plans were approved. total =
// human + auto_approve_configured + unknown.total.
type AnalyticsFlowApprovalDTO struct {
	TimeBasis             string                          `json:"time_basis"`
	Population            string                          `json:"population"`
	Applicable            bool                            `json:"applicable"`
	Reason                *string                         `json:"reason"`
	Total                 int64                           `json:"total"`
	Human                 int64                           `json:"human"`
	AutoApproveConfigured int64                           `json:"auto_approve_configured"`
	Unknown               AnalyticsFlowApprovalUnknownDTO `json:"unknown"`
}

// AnalyticsFlowApprovalUnknownDTO is the unknown approvals and their reasons.
type AnalyticsFlowApprovalUnknownDTO struct {
	Total   int64                           `json:"total"`
	Reasons AnalyticsFlowApprovalReasonsDTO `json:"reasons"`
}

// AnalyticsFlowApprovalReasonsDTO split the unknown approvals.
type AnalyticsFlowApprovalReasonsDTO struct {
	Ambiguous              int64 `json:"ambiguous"`
	LegacyUnbound          int64 `json:"legacy_unbound"`
	HistoricalApprovalOnly int64 `json:"historical_approval_only"`
	NoEvidence             int64 `json:"no_evidence"`
}

// AnalyticsFlowElapsedDTO is the elapsed-time distribution of terminal issue runs over the
// creation cohort. applicable is false (reason not_applicable, all counts zero) when the kind
// selector excludes issue. Histogram bin i is [lower, upper); the last bin has a null upper.
type AnalyticsFlowElapsedDTO struct {
	TimeBasis  string                         `json:"time_basis"`
	Population string                         `json:"population"`
	Applicable bool                           `json:"applicable"`
	Reason     *string                        `json:"reason"`
	N          int64                          `json:"n"`
	Unfinished int64                          `json:"unfinished"`
	Invalid    int64                          `json:"invalid"`
	Incomplete bool                           `json:"incomplete"`
	P50Seconds AnalyticsNullableNumberDTO     `json:"p50_seconds"`
	P90Seconds AnalyticsNullableNumberDTO     `json:"p90_seconds"`
	Histogram  []AnalyticsFlowHistogramBinDTO `json:"histogram"`
	Buckets    []AnalyticsFlowElapsedCellDTO  `json:"buckets"`
}

// AnalyticsFlowHistogramBinDTO is one histogram bin; upper_seconds is null for the last bin.
type AnalyticsFlowHistogramBinDTO struct {
	LowerSeconds float64  `json:"lower_seconds"`
	UpperSeconds *float64 `json:"upper_seconds"`
	Count        int64    `json:"count"`
}

// AnalyticsFlowElapsedCellDTO is one creation bucket. n is kept when the percentiles are
// suppressed. Partial is the calendar's marker for the bucket that ends at generated_at: its
// percentiles cover a bucket that is still open.
type AnalyticsFlowElapsedCellDTO struct {
	Index      int                        `json:"index"`
	Partial    bool                       `json:"partial"`
	N          int64                      `json:"n"`
	Unfinished int64                      `json:"unfinished"`
	Invalid    int64                      `json:"invalid"`
	Incomplete bool                       `json:"incomplete"`
	P50Seconds AnalyticsNullableNumberDTO `json:"p50_seconds"`
	P90Seconds AnalyticsNullableNumberDTO `json:"p90_seconds"`
}
