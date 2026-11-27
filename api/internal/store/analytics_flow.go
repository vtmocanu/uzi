package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Issue #4729: typed mirror of the jsonb document that AnalyticsOwnerFlow and
// AnalyticsAllFlow return. The SQL is the authority for the shape; DecodeAnalyticsFlowDoc
// rejects unknown keys, so a key the statement adds or renames without a mirrored field
// fails loudly. A key the statement drops is not detected: the missing field decodes as its
// zero value.
//
// Conventions shared by every block below:
//   - Every block names its time_basis (now, created_at or finished_at) and a population
//     label. Blocks are independent populations and must not be summed against each other.
//   - "index" in a bucket cell is the zero-based position in the bucket_starts / bucket_ends
//     arrays the caller passed; every bucket is present, including empty ones.
//   - A percentile is an AnalyticsNullableNumber: null always travels with a reason.
//   - Durations are seconds as JSON numbers. Timestamps are UTC RFC 3339 strings with
//     microseconds, independent of the session TimeZone.
//   - The document names no repo id or path, so owner repo masking has nothing to hide. It
//     does not echo the request filters either; the handler discloses those.
//   - The calendar "partial" marker is not in this document: the handler takes it from the
//     Go bounds.

// FlowMinPercentileN is the fewest valid observations a creation bucket needs before its
// elapsed percentiles are shown. Callers pass it as @min_percentile_n.
const FlowMinPercentileN = 5

// FlowHistogramEdgesSeconds are the lower edges of the issue-run elapsed histogram bins in
// seconds. Bin i covers [edge i, edge i+1) and the last bin is open-ended. Callers pass it as
// @histogram_edges and must not modify it.
var FlowHistogramEdgesSeconds = []float64{0, 900, 1800, 3600, 7200, 14400, 28800, 86400, 259200}

// Reasons the Flow document adds to the shared AnalyticsReason* set.
const (
	// AnalyticsReasonInsufficientObservations: a bucket has observations, but fewer than
	// FlowMinPercentileN, so its percentiles are suppressed (its n is kept).
	AnalyticsReasonInsufficientObservations = "insufficient_observations"
	// AnalyticsReasonNoObservations: the population has no valid observation at all.
	AnalyticsReasonNoObservations = "no_observations"
)

// AnalyticsFlowDoc is the whole Flow document.
type AnalyticsFlowDoc struct {
	// GeneratedAt is the statement's @now, echoed in UTC.
	GeneratedAt string `json:"generated_at"`
	// WaitingNow is a snapshot of current runs; the window does not apply to it.
	WaitingNow AnalyticsFlowWaiting `json:"waiting_now"`
	// Activity holds the created and finished series, each with its own time basis.
	Activity AnalyticsFlowActivity `json:"activity"`
	// FirstStartDelay is first_started_at - created_at over the creation cohort.
	FirstStartDelay AnalyticsFlowFirstStart `json:"first_start_delay"`
	// PlanResponse is the human plan-response time over gate presentation / approval pairs.
	PlanResponse AnalyticsFlowPlanResponse `json:"plan_response"`
	// DeliveryFunnel is the issue / self_improve creation cohort's delivery funnel.
	DeliveryFunnel AnalyticsFlowFunnel `json:"delivery_funnel"`
	// Approval classifies how the plan cohort's plans were approved.
	Approval AnalyticsFlowApproval `json:"approval"`
	// IssueElapsed is the elapsed-time distribution of terminal issue runs.
	IssueElapsed AnalyticsFlowElapsed `json:"issue_elapsed"`
}

// AnalyticsFlowWaiting counts the selected-scope runs currently in a waiting status.
// States always lists all eight states in a fixed order (queued, awaiting_approval,
// awaiting_input, awaiting_followup, paused, limit_wait, pool_wait, recovery_wait), zero
// rows included. TimeBasis is "now".
type AnalyticsFlowWaiting struct {
	TimeBasis  string                   `json:"time_basis"`
	Population string                   `json:"population"`
	States     []AnalyticsFlowWaitState `json:"states"`
	Total      int64                    `json:"total"`
}

// AnalyticsFlowWaitState is one waiting status. OldestStatusSince and OldestAgeSeconds are
// null when Count is 0; the age is the statement's @now minus the oldest status_since,
// never negative.
type AnalyticsFlowWaitState struct {
	State             string   `json:"state"`
	Count             int64    `json:"count"`
	OldestStatusSince *string  `json:"oldest_status_since"`
	OldestAgeSeconds  *float64 `json:"oldest_age_seconds"`
}

// AnalyticsFlowActivity holds the two independent activity series.
type AnalyticsFlowActivity struct {
	Created  AnalyticsFlowCreated  `json:"created"`
	Finished AnalyticsFlowFinished `json:"finished"`
}

