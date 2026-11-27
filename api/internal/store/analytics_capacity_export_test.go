package store

// The sqlc-generated Capacity statement texts (issue #4731) the text-shape tests compare, so
// they measure exactly the SQL the app sends. Compiled only into this package's test binary.
var (
	AnalyticsOwnerCapacitySQL          = analyticsOwnerCapacity
	AnalyticsAllCapacitySQL            = analyticsAllCapacity
	AnalyticsOwnerAnthropicGaugesSQL   = analyticsOwnerAnthropicGauges
	AnalyticsAllAnthropicGaugesSQL     = analyticsAllAnthropicGauges
	AnalyticsOwnerCodexGaugesSQL       = analyticsOwnerCodexGauges
	AnalyticsAllCodexGaugesSQL         = analyticsAllCodexGauges
	ListRateLimitsForUserSQL           = listRateLimitsForUser
	GetCodexAccountRateLimitsOwnerSQL  = getCodexAccountRateLimitsForUser
	ListCodexAccountRateLimitsAdminSQL = listCodexAccountRateLimits
)
