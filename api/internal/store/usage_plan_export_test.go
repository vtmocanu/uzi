package store

// The sqlc-generated query texts the generic-plan regression test (issue #1620,
// run_usage_generic_plan_livedb_test.go) PREPAREs verbatim, so it measures exactly the
// SQL the app sends rather than a hand-copied lookalike. Compiled only into this
// package's test binary.
var (
	ListRunsForUserSQL           = listRunsForUser
	ListRunUsageTotalsForRunsSQL = listRunUsageTotalsForRuns
	SelfUsageSQL                 = selfUsage
	GetJudgeRunUsageForTargetSQL = getJudgeRunUsageForTarget
	GetRunUsageTotalSQL          = getRunUsageTotal
)