// AnalyticsFlowCreated counts runs of the selected kinds created in [from, now); chat is
// included when the kind selector includes it. TimeBasis is "created_at".
type AnalyticsFlowCreated struct {
	TimeBasis  string                     `json:"time_basis"`
	Population string                     `json:"population"`
	Total      int64                      `json:"total"`
	Buckets    []AnalyticsFlowCreatedCell `json:"buckets"`
}

// AnalyticsFlowCreatedCell is one bucket's created count.
type AnalyticsFlowCreatedCell struct {
	Index int   `json:"index"`
	Count int64 `json:"count"`
}

// AnalyticsFlowFinished counts currently terminal runs whose end time
// COALESCE(finished_at, status_since) is in [from, now), by outcome class, independent of
// created_at. Chat has no outcome class and is never counted (ExcludedKinds says so).
// TimeBasis is "finished_at".
type AnalyticsFlowFinished struct {
	TimeBasis     string                      `json:"time_basis"`
	Population    string                      `json:"population"`
	ExcludedKinds []string                    `json:"excluded_kinds"`
	Total         AnalyticsFlowOutcomeCounts  `json:"total"`
	Buckets       []AnalyticsFlowFinishedCell `json:"buckets"`
}

// AnalyticsFlowOutcomeCounts are finished-run outcome classes (the run_outcome_classes
// view) and their sum.
type AnalyticsFlowOutcomeCounts struct {
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	Failed       int64 `json:"failed"`
	Total        int64 `json:"total"`
}

// AnalyticsFlowFinishedCell is one bucket's finished counts, bucketed by end time.
type AnalyticsFlowFinishedCell struct {
	Index int `json:"index"`
	AnalyticsFlowOutcomeCounts
}

// AnalyticsFlowFirstStart is the approximate first-start delay over the creation cohort.
// Observed is first_started_at - created_at >= 0, NotStarted is a null first_started_at,
// Invalid is a negative difference. Percentiles are null with no_observations when Observed
// is 0. Approximate is always true and Limitations lists why: migration 00280 backfilled
// first_started_at from a potentially later-leg started_at ("first_started_at_legacy_backfill").
type AnalyticsFlowFirstStart struct {
	TimeBasis   string                  `json:"time_basis"`
	Population  string                  `json:"population"`
	Approximate bool                    `json:"approximate"`
	Limitations []string                `json:"limitations"`
	Observed    int64                   `json:"observed"`
	NotStarted  int64                   `json:"not_started"`
	Invalid     int64                   `json:"invalid"`
	P50Seconds  AnalyticsNullableNumber `json:"p50_seconds"`
	P90Seconds  AnalyticsNullableNumber `json:"p90_seconds"`
}

// AnalyticsFlowPlanResponse is the approximate human plan-response time: from a same-run,
// same-revision gate presentation's created_at to the applied human input's created_at. N is
// the number of timed pairs. Every other pair is either Unknown (with a reason) or
// NotApproved (a presentation with no bound applied approve_plan receipt, disclosed, neither
// timed nor unknown). A run with a NULL-binding (pre-00260) approve_plan receipt adds one
// unknown even when it also has pairs: that receipt belongs to an untimeable revision-0 gate.
// A run with only 'unbound' receipts adds one unknown only when it has no pair at all; with
// pairs, an unbound approve can never act on a gate, so its pair stays not_approved.
// Percentiles are null with no_observations when N is 0.
type AnalyticsFlowPlanResponse struct {
	TimeBasis   string                  `json:"time_basis"`
	Population  string                  `json:"population"`
	Approximate bool                    `json:"approximate"`
	N           int64                   `json:"n"`
	P50Seconds  AnalyticsNullableNumber `json:"p50_seconds"`
	P90Seconds  AnalyticsNullableNumber `json:"p90_seconds"`
	Unknown     AnalyticsFlowUnknown    `json:"unknown"`
	NotApproved int64                   `json:"not_approved"`
}

// AnalyticsFlowUnknown is the unknown total and its split. Reasons: ambiguous (more than
// one eligible receipt for the pair), no_presentation (an eligible receipt with no same-pair
// presentation row), invalid_chronology (a receipt applied before it was created, or a
// presentation created after the input), excluded_disposition (only superseded, declined or
// advisory receipts), reconciliation_only (the single eligible receipt has disposition
// 'applied'), legacy_unbound (a run with legacy gate evidence: any approve_plan receipt with a
// NULL or 'unbound' binding; such a run is counted once here, whatever its other pairs).
type AnalyticsFlowUnknown struct {
	Total   int64                        `json:"total"`
	Reasons AnalyticsFlowResponseReasons `json:"reasons"`
}

