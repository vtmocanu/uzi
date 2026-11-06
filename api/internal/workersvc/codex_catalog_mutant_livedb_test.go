//go:build catalogmutant

package workersvc

import "testing"

// TestCodexCatalogRetentionLockFreeMutantLiveDB is the mutation proof of the per-user catalog
// lock (issue #3251). It is compiled only with -tags catalogmutant and is meant to be run in a
// disposable scratch export of the candidate from which the q.LockCodexCatalogDefaults call in
// writeCodexCatalogRow has been removed:
//
//	go test -tags catalogmutant -run 'CodexCatalogRetention(LockFreeMutant|LockSerializesWriters)LiveDB$' ./internal/workersvc/
//
// Without the lock every writer reaches the post-upsert hook before any prune runs, so the
// lock-free protocol completes and the table ends with more than 32 rows (this test passes), while
// TestCodexCatalogRetentionLockSerializesWritersLiveDB goes red (it waits for 63 writers blocked on
// the advisory lock, a barrier that is never reached). On the unmutated candidate this test goes
// red instead: the first writer holds the lock at the hook, so the "all writers reached the
// post-upsert hook" barrier times out, naming itself.
func TestCodexCatalogRetentionLockFreeMutantLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	const n = 64
	rows, _, _ := runCatalogRetentionBarriers(t, e, n, retentionLockFree)
	if rows <= 32 {
		t.Fatalf("rows = %d with %d lock-free writers, want more than 32 (the bound depends on the lock)", rows, n)
	}
}
