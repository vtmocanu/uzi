package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SetBudgetHookForTest replaces the remaining-budget read of the folder's fullBudget; site is
// one of "retry", "fold", "foldEach", "kick", "missingQuery". Like SetWaitHookForTest it must
// be called before any Notify or Pass on f. The hook is consulted only when the ctx has a
// deadline; a ctx without one always counts as having the full budget.
func SetBudgetHookForTest(f *UsageFolder, fn func(ctx context.Context, site string) time.Duration) {
	f.budgetHook = func(ctx context.Context, site budgetSite) time.Duration { return fn(ctx, string(site)) }
}

// SetWaitHookForTest installs a hook called when a materializer call finds the folder-wide
// semaphore busy, before it blocks. Must be called before any Notify or Pass on f.
func SetWaitHookForTest(f *UsageFolder, fn func(ids []uuid.UUID)) { f.waitHook = fn }

// SkippedForTest returns the unexpired TTL-skipped run ids.
func (f *UsageFolder) SkippedForTest() []uuid.UUID { return f.skipList() }

// RetryMissingForTest returns a copy of the missing lane's retry queue.
func (f *UsageFolder) RetryMissingForTest() []uuid.UUID {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	return append([]uuid.UUID(nil), f.retryMissing...)
}

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
