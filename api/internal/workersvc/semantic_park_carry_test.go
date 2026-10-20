package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A parked gate re-presented without semantic_park records the server-expected reason for the
// same candidate: never the stale stored one, never an erased one, and never one on a gate that
// did not park.
func TestSemanticParkOmittedOnParkedGateRecordsExpected(t *testing.T) {
	parkedRun := func(t *testing.T, digest, park string) store.Run {
		t.Helper()
		raw, err := json.Marshal(gatePresentedPayload{Semantic: &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: digest}, SemanticPark: park}})
		if err != nil {
			t.Fatal(err)
		}
		return store.Run{Status: "awaiting_approval", GatePresentedPayload: raw}
	}
	maintainers := []apitypes.SemanticMaintainerApproval{{ID: "c1"}}
	for _, tc := range []struct {
		name, storedDigest, stored, outcome string
		maintainers                         []apitypes.SemanticMaintainerApproval
		want                                string
	}{
		{"unchanged projection keeps reason", "d", semanticParkCoverageIncomplete, "incomplete", nil, semanticParkCoverageIncomplete},
		{"moved projection records successor", "d", semanticParkCoverageIncomplete, "uncertain", nil, semanticParkCoverageUncertain},
		{"reclaim to unavailable records unavailable", "d", semanticParkCoverageIncomplete, "unavailable", nil, semanticParkCoverageUnavailable},
		{"maintainer park stays", "d", semanticParkMaintainerAuth, "complete", maintainers, semanticParkMaintainerAuth},
		{"complete projection needs none", "d", semanticParkCoverageIncomplete, "complete", nil, ""},
		{"other candidate carries nothing", "other", semanticParkCoverageIncomplete, "incomplete", nil, ""},
		{"never parked carries nothing", "d", "", "incomplete", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: "d"}, Coverage: apitypes.SemanticCoverageStatus{Outcome: tc.outcome}, Maintainers: tc.maintainers}
			if err := validateSemanticPark(parkedRun(t, tc.storedDigest, tc.stored), StateRequest{}, p); err != nil {
				t.Fatalf("err=%v", err)
			}
			if p.SemanticPark != tc.want {
				t.Fatalf("semantic_park=%q want %q", p.SemanticPark, tc.want)
			}
		})
	}
}