// AnalyticsFlowResponseReasons are the plan-response unknown reasons.
type AnalyticsFlowResponseReasons struct {
	Ambiguous           int64 `json:"ambiguous"`
	NoPresentation      int64 `json:"no_presentation"`
	InvalidChronology   int64 `json:"invalid_chronology"`
	ExcludedDisposition int64 `json:"excluded_disposition"`
	ReconciliationOnly  int64 `json:"reconciliation_only"`
	LegacyUnbound       int64 `json:"legacy_unbound"`
}

// AnalyticsFlowFunnel is the selected delivery funnel over the creation cohort restricted
// to kinds issue and self_improve. When the kind selector excludes both, Applicable is false,
// Reason is not_applicable and every count is zero.
type AnalyticsFlowFunnel struct {
	TimeBasis  string  `json:"time_basis"`
	Population string  `json:"population"`
	Applicable bool    `json:"applicable"`
	Reason     *string `json:"reason"`
	// Stages count runs: PlanSettled is Planning.SettledHuman + Planning.SettledAuto,
	// Completed is outcome completed, PRPresent has a PR and repo, Merged has a witnessed
	// merge.
	Stages AnalyticsFlowStages `json:"stages"`
	// Planning branches are exclusive and sum to Stages.Created. Precedence: rejected,
	// settled_human, settled_auto, skipped, in_progress, failed, cancelled, unknown.
	Planning AnalyticsFlowPlanning `json:"planning"`
	// Outcomes are exclusive and sum to Stages.Created; in_progress has no outcome yet.
	Outcomes AnalyticsFlowOutcomes `json:"outcomes"`
	// Disclosure counts runs with approval evidence caveats.
	Disclosure AnalyticsFlowFunnelDisclosure `json:"disclosure"`
	// PRUnits counts distinct (repo_id, mr_iid) units, each attributed to its earliest
	// creation-cohort run (created_at, then id) and bucketed by that run's creation bucket.
	PRUnits AnalyticsFlowPRUnits `json:"pr_units"`
}

// AnalyticsFlowStages are the funnel's run-level stage counts.
type AnalyticsFlowStages struct {
	Created     int64 `json:"created"`
	PlanSettled int64 `json:"plan_settled"`
	Completed   int64 `json:"completed"`
	PRPresent   int64 `json:"pr_present"`
	Merged      int64 `json:"merged"`
}

// AnalyticsFlowPlanning are the exclusive planning branches. Unknown means evidence cannot
// establish how planning ended (for example a completed run with no proof of approval and no
// seeded plan).
type AnalyticsFlowPlanning struct {
	SettledHuman int64 `json:"settled_human"`
	SettledAuto  int64 `json:"settled_auto"`
	Skipped      int64 `json:"skipped"`
	Rejected     int64 `json:"rejected"`
	InProgress   int64 `json:"in_progress"`
	Failed       int64 `json:"failed"`
	Cancelled    int64 `json:"cancelled"`
	Unknown      int64 `json:"unknown"`
}

