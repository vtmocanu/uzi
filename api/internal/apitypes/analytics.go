package apitypes

import "time"

// Issue #4684: wire DTOs of the scoped usage analytics API
// (GET /api/analytics/{options,spend} and GET /api/admin/analytics/{options,spend}).
//
// Conventions shared by every type below:
//   - Dollar amounts (cost_usd, numerator_usd and ratio values) are float64 converted from the
//     exact numeric strings the store returns; the tolerance of any sum over them is 1e-6 USD.
//     They are a recorded-spend figure, not a bill.
//   - A rate or ratio that is null always travels with a reason from the closed set no_completed_delivery_runs,
//     no_merged_prs, no_applicable_runs, no_input_tokens, no_finished_runs, not_applicable.
//   - "index" in a cell is the zero-based position in the response's buckets (or rolling) array.
//   - Timestamps are UTC RFC 3339.

// AnalyticsScopeDTO names whose runs a response covers: the route that served it and the
// user it is scoped to ("all" on an admin response that spans every user, else a user id).
type AnalyticsScopeDTO struct {
	// Route is "owner" (the caller's own runs) or "admin".
	Route string `json:"route"`
	User  string `json:"user"`
}

// AnalyticsBucketDTO is one half-open calendar bucket [start, end) labelled by a civil date
// in the response's timezone. Partial is true for the bucket that ends at generated_at.
type AnalyticsBucketDTO struct {
	Label   string    `json:"label"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Partial bool      `json:"partial"`
}

// AnalyticsRollingDTO is the seven-day rolling range aligned with the bucket of the same
// index. Partial is true when its seven days are not fully covered by history.
type AnalyticsRollingDTO struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Partial bool      `json:"partial"`
}

// AnalyticsEnvelopeDTO is the calendar every Spend panel is indexed against.
type AnalyticsEnvelopeDTO struct {
	Scope       AnalyticsScopeDTO `json:"scope"`
	GeneratedAt time.Time         `json:"generated_at"`
	// Window is 7d, 30d, 90d or all. Timezone is the IANA name the calendar was computed in.
	Window   string    `json:"window"`
	Timezone string    `json:"timezone"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	// PreviousFrom and PreviousTo are absent for window=all.
	PreviousFrom *time.Time `json:"previous_from,omitempty"`
	PreviousTo   *time.Time `json:"previous_to,omitempty"`
	// HistoryStart is the start of the ISO week of the scope's earliest matching run; null
	// when the scope has none.
	HistoryStart *time.Time `json:"history_start"`
	// BucketWidthDays is 1 for day buckets and 7 times the week count for window=all.
	BucketWidthDays int                   `json:"bucket_width_days"`
	Buckets         []AnalyticsBucketDTO  `json:"buckets"`
	Rolling         []AnalyticsRollingDTO `json:"rolling"`
}

// AnalyticsRepoOptionDTO is one repo the caller may filter by.
type AnalyticsRepoOptionDTO struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// AnalyticsRepoOptionsDTO is the repo list, capped at 200 items. Truncated is true when more
// exist; a valid repo that was cut off is still accepted as a filter.
type AnalyticsRepoOptionsDTO struct {
	Items     []AnalyticsRepoOptionDTO `json:"items"`
	Truncated bool                     `json:"truncated"`
}

// AnalyticsUserOptionDTO is one user an admin may scope to.
type AnalyticsUserOptionDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// AnalyticsUserOptionsDTO is the user list, capped at 200 items like the repo list.
type AnalyticsUserOptionsDTO struct {
	Items     []AnalyticsUserOptionDTO `json:"items"`
	Truncated bool                     `json:"truncated"`
}

// AnalyticsOptionsDTO is the filter vocabulary for the caller's scope.
type AnalyticsOptionsDTO struct {
	Scope       AnalyticsScopeDTO `json:"scope"`
	GeneratedAt time.Time         `json:"generated_at"`
	// SelectedUser is "all" for an admin response spanning every user, else the user id.
	SelectedUser string                  `json:"selected_user"`
	Repos        AnalyticsRepoOptionsDTO `json:"repos"`
	// Users is null with UsersReason "not_applicable" on the owner route.
	Users       *AnalyticsUserOptionsDTO `json:"users"`
	UsersReason *string                  `json:"users_reason"`
	Windows     []string                 `json:"windows"`
	Kinds       []string                 `json:"kinds"`
	Harnesses   []string                 `json:"harnesses"`
	Splits      []string                 `json:"splits"`
	Breakdowns  []string                 `json:"breakdowns"`
}

