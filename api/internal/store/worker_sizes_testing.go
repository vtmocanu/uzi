package store

import "testing"

// SetWorkerSizeStepHookForTest installs a hook called synchronously after a worker-size row
// lock is taken, with the locking transaction's DBTX so a test can read that backend's pid
// and hold the transaction at that point. The caller must own the hook for the whole test
// (tests that install one must not run in parallel). It is removed by tb's cleanup.
func SetWorkerSizeStepHookForTest(tb testing.TB, fn func(stage string, names []string, db DBTX)) {
	if !testing.Testing() || tb == nil || fn == nil {
		panic("worker size step hook requires a test and a hook")
	}
	tb.Helper()
	f := workerSizeStepFunc(fn)
	workerSizeStepHook.Store(&f)
	tb.Cleanup(func() { workerSizeStepHook.Store(nil) })
}
