package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
)

// TestClassifyDiscoveryFailure pins the terminal/transient boundary of a DiscoverIdentity
// error (issue #1209 review) with no DB. Only a PROVEN unusable credential is terminal — a
// 401/403 auth rejection (AuthError.Unauthorized() covers both), or a 2xx whose identity was
// incomplete. A transport error, a 429, and a 5xx are transient: classify returns terminal
// false so the reconciler leaves the alias 'staging' for the poller to re-probe. A terminal
// failure carries a non-empty last_error reason; a transient one carries none (the reason is
// written only on the terminal path). Wrapped sentinels must classify the same as bare ones.
func TestClassifyDiscoveryFailure(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		terminal bool
	}{
		{"incomplete_identity", codexauth.ErrIdentityIncomplete, true},
		{"incomplete_wrapped", fmt.Errorf("discover: %w", codexauth.ErrIdentityIncomplete), true},
		{"account_mismatch", codexauth.ErrIdentityAccountMismatch, true},
		{"account_mismatch_wrapped", fmt.Errorf("discover: %w", codexauth.ErrIdentityAccountMismatch), true},
		{"unauthorized_401", &codexauth.AuthError{Op: "discover_identity", StatusCode: 401}, true},
		{"forbidden_403", &codexauth.AuthError{Op: "discover_identity", StatusCode: 403}, true},
		{"unauthorized_wrapped", fmt.Errorf("discover: %w", &codexauth.AuthError{Op: "discover_identity", StatusCode: 401}), true},
		{"rate_limited_429", &codexauth.AuthError{Op: "discover_identity", StatusCode: 429}, false},
		{"server_5xx_503", &codexauth.AuthError{Op: "discover_identity", StatusCode: 503}, false},
		{"transport", errors.New("codexauth: identity request: connection refused"), false},
		{"deadline_exceeded", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, terminal := classifyDiscoveryFailure(tc.err)
			if terminal != tc.terminal {
				t.Fatalf("terminal = %v, want %v", terminal, tc.terminal)
			}
			if terminal && reason == "" {
				t.Fatal("a terminal failure must carry a non-empty last_error reason")
			}
			if !terminal && reason != "" {
				t.Fatalf("a transient failure must carry no reason, got %q", reason)
			}
		})
	}
}

// #1239: an account mismatch is still an incomplete identity (same terminal routing), but it
// must report its own reason rather than the misleading "missing user or account id".
func TestClassifyDiscoveryFailureAccountMismatchReason(t *testing.T) {
	mismatch, _ := classifyDiscoveryFailure(codexauth.ErrIdentityAccountMismatch)
	incomplete, _ := classifyDiscoveryFailure(codexauth.ErrIdentityIncomplete)
	if mismatch == incomplete {
		t.Fatalf("account mismatch reason = %q, want a reason distinct from the incomplete-identity one", mismatch)
	}
}