// AnalyticsDisclosureDTO counts DISTINCT runs by run-level cost status. Incomplete is true
// when any run is not metered, so the dollars cannot be read as the full cost.
type AnalyticsDisclosureDTO struct {
	Metered      int64 `json:"metered"`
	Subscription int64 `json:"subscription"`
	Unreported   int64 `json:"unreported"`
	Missing      int64 `json:"missing"`
	Incomplete   bool  `json:"incomplete"`
}

// AnalyticsCoverageDTO is metered runs over applicable non-chat runs; value is null with
// reason no_applicable_runs for an empty population.
type AnalyticsCoverageDTO struct {
	Metered    int64    `json:"metered"`
	Applicable int64    `json:"applicable"`
	Value      *float64 `json:"value"`
	Reason     *string  `json:"reason"`
}

// AnalyticsSpendBlockDTO is the spend aggregate every grouping level shares. Runs is the
// number of DISTINCT applicable non-chat runs behind it.
type AnalyticsSpendBlockDTO struct {
	CostUSD             float64                `json:"cost_usd"`
	InputTokens         int64                  `json:"input_tokens"`
	CacheReadTokens     int64                  `json:"cache_read_tokens"`
	CacheCreationTokens int64                  `json:"cache_creation_tokens"`
	OutputTokens        int64                  `json:"output_tokens"`
	Runs                int64                  `json:"runs"`
	Disclosure          AnalyticsDisclosureDTO `json:"disclosure"`
	Coverage            AnalyticsCoverageDTO   `json:"coverage"`
}

// AnalyticsSpendCellDTO is one bucket's spend aggregate.
type AnalyticsSpendCellDTO struct {
	Index int `json:"index"`
	AnalyticsSpendBlockDTO
}

// AnalyticsSpendTotalsDTO holds the current window and, for bounded windows only, the previous one.
type AnalyticsSpendTotalsDTO struct {
	Current  AnalyticsSpendBlockDTO  `json:"current"`
	Previous *AnalyticsSpendBlockDTO `json:"previous,omitempty"`
}

// AnalyticsSplitGroupDTO is one ranked group: ranks 1..5, then Other (is_other, no key).
// Label is the user email or repo path, and ID the user or repo id; both are null when the
// dimension has none or the repo is not revealed (key "inaccessible_repo").
type AnalyticsSplitGroupDTO struct {
	Rank    int                     `json:"rank"`
	IsOther bool                    `json:"is_other"`
	Key     *string                 `json:"key"`
	Label   *string                 `json:"label"`
	ID      *string                 `json:"id"`
	Totals  AnalyticsSpendBlockDTO  `json:"totals"`
	Buckets []AnalyticsSpendCellDTO `json:"buckets"`
}

// AnalyticsSplitDTO is the grouped spend. For dimension usage_model a run counts in every
// model it used, so group counts and disclosures are not additive (non_additive).
type AnalyticsSplitDTO struct {
	Dimension   string                   `json:"dimension"`
	NonAdditive bool                     `json:"non_additive"`
	Groups      []AnalyticsSplitGroupDTO `json:"groups"`
}

// AnalyticsRatioDTO is a numerator, a denominator and their quotient in dollars. Value is
// null with a reason when the denominator is zero. Disclosure covers the numerator's runs.
type AnalyticsRatioDTO struct {
	NumeratorUSD float64                `json:"numerator_usd"`
	Denominator  int64                  `json:"denominator"`
	Value        *float64               `json:"value"`
	Reason       *string                `json:"reason"`
	Disclosure   AnalyticsDisclosureDTO `json:"disclosure"`
}

// AnalyticsRatioCellDTO is a ratio at one bucket (or rolling) index.
type AnalyticsRatioCellDTO struct {
	Index int `json:"index"`
	AnalyticsRatioDTO
}

