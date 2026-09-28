package pushbroker

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"
)

// Test-only access for the external pushbroker_test package (PRD #1867 M1). Compiled
// into test binaries only.

// EmptyPackForTest returns a copy of the zero-object pack the salvage create ships.
func EmptyPackForTest() []byte { return append([]byte(nil), emptyPack...) }

// ForwardPackForTest runs forwardPack against cloneURL with an explicit pack (nil for
// none), so a test can compare the salvage create with and without emptyPack against
// the real git-receive-pack.
func ForwardPackForTest(ctx context.Context, cloneURL, ref string, oldTip, newTip string, pack []byte) error {
	remote, err := newOriginRemote(cloneURL)
	if err != nil {
		return err
	}
	old := plumbing.ZeroHash
	if oldTip != "" {
		old = plumbing.NewHash(oldTip)
	}
	return forwardPack(ctx, remote, nil, plumbing.ReferenceName(ref), old, plumbing.NewHash(newTip), pack)
}
