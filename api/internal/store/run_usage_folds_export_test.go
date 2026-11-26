package store

import "context"

// Test-only seams for the run_usage fold tests (issue #4700): the generated statement texts
// the plan-property test EXPLAINs verbatim, and the materializer's step hook.
var (
	DeleteRunUsageSQL           = deleteRunUsage
	InsertRunUsageModelFoldsSQL = insertRunUsageModelFolds
)

// SetRunUsageFoldStepHook installs a hook the materializer calls after each step and returns
// a restore function. Tests using it must not run in parallel with each other.
func SetRunUsageFoldStepHook(h func(ctx context.Context, step string)) (restore func()) {
	prev := runUsageFoldStepHook
	runUsageFoldStepHook = h
	return func() { runUsageFoldStepHook = prev }
}
