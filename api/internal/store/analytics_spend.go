package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// Issue #4684: typed mirror of the jsonb document that AnalyticsOwnerSpend and
// AnalyticsAllSpend return. The SQL is the authority for the shape; DecodeAnalyticsSpendDoc
// rejects unknown keys so a statement change that is not mirrored here fails loudly instead
// of being silently dropped by the handler's DTO mapping.
//
// Conventions shared by every type below:
//   - Dollar amounts are numeric strings (cost_usd keeps six decimals), never floats, so
//     sums reconcile exactly. They are a recorded-spend figure, not a bill.
//   - Rates (coverage, failure rate, completion ratio, cost share, cache-read ratio) are
//     JSON numbers rounded to six decimals; a value of null always travels with a reason
//     from AnalyticsReason*.
//   - "index" in a bucket cell or rolling point is the zero-based position in the
//     bucket_starts / bucket_ends (resp. roll_starts / roll_ends) arrays the caller passed;
//     every bucket is present, including empty ones.
//   - Timestamps are UTC RFC 3339 strings with microseconds, independent of the session
//     TimeZone.

// Null reasons the document uses next to a null value. The set is closed.
const (
	AnalyticsReasonNoCompletedDeliveryRuns = "no_completed_delivery_runs"
	AnalyticsReasonNoMergedPRs             = "no_merged_prs"
	AnalyticsReasonNoApplicableRuns        = "no_applicable_runs"
	AnalyticsReasonNoInputTokens           = "no_input_tokens"
	AnalyticsReasonNoFinishedRuns          = "no_finished_runs"
	AnalyticsReasonNotApplicable           = "not_applicable"
)

// AnalyticsSpendDoc is the whole Spend document.
type AnalyticsSpendDoc struct {
	// GeneratedAt is the statement's @now, echoed in UTC.
	GeneratedAt string `json:"generated_at"`
	// UntrackedChatRuns counts chat runs created in the current window. Chat is untracked,
	// not a recorded zero: chat runs appear in no cost, token, ratio, coverage or top-run
	// figure.
	UntrackedChatRuns int64 `json:"untracked_chat_runs"`
	// Totals holds the current window and, for bounded windows only, the previous one.
	Totals AnalyticsSpendTotals `json:"totals"`
	// Buckets is the ungrouped spend per bucket, one cell per bucket index.
	Buckets []AnalyticsSpendCell `json:"buckets"`
	// Split is the top-five-plus-Other grouping of the current window.
	Split AnalyticsSplit `json:"split"`
	// CostPerCompletedRun is all recorded non-chat cost divided by completed Delivery runs.
	CostPerCompletedRun AnalyticsRatioPanel `json:"cost_per_completed_run"`
	// CostPerMergedPR is the cost of Delivery runs carrying a PR divided by the distinct
	// witnessed merged PRs, with each PR attributed to its earliest run.
	CostPerMergedPR AnalyticsRatioPanel `json:"cost_per_merged_pr"`
	// CacheRead is the recorded cache-read ratio of the current window (not proven savings).
	CacheRead AnalyticsCacheRead `json:"cache_read"`
	// TopRuns are the ten most expensive current-window runs, most expensive first.
	TopRuns []AnalyticsTopRun `json:"top_runs"`
	// Breakdown is the top-50-plus-Other table of the current window.
	Breakdown AnalyticsBreakdown `json:"breakdown"`
}

// AnalyticsSpendTotals are the headline totals.
type AnalyticsSpendTotals struct {
	Current AnalyticsSpendBlock `json:"current"`
	// Previous is nil (key absent) when the window has no previous period (window=all).
	Previous *AnalyticsSpendBlock `json:"previous,omitempty"`
}

