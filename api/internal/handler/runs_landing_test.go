package handler

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestLandingStateOverlay pins the single-run GetRun overlay's degradation policy (issue #1418):
// a capture-lookup error must NEVER leave the capture-unaware seed's definitive "unrecoverable"
// on a human-landable run — a transient DB blip would otherwise tell an operator their
// recoverable work is lost. The regression is the no-preserved-patch origin (push_secret_blocked),
// whose seed is "unrecoverable" and which reaches needs_landing ONLY via an available capture.
func TestLandingStateOverlay(t *testing.T) {
	secretBlocked := "push_secret_blocked"      // human-landable, NO preserved_patch
	baseAlign := "finalize_base_align_conflict" // human-landable, carries a preserved_patch
	notLandable := "agent_failure"              // not in the human-landable set
	lookupErr := errors.New("db blip")

	for _, tc := range []struct {
		name           string
		failOrigin     string
		preservedPatch bool
		hasCapture     bool
		lookupErr      error
		want           string
	}{
		// THE BUG: a secret-blocked run with a real available capture, hit by a lookup error,
		// must degrade to needs_landing (actionable), never the false "unrecoverable".
		{"no-patch origin, lookup errored → needs_landing not unrecoverable", secretBlocked, false, false, lookupErr, workersvc.LandingStateNeedsLanding},
		// Successful lookups still derive the true state on both sides.
		{"no-patch origin, capture present → needs_landing", secretBlocked, false, true, nil, workersvc.LandingStateNeedsLanding},
		{"no-patch origin, no capture → unrecoverable", secretBlocked, false, false, nil, workersvc.LandingStateUnrecoverable},
		// A preserved_patch alone reaches needs_landing; an error is harmless there too.
		{"patch origin, lookup errored → needs_landing", baseAlign, true, false, lookupErr, workersvc.LandingStateNeedsLanding},
		{"patch origin, clean lookup → needs_landing", baseAlign, true, false, nil, workersvc.LandingStateNeedsLanding},
		// A non-landable origin is always none, error or not.
		{"non-landable origin → none", notLandable, false, false, lookupErr, workersvc.LandingStateNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fo := tc.failOrigin
			got := landingStateOverlay(&fo, tc.preservedPatch, tc.hasCapture, tc.lookupErr)
			if got != tc.want {
				t.Errorf("landingStateOverlay(%q, patch=%v, cap=%v, err=%v) = %q, want %q",
					tc.failOrigin, tc.preservedPatch, tc.hasCapture, tc.lookupErr != nil, got, tc.want)
			}
		})
	}
}
