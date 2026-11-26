package store

// The sqlc-generated Reliability and latest-failure statement texts (issue #4685) the
// generic-plan test PREPAREs verbatim and the drift tests compare, so both measure exactly the
// SQL the app sends. Compiled only into this package's test binary.
var (
	AnalyticsOwnerReliabilitySQL   = analyticsOwnerReliability
	AnalyticsAllReliabilitySQL     = analyticsAllReliability
	AnalyticsOwnerLatestFailureSQL = analyticsOwnerLatestFailure
	AnalyticsAllLatestFailureSQL   = analyticsAllLatestFailure
)
