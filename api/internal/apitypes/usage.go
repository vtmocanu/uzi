package apitypes

import "time"

// UsageDTO is a bundle of token/cost totals (PRD #40). It serves two roles: a
// single run's rolled-up totals (attached to a run list item / detail, absent when
// the run has no usage) and a windowed total over a set of runs (lifetime /
// last-7-days on the usage summaries). All five figures come straight from the
// run_usage_totals rollup (greatest-wins per model, summed across models — the
// verdict-(b) rule lives in the DB view, not here).
type UsageDTO struct {
	InputTokens         int64   `json:"input_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	// CostStatus is the run's folded per-run cost-observability marker (PRD #1429 M1 / D7),
	// one of "metered" | "subscription" | "unreported" — so a reader keys on the status
	// rather than reading a placeholder cost_usd 0 as a real dollar total: metered may show
	// dollars, subscription is labelled subscription usage, unreported shows tokens with cost
	// unavailable. It is meaningful on a PER-RUN bundle (RunDTO.Usage, run-list rows), copied
	// from run_usage_totals; on a per-WINDOW aggregate (SelfUsageDTO.Lifetime/Last7Days) a
	// single status does not apply, so it is left "" and the window's truth is the
	// subscription/unreported RUN COUNTS on the containers below. A client must render an
	// unrecognised value honestly — the API is deployed separately and a newer server can add
	// a status this client has not heard of.
	CostStatus string `json:"cost_status"`
}

// RunOutcomesDTO is the failed-run rate aggregate for one scope+window (PRD #1293).
// Counted over `runs` directly, NOT the usage rollup (D1): a run that fails before
// spending a token has no usage row but must still count. All five counts are always
// present (zeros, never null). Invariants, asserted in tests:
//   - finished == completed + cancelled + plan_rejected + failed
//   - sum(fail_origins) == failed
//
// plan_rejected is status=failed AND fail_origin='plan_rejected'; failed is every other
// status=failed row (fail_origin IS DISTINCT FROM 'plan_rejected') (D4). The client
// computes the rate (failed/finished), never the server (D6). FailOrigins keys are the
// failorigin.go vocabulary plus 'unknown' (a NULL fail_origin, pre-migration 00126); it
// is always a non-nil map so it marshals as {} rather than null, even when empty.
type RunOutcomesDTO struct {
	Finished     int64 `json:"finished"`
	Completed    int64 `json:"completed"`
	Cancelled    int64 `json:"cancelled"`
	PlanRejected int64 `json:"plan_rejected"`
	Failed       int64 `json:"failed"`
	// FailOrigins counts the `failed` rows by fail_origin (NULL keyed "unknown"). Always
	// non-nil so it marshals {} not null; sum over the map equals Failed.
	FailOrigins map[string]int64 `json:"fail_origins"`
}

// RunOutcomeWindowsDTO carries the two windows the usage cards show side by side
// (PRD #1293): lifetime and last-7-days, windowed on runs.created_at (D3) so the 7-day
// failure figure describes the same run set as the 7-day usage figure on the same card.
type RunOutcomeWindowsDTO struct {
	Lifetime  RunOutcomesDTO `json:"lifetime"`
	Last7Days RunOutcomesDTO `json:"last_7_days"`
}

// SelfUsageDTO is the current user's own consumption (GET /api/usage) or, reused,
// the factory-wide totals on the admin summary. run_count is the number of the
// scope's runs that carry usage; the client reads run_count == 0 as "nothing yet"
// rather than rendering fabricated zeros.
type SelfUsageDTO struct {
	Lifetime  UsageDTO `json:"lifetime"`
	Last7Days UsageDTO `json:"last_7_days"`
	RunCount  int64    `json:"run_count"`
	// The subscription/unreported RUN COUNTS for each window (PRD #1429 M1 / D7). A mixed
	// aggregate never presents its numeric metered subset as the complete total: these say
	// how many of the window's runs carried a non-metered cost, so a reader discloses that a
	// subscription/unreported component makes the dollar sum incomplete. Lifetime AND
	// last-seven-day so both the summary's totals can be read honestly. Copied from
	// SelfUsage/AdminUsageTotals, which already compute them per window.
	LifetimeSubscriptionRunCount int64 `json:"lifetime_subscription_run_count"`
	LifetimeUnreportedRunCount   int64 `json:"lifetime_unreported_run_count"`
	Last7SubscriptionRunCount    int64 `json:"last7_subscription_run_count"`
	Last7UnreportedRunCount      int64 `json:"last7_unreported_run_count"`
	// Outcomes is the failed-run rate aggregate for this scope, both windows (PRD #1293).
	// The factory card reuses this type, so it gets the block for free.
	Outcomes RunOutcomeWindowsDTO `json:"outcomes"`
}

// AdminUserUsageDTO is one user's lifetime consumption row on the admin factory
// breakdown; the client draws each user's share against the factory total.
type AdminUserUsageDTO struct {
	UserID   string   `json:"user_id"`
	Email    string   `json:"email"`
	Usage    UsageDTO `json:"usage"`
	RunCount int64    `json:"run_count"`
	// The user's LIFETIME subscription/unreported run counts (PRD #1429 M1 / D7), so the admin
	// per-user breakdown discloses a non-metered component the same way the factory total does.
	// Lifetime-only here (the per-user row is a lifetime breakdown; the windowed counts live on
	// the factory SelfUsageDTO). Copied from AdminUsagePerUser, which computes them per user.
	SubscriptionRunCount int64 `json:"subscription_run_count"`
	UnreportedRunCount   int64 `json:"unreported_run_count"`
	// Outcomes is this user's LIFETIME failed-run rate aggregate (PRD #1293), matching the
	// row's lifetime usage. A user present in the outcomes aggregate but absent from usage
	// (every run died before spending, D5) still gets a row, with zero usage.
	Outcomes RunOutcomesDTO `json:"outcomes"`
}

// AdminUsageDTO is the admin factory view: the factory-wide totals plus the
// per-user breakdown. By construction the per-user rows sum to factory.lifetime.
// The subscription/unreported run counts (PRD #1429 M1 / D7) ride here through its two
// members: the windowed factory counts on Factory (a SelfUsageDTO) and the lifetime per-user
// counts on each Users row (AdminUserUsageDTO), so the admin dashboard shows the counts beside
// numeric metered totals and never presents a partial dollar sum as complete.
type AdminUsageDTO struct {
	Factory SelfUsageDTO        `json:"factory"`
	Users   []AdminUserUsageDTO `json:"users"`
	// EarliestRun is the factory's first usage-bearing run's timestamp, for the
	// card's "since <date>" line; null when the factory has no usage yet (PRD #40).
	EarliestRun *time.Time `json:"earliest_run"`
}
