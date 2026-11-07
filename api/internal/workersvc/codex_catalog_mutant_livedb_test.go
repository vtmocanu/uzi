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
// It asserts the retention bound under the two-barrier lock-free protocol: every writer upserts
// before any prune runs, and every prune has taken its snapshot before any commit. On the mutant
// (lock removed) the protocol completes and the bound assertion goes red with the real count
// (64 rows), which is the required red. On the unmutated candidate the first writer holds the lock
// at the hook, so the "all writers reached the post-upsert hook" barrier times out, naming itself;
// that is inconclusive, not a red, and the green side of the proof is
// TestCodexCatalogRetentionLockSerializesWritersLiveDB on the unmutated candidate.
func TestCodexCatalogRetentionLockFreeMutantLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	const n = 64
	rows, _, _ := runCatalogRetentionBarriers(t, e, n, retentionLockFree)
	if rows > 32 {
		t.Fatalf("retention bound violated: rows = %d after %d concurrent writers, want at most 32", rows, n)
	}
}
