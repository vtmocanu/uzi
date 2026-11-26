package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// Issue #4685: typed mirror of the jsonb document that AnalyticsOwnerReliability and
// AnalyticsAllReliability return. The SQL is the authority for the shape;
// DecodeAnalyticsReliabilityDoc rejects unknown keys, trailing data and mistyped values, so a
// statement change that adds a key or changes a type is not silently dropped by the handler's
// DTO mapping. It does not enforce that every key is present (as DecodeAnalyticsSpendDoc): a
// removed key decodes to its zero value.
// The conventions of analytics_spend.go apply: rates are JSON numbers rounded to six decimals
// that are null together with a reason, "index" is the zero-based position in the bucket
// (resp. rolling) arrays the caller passed, and timestamps are UTC RFC 3339 strings.

// AnalyticsReasonInsufficientRuns is the null reason of a rate computed over fewer than five
// finished runs (a breakdown row or a rolling point); the raw counts stay in the document.
const AnalyticsReasonInsufficientRuns = "insufficient_runs"

// AnalyticsReliabilityDoc is the whole Reliability document.
type AnalyticsReliabilityDoc struct {
	// GeneratedAt is the statement's @now, echoed in UTC.
	GeneratedAt string `json:"generated_at"`
	// Totals holds the current window and, for bounded windows only, the previous one.
	Totals AnalyticsReliabilityTotals `json:"totals"`
	// Buckets is one outcome cell per bucket index (every bucket present, empty ones too).
	Buckets []AnalyticsReliabilityBucket `json:"buckets"`
	// Causes is the top-five-plus-Other failure-origin grouping of the current window.
	Causes AnalyticsReliabilityCauses `json:"causes"`
	// Rolling is the seven-day rolling failure rate, overall and per harness.
	Rolling AnalyticsReliabilityRolling `json:"rolling"`
	// Breakdown is the top-50-plus-Other table of the current window.
	Breakdown AnalyticsReliabilityBreakdown `json:"breakdown"`
	// NeedsLanding counts current-window failed runs with recoverable work (a human-landable
	// origin plus an available capture or a preserved patch). The work may already have landed.
	NeedsLanding int64 `json:"needs_landing"`
}

// AnalyticsReliabilityTotals are the headline blocks.
type AnalyticsReliabilityTotals struct {
	Current AnalyticsReliabilityBlock `json:"current"`
	// Previous is nil (key absent) when the window has no previous period (window=all).
	Previous *AnalyticsReliabilityBlock `json:"previous,omitempty"`
}

// AnalyticsReliabilityBlock is the outcome aggregate of one population. Finished is
// completed + cancelled + plan_rejected + base_diverged + failed; in-flight runs are in no
// finished figure. The failure rate is failed / finished, never suppressed: null with
// no_finished_runs only when nothing finished. RequeuedRuns counts runs whose retained
// requeue_count is above zero.
type AnalyticsReliabilityBlock struct {
	Finished     int64                   `json:"finished"`
	Outcomes     AnalyticsOutcomes       `json:"outcomes"`
	FailureRate  AnalyticsNullableNumber `json:"failure_rate"`
	RequeuedRuns int64                   `json:"requeued_runs"`
}

// AnalyticsReliabilityBucket is one bucket's finished count and outcome classes.
type AnalyticsReliabilityBucket struct {
	Index    int               `json:"index"`
	Finished int64             `json:"finished"`
	Outcomes AnalyticsOutcomes `json:"outcomes"`
}

// AnalyticsReliabilityCauses groups the current window's failed runs by failure origin
// (NULL origin groups as "unknown"). The item runs sum to Failed.
type AnalyticsReliabilityCauses struct {
	Failed int64                       `json:"failed"`
	Items  []AnalyticsReliabilityCause `json:"items"`
}

// AnalyticsReliabilityCause is one ranked cause: ranks 1..5, then Other at rank 6 (IsOther,
// nil Origin). Share is runs / failed, six decimals.
type AnalyticsReliabilityCause struct {
	Rank    int     `json:"rank"`
	IsOther bool    `json:"is_other"`
	Origin  *string `json:"origin"`
	Runs    int64   `json:"runs"`
	Share   float64 `json:"share"`
}

// AnalyticsReliabilityRolling is the overall series and one series per requested harness,
// each with exactly one point per rolling range.
type AnalyticsReliabilityRolling struct {
	Overall   []AnalyticsReliabilityPoint         `json:"overall"`
	Harnesses []AnalyticsReliabilityHarnessSeries `json:"harnesses"`
}

// AnalyticsReliabilityHarnessSeries is one harness's rolling points.
type AnalyticsReliabilityHarnessSeries struct {
	Harness string                      `json:"harness"`
	Points  []AnalyticsReliabilityPoint `json:"points"`
}

// AnalyticsReliabilityPoint is one rolling range: real finished and failed counts (0/0 when
// empty) and a rate that is null with insufficient_runs below five finished runs.
type AnalyticsReliabilityPoint struct {
	Index    int      `json:"index"`
	Finished int64    `json:"finished"`
	Failed   int64    `json:"failed"`
	Value    *float64 `json:"value"`
	Reason   *string  `json:"reason"`
}

// AnalyticsReliabilityBreakdown is the ranked breakdown table.
type AnalyticsReliabilityBreakdown struct {
	Dimension string                      `json:"dimension"`
	Rows      []AnalyticsReliabilityGroup `json:"rows"`
}

// AnalyticsReliabilityGroup is one ranked row (ranks 1..50 by finished runs, then Other at
// rank 51 with no key). Each run is counted once in exactly one row. N is the row's finished
// runs; FailureRate is null with insufficient_runs below five of them.
type AnalyticsReliabilityGroup struct {
	Rank         int                     `json:"rank"`
	IsOther      bool                    `json:"is_other"`
	Key          *string                 `json:"key"`
	Label        *string                 `json:"label"`
	ID           *uuid.UUID              `json:"id"`
	N            int64                   `json:"n"`
	Outcomes     AnalyticsOutcomes       `json:"outcomes"`
	FailureRate  AnalyticsNullableNumber `json:"failure_rate"`
	RequeuedRuns int64                   `json:"requeued_runs"`
}

// DecodeAnalyticsReliabilityDoc decodes the document an Analytics*Reliability statement
// returned, rejecting unknown keys, trailing data and mistyped values; missing keys are not an error.
func DecodeAnalyticsReliabilityDoc(doc []byte) (AnalyticsReliabilityDoc, error) {
	var out AnalyticsReliabilityDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AnalyticsReliabilityDoc{}, fmt.Errorf("decode analytics reliability document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AnalyticsReliabilityDoc{}, errors.New("decode analytics reliability document: trailing data")
	}
	return out, nil
}
