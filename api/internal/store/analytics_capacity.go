package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Issue #4731: typed mirror of the jsonb document that AnalyticsOwnerCapacity and
// AnalyticsAllCapacity return. The SQL is the authority for the shape; DecodeAnalyticsCapacityDoc
// rejects unknown keys so a statement change that is not mirrored here fails loudly instead of
// being silently dropped by the handler's DTO mapping.
//
// Conventions:
//   - "index" in a bucket cell is the zero-based position in the bucket_starts / bucket_ends
//     arrays the caller passed; every bucket is present, including empty ones. With zero
//     buckets (an empty all-time range) the arrays are empty, never null.
//   - Rates and shares are JSON numbers rounded to six decimals; a null value always travels
//     with a reason from the constants below.
//   - Timestamps are UTC RFC 3339 strings with microseconds, independent of the session
//     TimeZone.

// Reasons the Capacity document and its handler use next to a null or unavailable value. The
// set is closed; AnalyticsReasonNoApplicableRuns (usage-wait share with no applicable run) is
// shared with the Spend document.
const (
	// AnalyticsReasonZeroDurationBucket marks a bucket whose duration is zero (a local-midnight
	// now leaves a final [now, now) bucket): the average is null, never a division by zero.
	AnalyticsReasonZeroDurationBucket = "zero_duration_bucket"
	// AnalyticsReasonZeroDurationRange marks a window whose duration is zero (From == Now).
	AnalyticsReasonZeroDurationRange = "zero_duration_range"
	// AnalyticsReasonNotTracked marks a measure the endpoint reports as not recorded.
	AnalyticsReasonNotTracked = "not_tracked"
	// AnalyticsReasonGaugeUnavailable marks a provider gauge whose read failed with a
	// non-timeout error while the snapshot stayed usable.
	AnalyticsReasonGaugeUnavailable = "gauge_unavailable"
	// AnalyticsReasonVaultStatusUnavailable marks Codex vault state the admin route could not
	// observe without waiting (admin-only).
	AnalyticsReasonVaultStatusUnavailable = "vault_status_unavailable"
	// AnalyticsReasonNotSelected marks a provider the harness selector excluded.
	AnalyticsReasonNotSelected = "not_selected"
	// AnalyticsReasonSharedQuotaNotAttributable marks the repo and kind selectors, which cannot
	// partition a shared provider quota.
	AnalyticsReasonSharedQuotaNotAttributable = "shared_quota_not_attributable"
)

// AnalyticsCapacityDoc is the whole historical Capacity document: the run-interval occupancy
// and the creation-cohort usage-wait measure. Current provider gauges come from the separate
// Analytics*AnthropicGauges and Analytics*CodexGauges statements.
type AnalyticsCapacityDoc struct {
	// GeneratedAt is the statement's @now, echoed in UTC.
	GeneratedAt string                      `json:"generated_at"`
	Population  AnalyticsCapacityPopulation `json:"population"`
	Occupancy   AnalyticsCapacityOccupancy  `json:"occupancy"`
	UsageWait   AnalyticsCapacityUsageWait  `json:"usage_wait"`
}

// AnalyticsCapacityPopulation counts the retained-interval population. Candidates are the
// applicable runs (ordinary lane, selectors applied) that could overlap [from, now): terminal
// runs ending after from and nonterminal runs created before now. Every candidate is exactly one
// of WithInterval, WithoutStart or InvalidInterval.
type AnalyticsCapacityPopulation struct {
	Candidates int64 `json:"candidates"`
	// WithInterval have first_started_at, start before now and end at or after the start.
	WithInterval int64 `json:"with_interval"`
	// WithoutStart have no first_started_at (never started, or started before it was recorded).
	WithoutStart int64 `json:"without_start"`
	// InvalidInterval end before they start or start at or after now.
	InvalidInterval int64 `json:"invalid_interval"`
	// EndFromStatusSince are terminal runs with no finished_at, so status_since stands in for
	// the end.
	EndFromStatusSince int64 `json:"end_from_status_since"`
	// Ongoing are nonterminal runs, whose end is the captured now.
	Ongoing int64 `json:"ongoing"`
	// CrossCheckKindIncluded are runs of kind cross_check that are not on the cross-check lane.
	CrossCheckKindIncluded int64 `json:"cross_check_kind_included"`
}

// AnalyticsCapacityOccupancy is the interval-overlap series.
type AnalyticsCapacityOccupancy struct {
	Window  AnalyticsCapacityOccupancyCell   `json:"window"`
	Buckets []AnalyticsCapacityOccupancyCell `json:"buckets"`
}

// AnalyticsCapacityOccupancyCell is one range's integral. RunSeconds is the sum over time of the
// number of concurrent intervals; AverageConcurrentRuns is RunSeconds over DurationSeconds,
// null with a reason when the duration is zero.
type AnalyticsCapacityOccupancyCell struct {
	// Index is the bucket position; absent (zero) on the window cell.
	Index                 int      `json:"index"`
	RunSeconds            float64  `json:"run_seconds"`
	DurationSeconds       float64  `json:"duration_seconds"`
	AverageConcurrentRuns *float64 `json:"average_concurrent_runs"`
	Reason                *string  `json:"reason"`
}

// AnalyticsCapacityUsageWait is the creation-cohort usage-wait measure.
type AnalyticsCapacityUsageWait struct {
	Window  AnalyticsCapacityUsageWaitCell   `json:"window"`
	Buckets []AnalyticsCapacityUsageWaitCell `json:"buckets"`
}

// AnalyticsCapacityUsageWaitCell counts applicable runs created in the range (N) and those with
// limit_wait_count > 0 (Count); Share is Count / N, null with no_applicable_runs when N is zero.
type AnalyticsCapacityUsageWaitCell struct {
	Index  int      `json:"index"`
	Count  int64    `json:"count"`
	N      int64    `json:"n"`
	Share  *float64 `json:"share"`
	Reason *string  `json:"reason"`
}

// DecodeAnalyticsCapacityDoc decodes the document an Analytics*Capacity statement returned,
// rejecting unknown keys and trailing data.
func DecodeAnalyticsCapacityDoc(doc []byte) (AnalyticsCapacityDoc, error) {
	var out AnalyticsCapacityDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AnalyticsCapacityDoc{}, fmt.Errorf("decode analytics capacity document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AnalyticsCapacityDoc{}, errors.New("decode analytics capacity document: trailing data")
	}
	return out, nil
}
