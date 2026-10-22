package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A parked gate re-presented without semantic_park records the server-expected reason for the
// same candidate: never the stale stored one, never an erased one, and never one on a gate that
// did not park.
func TestSemanticParkOmittedOnParkedGateRecordsExpected(t *testing.T) {
	parkedRun := func(t *testing.T, status, digest, park string) store.Run {
		t.Helper()
		raw, err := json.Marshal(gatePresentedPayload{Semantic: &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: digest}, SemanticPark: park}})
		if err != nil {
			t.Fatal(err)
		}
		return store.Run{Status: status, GatePresentedPayload: raw}
	}
	maintainers := []apitypes.SemanticMaintainerApproval{{ID: "c1"}}
	for _, tc := range []struct {
		name, status, storedDigest, stored, outcome string
		maintainers                                 []apitypes.SemanticMaintainerApproval
		want                                        string
	}{
		{"unchanged projection keeps reason", "awaiting_approval", "d", semanticParkCoverageIncomplete, "incomplete", nil, semanticParkCoverageIncomplete},
		{"moved projection records successor", "awaiting_approval", "d", semanticParkCoverageIncomplete, "uncertain", nil, semanticParkCoverageUncertain},
		// A reclaimed run re-presents from running, where loadSemanticGate's carry cannot fire.
		{"reclaim to unavailable records unavailable", "running", "d", semanticParkCoverageIncomplete, "unavailable", nil, semanticParkCoverageUnavailable},
		{"maintainer park stays", "awaiting_approval", "d", semanticParkMaintainerAuth, "complete", maintainers, semanticParkMaintainerAuth},
		{"complete projection needs none", "awaiting_approval", "d", semanticParkCoverageIncomplete, "complete", nil, ""},
		{"other candidate carries nothing", "awaiting_approval", "other", semanticParkCoverageIncomplete, "incomplete", nil, ""},
		{"never parked carries nothing", "awaiting_approval", "d", "", "incomplete", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: "d"}, Coverage: apitypes.SemanticCoverageStatus{Outcome: tc.outcome}, Maintainers: tc.maintainers}
			if err := validateSemanticPark(parkedRun(t, tc.status, tc.storedDigest, tc.stored), StateRequest{}, p); err != nil {
				t.Fatalf("err=%v", err)
			}
			if p.SemanticPark != tc.want {
				t.Fatalf("semantic_park=%q want %q", p.SemanticPark, tc.want)
			}
		})
	}
}

// An explicit replay of the stored reason is refused once the projection no longer expects it.
func TestSemanticParkStaleReplayRefused(t *testing.T) {
	raw, err := json.Marshal(gatePresentedPayload{Semantic: &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: "d"}, SemanticPark: semanticParkCoverageIncomplete}})
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{Status: "awaiting_approval", GatePresentedPayload: raw}
	p := &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: "d"}, Coverage: apitypes.SemanticCoverageStatus{Outcome: "uncertain"}}
	stale := semanticParkCoverageIncomplete
	if err := validateSemanticPark(run, StateRequest{SemanticPark: &stale}, p); !errors.Is(err, ErrSemanticGateMismatch) {
		t.Fatalf("stale replay: err=%v park=%q", err, p.SemanticPark)
	}
}
