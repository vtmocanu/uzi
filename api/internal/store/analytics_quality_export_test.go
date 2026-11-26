package store

// The sqlc-generated Quality statement texts (issue #4730) the drift test compares, so it
// measures exactly the SQL the app sends. Compiled only into this package's test binary.
var (
	AnalyticsOwnerQualitySQL = analyticsOwnerQuality
	AnalyticsAllQualitySQL   = analyticsAllQuality
)
