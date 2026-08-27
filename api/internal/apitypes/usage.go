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
}

// SelfUsageDTO is the current user's own consumption (GET /api/usage) or, reused,
// the factory-wide totals on the admin summary. run_count is the number of the
// scope's runs that carry usage; the client reads run_count == 0 as "nothing yet"
// rather than rendering fabricated zeros.
type SelfUsageDTO struct {
	Lifetime  UsageDTO `json:"lifetime"`
	Last7Days UsageDTO `json:"last_7_days"`
	RunCount  int64    `json:"run_count"`
}

// AdminUserUsageDTO is one user's lifetime consumption row on the admin factory
// breakdown; the client draws each user's share against the factory total.
type AdminUserUsageDTO struct {
	UserID   string   `json:"user_id"`
	Email    string   `json:"email"`
	Usage    UsageDTO `json:"usage"`
	RunCount int64    `json:"run_count"`
}

// AdminUsageDTO is the admin factory view: the factory-wide totals plus the
// per-user breakdown. By construction the per-user rows sum to factory.lifetime.
type AdminUsageDTO struct {
	Factory SelfUsageDTO        `json:"factory"`
	Users   []AdminUserUsageDTO `json:"users"`
	// EarliestRun is the factory's first usage-bearing run's timestamp, for the
	// card's "since <date>" line; null when the factory has no usage yet (PRD #40).
	EarliestRun *time.Time `json:"earliest_run"`
}