// AnalyticsSpendBlock is the spend aggregate every grouping level shares.
type AnalyticsSpendBlock struct {
	// Cost is the recorded dollar sum as an exact numeric string. Partial dollars are kept:
	// a run with unreported usage still contributes what was recorded.
	Cost                string `json:"cost"`
	InputTokens         int64  `json:"input_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	// Runs is the number of DISTINCT applicable non-chat runs behind the aggregate.
	Runs       int64               `json:"runs"`
	Disclosure AnalyticsDisclosure `json:"disclosure"`
	Coverage   AnalyticsCoverage   `json:"coverage"`
}

// AnalyticsSpendCell is one bucket's spend aggregate.
type AnalyticsSpendCell struct {
	Index int `json:"index"`
	AnalyticsSpendBlock
}

// AnalyticsDisclosure counts DISTINCT runs by run-level cost status. Incomplete is true when
// any run in the population is not metered (subscription, unreported or missing usage), so
// the dollars cannot be read as the full cost.
type AnalyticsDisclosure struct {
	Metered      int64 `json:"metered"`
	Subscription int64 `json:"subscription"`
	Unreported   int64 `json:"unreported"`
	Missing      int64 `json:"missing"`
	Incomplete   bool  `json:"incomplete"`
}

// AnalyticsCoverage is metered runs over applicable non-chat runs; runs with no recorded
// usage stay in the denominator. Value is null (reason no_applicable_runs) for an empty
// population.
type AnalyticsCoverage struct {
	Metered    int64    `json:"metered"`
	Applicable int64    `json:"applicable"`
	Value      *float64 `json:"value"`
	Reason     *string  `json:"reason"`
}

// AnalyticsSplit is the grouped spend. For dimension usage_model a run contributes to every
// model it used (runs with no usage rows go to the "unknown" group), so group counts and
// disclosures are not additive: NonAdditive is true.
type AnalyticsSplit struct {
	Dimension   string                `json:"dimension"`
	NonAdditive bool                  `json:"non_additive"`
	Groups      []AnalyticsSplitGroup `json:"groups"`
}

// AnalyticsSplitGroup is one ranked group: ranks 1..5 by current-window cost, then Other
// (IsOther, rank 6, no key) holding every remaining group with its own distinct-run counts.
type AnalyticsSplitGroup struct {
	Rank    int  `json:"rank"`
	IsOther bool `json:"is_other"`
	// Key is the group key: user id, model, harness, repo id, "no_repo",
	// "inaccessible_repo" or kind. Nil for Other.
	Key *string `json:"key"`
	// Label is the user's email or the repo path; nil when the dimension has none or the
	// repo is not revealed.
	Label *string `json:"label"`
	// ID is the user or repo UUID; nil for hidden repos, "no_repo" and other dimensions.
	ID      *uuid.UUID           `json:"id"`
	Totals  AnalyticsSpendBlock  `json:"totals"`
	Buckets []AnalyticsSpendCell `json:"buckets"`
}

// AnalyticsRatioStat is a numerator, a denominator and their quotient. Numerator is a
// dollar string; Value is the quotient as a six-decimal dollar string, nil with a reason
// when the denominator is zero. Disclosure covers the numerator's runs.
type AnalyticsRatioStat struct {
	Numerator   string              `json:"numerator"`
	Denominator int64               `json:"denominator"`
	Value       *string             `json:"value"`
	Reason      *string             `json:"reason"`
	Disclosure  AnalyticsDisclosure `json:"disclosure"`
}

// AnalyticsRatioCell is a ratio at one bucket (or rolling) index.
type AnalyticsRatioCell struct {
	Index int `json:"index"`
	AnalyticsRatioStat
}

// AnalyticsRatioPanel is a ratio's headline, previous headline, per-bucket cells and
// seven-day rolling points (aligned with the buckets).
type AnalyticsRatioPanel struct {
	Current AnalyticsRatioStat `json:"current"`
	// Previous is nil (key absent) when the window has no previous period.
	Previous *AnalyticsRatioStat  `json:"previous,omitempty"`
	Buckets  []AnalyticsRatioCell `json:"buckets"`
	Rolling  []AnalyticsRatioCell `json:"rolling"`
}

// AnalyticsCacheRead is read / (input + read + creation) over the current window's
// non-chat runs. Value is nil with reason no_input_tokens when the denominator is zero.
type AnalyticsCacheRead struct {
	CacheReadTokens     int64               `json:"cache_read_tokens"`
	InputTokens         int64               `json:"input_tokens"`
	CacheCreationTokens int64               `json:"cache_creation_tokens"`
	Denominator         int64               `json:"denominator"`
	Value               *float64            `json:"value"`
	Reason              *string             `json:"reason"`
	Disclosure          AnalyticsDisclosure `json:"disclosure"`
}

// AnalyticsTopRun is one of the most expensive runs. UsageStatus is the run's own cost
// status (metered, subscription, unreported or missing). The repo fields follow the same
// visibility rule as the groups.
type AnalyticsTopRun struct {
	RunID               uuid.UUID  `json:"run_id"`
	Kind                string     `json:"kind"`
	Harness             string     `json:"harness"`
	RunStatus           string     `json:"run_status"`
	UsageStatus         string     `json:"usage_status"`
	PrimaryModel        string     `json:"primary_model"`
	CreatedAt           string     `json:"created_at"`
	Cost                string     `json:"cost"`
	InputTokens         int64      `json:"input_tokens"`
	CacheReadTokens     int64      `json:"cache_read_tokens"`
	CacheCreationTokens int64      `json:"cache_creation_tokens"`
	OutputTokens        int64      `json:"output_tokens"`
	RepoKey             string     `json:"repo_key"`
	RepoID              *uuid.UUID `json:"repo_id"`
	RepoPath            *string    `json:"repo_path"`
	UserID              uuid.UUID  `json:"user_id"`
	UserEmail           string     `json:"user_email"`
}

// AnalyticsNullableNumber is a number that is null with a reason when undefined.
type AnalyticsNullableNumber struct {
	Value  *float64 `json:"value"`
	Reason *string  `json:"reason"`
}

// AnalyticsOutcomes are the finished-run outcome classes (the run_outcome_classes view):
// failed excludes plan_rejected and base_diverged, which are their own classes.
type AnalyticsOutcomes struct {
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	Failed       int64 `json:"failed"`
}

// AnalyticsBreakdown is the ranked breakdown table.
type AnalyticsBreakdown struct {
	Dimension string                  `json:"dimension"`
	Rows      []AnalyticsBreakdownRow `json:"rows"`
}

// AnalyticsBreakdownRow is one ranked row (ranks 1..50, then Other at rank 51 with no key).
// Each run is counted once in exactly one row.
type AnalyticsBreakdownRow struct {
	Rank    int        `json:"rank"`
	IsOther bool       `json:"is_other"`
	Key     *string    `json:"key"`
	Label   *string    `json:"label"`
	ID      *uuid.UUID `json:"id"`
	AnalyticsSpendBlock
	// Finished is completed + cancelled + plan_rejected + base_diverged + failed; in-flight
	// runs are in no finished figure.
	Finished int64             `json:"finished"`
	Outcomes AnalyticsOutcomes `json:"outcomes"`
	// FailureRate is failed / finished and CompletionRatio is completed / finished; both are
	// null with no_finished_runs.
	FailureRate     AnalyticsNullableNumber `json:"failure_rate"`
	CompletionRatio AnalyticsNullableNumber `json:"completion_ratio"`
	// CostShare is the row's cost over the window's total cost; null with not_applicable
	// when the total is zero.
	CostShare AnalyticsNullableNumber `json:"cost_share"`
	// ElapsedP50Seconds and ElapsedP90Seconds are percentiles of
	// COALESCE(finished_at, status_since) - created_at over the row's finished runs, in
	// seconds; for Other they are computed over its own population.
	ElapsedP50Seconds AnalyticsNullableNumber `json:"elapsed_p50_seconds"`
	ElapsedP90Seconds AnalyticsNullableNumber `json:"elapsed_p90_seconds"`
	// MergedPRs is the number of distinct witnessed merged PRs referenced by the row's
	// Delivery runs.
	MergedPRs int64 `json:"merged_prs"`
}

// DecodeAnalyticsSpendDoc decodes the document an Analytics*Spend statement returned,
// rejecting unknown keys and trailing data.
func DecodeAnalyticsSpendDoc(doc []byte) (AnalyticsSpendDoc, error) {
	var out AnalyticsSpendDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return AnalyticsSpendDoc{}, fmt.Errorf("decode analytics spend document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AnalyticsSpendDoc{}, errors.New("decode analytics spend document: trailing data")
	}
	return out, nil
}
