package store

import "context"

// SetCodexRejectionAfterAccountLockForTest installs the QuarantineRejectedCodexRefresh
// test seam (run between the account lock and the intent lock) and returns a func that
// restores the previous value. Compiled only into this package's test binary.
func SetCodexRejectionAfterAccountLockForTest(f func(ctx context.Context)) (restore func()) {
	prev := codexRejectionAfterAccountLock
	codexRejectionAfterAccountLock = f
	return func() { codexRejectionAfterAccountLock = prev }
}
