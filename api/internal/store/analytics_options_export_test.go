package store

// The sqlc-generated repo-options statement texts (issue #4780) the plan-evidence test measures,
// so it times exactly the SQL the app sends. Compiled only into this package's test binary.
var (
	AnalyticsOwnerRepoOptionsSQL = analyticsOwnerRepoOptions
	AnalyticsAllRepoOptionsSQL   = analyticsAllRepoOptions
)
