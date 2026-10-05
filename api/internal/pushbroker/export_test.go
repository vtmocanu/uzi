package pushbroker

import (
	"context"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing"
)

// Test-only hooks for the external pushbroker_test package, whose git http-backend
// harness is the only place a real receive-pack is reachable.

// CreateRefWithPack is CreateRef with the pack under the test's control, so a test
// can prove the empty pack is what makes a real receive-pack accept the create.
var CreateRefWithPack = createRefWithPack

// BrokerHTTPClient exposes the actual broker client for serial TLS/pool fixtures.
var BrokerHTTPClient *http.Client = brokerHTTPClient

// TransportFor exposes manual session selection for HTTP wiring tests.
var TransportFor = transportFor

// EmptyPack is the zero-object pack CreateRef sends.
var EmptyPack = emptyPack

// PushCreateSkippingList sends CreateRef's wire command (Old = zero) WITHOUT the
// list-time checks, then classifies it exactly as CreateRef does (the read-back), so a
// test can reach the remote's own compare-and-swap and connectivity refusals.
func PushCreateSkippingList(ctx context.Context, cloneURL, ref, tip string, pack []byte) error {
	remote, err := newOriginRemote(cloneURL)
	if err != nil {
		return err
	}
	return pushCreateVerified(ctx, remote, nil, plumbing.ReferenceName(ref), plumbing.NewHash(tip), pack)
}