// AnalyticsRatioPanelDTO is a ratio's headline, previous headline (absent for window=all),
// per-bucket cells and seven-day rolling points.
type AnalyticsRatioPanelDTO struct {
	Current  AnalyticsRatioDTO       `json:"current"`
	Previous *AnalyticsRatioDTO      `json:"previous,omitempty"`
	Buckets  []AnalyticsRatioCellDTO `json:"buckets"`
	Rolling  []AnalyticsRatioCellDTO `json:"rolling"`
}

// AnalyticsCacheReadDTO is read / (input + read + creation) over the window's non-chat runs.
// It is a recorded ratio, not proven savings.
type AnalyticsCacheReadDTO struct {
	CacheReadTokens     int64                  `json:"cache_read_tokens"`
	InputTokens         int64                  `json:"input_tokens"`
	CacheCreationTokens int64                  `json:"cache_creation_tokens"`
	Denominator         int64                  `json:"denominator"`
	Value               *float64               `json:"value"`
	Reason              *string                `json:"reason"`
	Disclosure          AnalyticsDisclosureDTO `json:"disclosure"`
}

// AnalyticsTopRunDTO is one of the most expensive runs of the window. repo_key is the repo
// id, "no_repo" or "inaccessible_repo"; repo_id and repo_path are null unless revealed.
type AnalyticsTopRunDTO struct {
	RunID               string    `json:"run_id"`
	Kind                string    `json:"kind"`
	Harness             string    `json:"harness"`
	RunStatus           string    `json:"run_status"`
	UsageStatus         string    `json:"usage_status"`
	PrimaryModel        string    `json:"primary_model"`
	CreatedAt           time.Time `json:"created_at"`
	CostUSD             float64   `json:"cost_usd"`
	InputTokens         int64     `json:"input_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	RepoKey             string    `json:"repo_key"`
	RepoID              *string   `json:"repo_id"`
	RepoPath            *string   `json:"repo_path"`
	UserID              string    `json:"user_id"`
	UserEmail           string    `json:"user_email"`
}

// AnalyticsNullableNumberDTO is a number that is null with a reason when undefined.
type AnalyticsNullableNumberDTO struct {
	Value  *float64 `json:"value"`
	Reason *string  `json:"reason"`
}

// AnalyticsOutcomesDTO are the finished-run outcome classes; failed excludes plan_rejected
// and base_diverged, which are their own classes.
type AnalyticsOutcomesDTO struct {
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	BaseDiverged int64 `json:"base_diverged"`
	Failed       int64 `json:"failed"`
}

// AnalyticsBreakdownRowDTO is one ranked row (ranks 1..50, then Other with no key). Each
// run is counted once in exactly one row.
type AnalyticsBreakdownRowDTO struct {
	Rank    int     `json:"rank"`
	IsOther bool    `json:"is_other"`
	Key     *string `json:"key"`
	Label   *string `json:"label"`
	ID      *string `json:"id"`
	AnalyticsSpendBlockDTO
	// Finished is completed + cancelled + plan_rejected + base_diverged + failed.
	Finished        int64                      `json:"finished"`
	Outcomes        AnalyticsOutcomesDTO       `json:"outcomes"`
	FailureRate     AnalyticsNullableNumberDTO `json:"failure_rate"`
	CompletionRatio AnalyticsNullableNumberDTO `json:"completion_ratio"`
	CostShare       AnalyticsNullableNumberDTO `json:"cost_share"`
	// ElapsedP50Seconds and ElapsedP90Seconds are percentiles of finished-or-last-status time
	// minus creation time, in seconds.
	ElapsedP50Seconds AnalyticsNullableNumberDTO `json:"elapsed_p50_seconds"`
	ElapsedP90Seconds AnalyticsNullableNumberDTO `json:"elapsed_p90_seconds"`
	// MergedPRs is the number of distinct witnessed merged PRs referenced by the row's runs.
	MergedPRs int64 `json:"merged_prs"`
}

// AnalyticsBreakdownDTO is the ranked breakdown table.
type AnalyticsBreakdownDTO struct {
	Dimension string                     `json:"dimension"`
	Rows      []AnalyticsBreakdownRowDTO `json:"rows"`
}

// Issue #4685: the Reliability response (GET /api/analytics/reliability and
// GET /api/admin/analytics/reliability). It reuses the envelope, outcome classes and nullable
// number above. A failure rate is failed / finished and is never suppressed for a small sample
// (null with no_finished_runs only when nothing finished); a breakdown row or rolling point over
// fewer than five finished runs carries null with insufficient_runs and keeps its raw counts.

// AnalyticsReliabilityBlockDTO is the outcome aggregate of one population. Finished is
// completed + cancelled + plan_rejected + base_diverged + failed; in-flight runs are in no
// finished figure. RequeuedRuns counts runs whose retained requeue count is above zero.
type AnalyticsReliabilityBlockDTO struct {
	Finished     int64                      `json:"finished"`
	Outcomes     AnalyticsOutcomesDTO       `json:"outcomes"`
	FailureRate  AnalyticsNullableNumberDTO `json:"failure_rate"`
	RequeuedRuns int64                      `json:"requeued_runs"`
}

// AnalyticsReliabilityTotalsDTO holds the current window and, for bounded windows only, the previous one.
type AnalyticsReliabilityTotalsDTO struct {
	Current  AnalyticsReliabilityBlockDTO  `json:"current"`
	Previous *AnalyticsReliabilityBlockDTO `json:"previous,omitempty"`
}

// AnalyticsReliabilityCellDTO is one bucket's finished count and outcome classes.
type AnalyticsReliabilityCellDTO struct {
	Index    int                  `json:"index"`
	Finished int64                `json:"finished"`
	Outcomes AnalyticsOutcomesDTO `json:"outcomes"`
}

// AnalyticsReliabilityCauseDTO is one ranked failure origin: ranks 1..5, then Other (is_other,
// no origin). Share is runs / the window's failed runs.
type AnalyticsReliabilityCauseDTO struct {
	Rank    int     `json:"rank"`
	IsOther bool    `json:"is_other"`
	Origin  *string `json:"origin"`
	Runs    int64   `json:"runs"`
	Share   float64 `json:"share"`
}

// AnalyticsReliabilityCausesDTO groups the window's failed runs by failure origin; the item
// runs sum to Failed. Failed excludes plan_rejected and base_diverged, which are their own classes.
type AnalyticsReliabilityCausesDTO struct {
	Failed int64                          `json:"failed"`
	Items  []AnalyticsReliabilityCauseDTO `json:"items"`
}

// AnalyticsReliabilityPointDTO is one seven-day rolling range: the real finished and failed
// counts and a rate that is null with a reason (insufficient_runs below five finished runs,
// not_applicable when the filters select no delivery kind).
type AnalyticsReliabilityPointDTO struct {
	Index    int      `json:"index"`
	Finished int64    `json:"finished"`
	Failed   int64    `json:"failed"`
	Value    *float64 `json:"value"`
	Reason   *string  `json:"reason"`
}

// AnalyticsReliabilityHarnessSeriesDTO is one harness's rolling points, one per rolling range.
type AnalyticsReliabilityHarnessSeriesDTO struct {
	Harness string                         `json:"harness"`
	Points  []AnalyticsReliabilityPointDTO `json:"points"`
}

// AnalyticsReliabilityRollingDTO is the overall rolling failure rate and one series per harness.
type AnalyticsReliabilityRollingDTO struct {
	Overall   []AnalyticsReliabilityPointDTO         `json:"overall"`
	Harnesses []AnalyticsReliabilityHarnessSeriesDTO `json:"harnesses"`
}

// AnalyticsReliabilityRowDTO is one ranked breakdown row (ranks 1..50 by finished runs, then
// Other with no key). Each run is counted once in exactly one row. N is the row's finished
// runs. Label and ID are the user email and id, or the repo path and id; null otherwise, and
// for a repo the caller may not see (key "inaccessible_repo").
type AnalyticsReliabilityRowDTO struct {
	Rank         int                        `json:"rank"`
	IsOther      bool                       `json:"is_other"`
	Key          *string                    `json:"key"`
	Label        *string                    `json:"label"`
	ID           *string                    `json:"id"`
	N            int64                      `json:"n"`
	Outcomes     AnalyticsOutcomesDTO       `json:"outcomes"`
	FailureRate  AnalyticsNullableNumberDTO `json:"failure_rate"`
	RequeuedRuns int64                      `json:"requeued_runs"`
}

// AnalyticsReliabilityBreakdownDTO is the ranked breakdown table.
type AnalyticsReliabilityBreakdownDTO struct {
	Dimension string                       `json:"dimension"`
	Rows      []AnalyticsReliabilityRowDTO `json:"rows"`
}

// AnalyticsReliabilityLatestFailureDTO is the scope's most recent failure over its lifetime
// (not bounded by the window), restricted by the repo, harness and delivery-kind filters.
// CompletedSinceLastFailure counts completed runs of the same scope after FailedAt.
type AnalyticsReliabilityLatestFailureDTO struct {
	FailedAt                  time.Time `json:"failed_at"`
	Origin                    string    `json:"origin"`
	RunID                     string    `json:"run_id"`
	UserID                    string    `json:"user_id"`
	CompletedSinceLastFailure int64     `json:"completed_since_last_failure"`
}

// AnalyticsReliabilityNeedsLandingDTO counts the window's failed runs that left recoverable
// work (a human-landable failure origin plus an available capture or a preserved patch). Caveat
// is always "work_may_already_have_landed": the work may have been landed by other means.
type AnalyticsReliabilityNeedsLandingDTO struct {
	Runs   int64  `json:"runs"`
	Caveat string `json:"caveat"`
}

// AnalyticsReliabilityDTO is the Reliability response: the calendar envelope plus every panel.
// Applicable is false, with Reason "supporting_kinds_only", when the kind filter selects no
// delivery kind: every count is then zero and every rate null with reason "not_applicable",
// which is not a zero failure rate.
type AnalyticsReliabilityDTO struct {
	AnalyticsEnvelopeDTO
	// SelectedUser matches the options response: "all" for an admin response spanning every
	// user, else the user id (the caller on the owner route).
	SelectedUser string                        `json:"selected_user"`
	Applicable   bool                          `json:"applicable"`
	Reason       *string                       `json:"reason"`
	Totals       AnalyticsReliabilityTotalsDTO `json:"totals"`
	// Series is the outcome count per bucket: series[i] is the cell of buckets[i].
	Series []AnalyticsReliabilityCellDTO `json:"series"`
	Causes AnalyticsReliabilityCausesDTO `json:"causes"`
	// RollingFailureRate points are aligned with the envelope's rolling ranges.
	RollingFailureRate AnalyticsReliabilityRollingDTO   `json:"rolling_failure_rate"`
	Breakdown          AnalyticsReliabilityBreakdownDTO `json:"breakdown"`
	// LatestFailure is null when the scope has no failure (never a zero timestamp).
	LatestFailure *AnalyticsReliabilityLatestFailureDTO `json:"latest_failure"`
	NeedsLanding  AnalyticsReliabilityNeedsLandingDTO   `json:"needs_landing"`
}

// AnalyticsSpendDTO is the Spend response: the calendar envelope plus every panel. Chat runs
// are untracked: they appear only in UntrackedChatRuns and in no cost, token or ratio figure.
type AnalyticsSpendDTO struct {
	AnalyticsEnvelopeDTO
	// SelectedUser matches the options response: "all" for an admin response spanning every
	// user, else the user id (the caller on the owner route).
	SelectedUser      string                  `json:"selected_user"`
	UntrackedChatRuns int64                   `json:"untracked_chat_runs"`
	Totals            AnalyticsSpendTotalsDTO `json:"totals"`
	// Series is the ungrouped spend per bucket: series[i] is the cell of buckets[i].
	Series              []AnalyticsSpendCellDTO `json:"series"`
	Split               AnalyticsSplitDTO       `json:"split"`
	CostPerCompletedRun AnalyticsRatioPanelDTO  `json:"cost_per_completed_run"`
	CostPerMergedPR     AnalyticsRatioPanelDTO  `json:"cost_per_merged_pr"`
	CacheRead           AnalyticsCacheReadDTO   `json:"cache_read"`
	TopRuns             []AnalyticsTopRunDTO    `json:"top_runs"`
	Breakdown           AnalyticsBreakdownDTO   `json:"breakdown"`
}
