package store

// The sqlc-generated Spend statement texts (issue #4684) the generic-plan test PREPAREs
// verbatim and the drift test compares, so both measure exactly the SQL the app sends.
// Compiled only into this package's test binary.
var (
	AnalyticsOwnerSpendSQL = analyticsOwnerSpend
	AnalyticsAllSpendSQL   = analyticsAllSpend
)
