package pushbroker

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"
)

// Test-only hooks for the external pushbroker_test package, whose git http-backend
// harness is the only place a real receive-pack is reachable.

// CreateRefWithPack is CreateRef with the pack under the test's control, so a test
// can prove the empty pack is what makes a real receive-pack accept the create.
var CreateRefWithPack = createRefWithPack

// EmptyPack is the zero-object pack CreateRef sends.
var EmptyPack = emptyPack

// PushCreateSkippingList sends CreateRef's wire command (Old = zero) WITHOUT the
// list-time checks, so a test can reach the remote's own compare-and-swap and
// connectivity refusals.
func PushCreateSkippingList(ctx context.Context, cloneURL, ref, tip string, pack []byte) error {
	remote, err := newOriginRemote(cloneURL)
	if err != nil {
		return err
	}
	return pushCreate(ctx, remote, nil, plumbing.ReferenceName(ref), plumbing.NewHash(tip), pack)
}