// AnalyticsFlowOutcomes are the exclusive outcome branches of the funnel cohort.
type AnalyticsFlowOutcomes struct {
	Completed    int64 `json:"completed"`
	Failed       int64 `json:"failed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	InProgress   int64 `json:"in_progress"`
}

// AnalyticsFlowFunnelDisclosure counts runs whose approval evidence is incomplete:
// HistoricalApprovalOnly (no eligible receipt for the current revision and at least one older
// revision with exactly one eligible receipt; independent of the approval ranking),
// AmbiguousCurrent (more than one eligible receipt for the current revision; a run with no
// eligible current receipt whose only older evidence is ambiguous is counted in neither) and
// LegacyEvidence (a run with any NULL-binding or unbound approve_plan receipt, even when it
// also has human proof for the current plan; the approval classification still ranks that
// human proof first).
type AnalyticsFlowFunnelDisclosure struct {
	HistoricalApprovalOnly int64 `json:"historical_approval_only"`
	AmbiguousCurrent       int64 `json:"ambiguous_current"`
	LegacyEvidence         int64 `json:"legacy_evidence"`
}

// AnalyticsFlowPRUnits are the distinct PR units. Merged units have a witness: a merged run
// of the same user (owner and admin-selected-user) or any user (admin all-users) on the same
// (repo_id, mr_iid), found irrespective of the window, harness, kind and repo filters.
type AnalyticsFlowPRUnits struct {
	Present int64                     `json:"present"`
	Merged  int64                     `json:"merged"`
	Buckets []AnalyticsFlowPRUnitCell `json:"buckets"`
}

// AnalyticsFlowPRUnitCell is one creation bucket's PR units.
type AnalyticsFlowPRUnitCell struct {
	Index   int   `json:"index"`
	Present int64 `json:"present"`
	Merged  int64 `json:"merged"`
}

// AnalyticsFlowApproval classifies the plan cohort: funnel-cohort runs with gate_revision > 0,
// auto_approve set, or any approve_plan receipt. Human is exactly one eligible applied human
// receipt for the run's current gate revision; it takes precedence over configuration and
// over legacy evidence.
// AutoApproveConfigured has no human proof but auto_approve set (configuration, not an
// observed event). Unknown is the rest, split by reason in order ambiguous (more than one
// eligible current receipt, or older-revision ambiguity with no older revision holding exactly
// one eligible receipt), legacy_unbound, historical_approval_only (at least one older revision
// with exactly one eligible receipt), no_evidence. Human and auto-approve configuration rank
// ahead of any older ambiguity.
// Total = Human + AutoApproveConfigured + Unknown.Total. Applicable follows the funnel.
type AnalyticsFlowApproval struct {
	TimeBasis             string                       `json:"time_basis"`
	Population            string                       `json:"population"`
	Applicable            bool                         `json:"applicable"`
	Reason                *string                      `json:"reason"`
	Total                 int64                        `json:"total"`
	Human                 int64                        `json:"human"`
	AutoApproveConfigured int64                        `json:"auto_approve_configured"`
	Unknown               AnalyticsFlowApprovalUnknown `json:"unknown"`
}

// AnalyticsFlowApprovalUnknown is the unknown approvals and their reasons.
type AnalyticsFlowApprovalUnknown struct {
	Total   int64                        `json:"total"`
	Reasons AnalyticsFlowApprovalReasons `json:"reasons"`
}

// AnalyticsFlowApprovalReasons split the unknown approvals.
type AnalyticsFlowApprovalReasons struct {
	Ambiguous              int64 `json:"ambiguous"`
	LegacyUnbound          int64 `json:"legacy_unbound"`
	HistoricalApprovalOnly int64 `json:"historical_approval_only"`
	NoEvidence             int64 `json:"no_evidence"`
}

// AnalyticsFlowElapsed is the issue-run elapsed time over the creation cohort: terminal
// issue runs with first_started_at set and COALESCE(finished_at, status_since) -
// first_started_at >= 0 are valid observations (N); terminal runs without a valid pair are
// Invalid; non-terminal runs are Unfinished (Incomplete when any). Durations include held
// time. Applicable is false (reason not_applicable, all counts zero) when the kind selector
// excludes issue. The overall percentiles are null with no_observations only when N is 0.
// Histogram counts valid observations: bin i is [lower, upper), the last bin has a null
// upper. Buckets are by creation bucket, every bucket present, and compute their percentiles
// directly over their own population.
type AnalyticsFlowElapsed struct {
	TimeBasis  string                      `json:"time_basis"`
	Population string                      `json:"population"`
	Applicable bool                        `json:"applicable"`
	Reason     *string                     `json:"reason"`
	N          int64                       `json:"n"`
	Unfinished int64                       `json:"unfinished"`
	Invalid    int64                       `json:"invalid"`
	Incomplete bool                        `json:"incomplete"`
	P50Seconds AnalyticsNullableNumber     `json:"p50_seconds"`
	P90Seconds AnalyticsNullableNumber     `json:"p90_seconds"`
	Histogram  []AnalyticsFlowHistogramBin `json:"histogram"`
	Buckets    []AnalyticsFlowElapsedCell  `json:"buckets"`
}

// AnalyticsFlowHistogramBin is one histogram bin; UpperSeconds is nil for the last bin.
type AnalyticsFlowHistogramBin struct {
	LowerSeconds float64  `json:"lower_seconds"`
	UpperSeconds *float64 `json:"upper_seconds"`
	Count        int64    `json:"count"`
}

// AnalyticsFlowElapsedCell is one creation bucket. N is kept when the percentiles are
// suppressed: both are null with insufficient_observations when 0 < N < FlowMinPercentileN
// and with no_observations when N is 0. Incomplete is Unfinished > 0.
type AnalyticsFlowElapsedCell struct {
	Index      int                     `json:"index"`
	N          int64                   `json:"n"`
	Unfinished int64                   `json:"unfinished"`
	Invalid    int64                   `json:"invalid"`
	Incomplete bool                    `json:"incomplete"`
	P50Seconds AnalyticsNullableNumber `json:"p50_seconds"`
	P90Seconds AnalyticsNullableNumber `json:"p90_seconds"`
}

// DecodeAnalyticsFlowDoc decodes the document an Analytics*Flow statement returned,
// rejecting unknown keys and trailing data.
func DecodeAnalyticsFlowDoc(doc []byte) (AnalyticsFlowDoc, error) {
	var out AnalyticsFlowDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AnalyticsFlowDoc{}, fmt.Errorf("decode analytics flow document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AnalyticsFlowDoc{}, errors.New("decode analytics flow document: trailing data")
	}
	return out, nil
}
