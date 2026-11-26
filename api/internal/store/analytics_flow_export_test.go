package store

// The sqlc-generated Flow statement texts (issue #4729) the drift test compares and the
// lead's generic-plan checks PREPARE verbatim. Compiled only into this package's test binary.
var (
	AnalyticsOwnerFlowSQL = analyticsOwnerFlow
	AnalyticsAllFlowSQL   = analyticsAllFlow
)
