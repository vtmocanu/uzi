package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// hibernationTransitionFixture is the golden for the hibernation transition body
// (PRD #4205). The body flows controller to api, so this side is the producer and
// owns the golden, exactly like controller_status_wire.json; the api's consumer test
// (hostedsvc.TestHibernationTransitionConsumerContract) parses the same file.
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/protocol/...`.
const hibernationTransitionFixture = "testdata/controller_hibernation_transition_wire.json"

// sampleHibernationTransitions carries one body with every optional field set, one with
// none, and one valid body per transition, so an omitempty dropped or added on either side reddens.
func sampleHibernationTransitions() []HibernationTransition {
	return []HibernationTransition{
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScaledDown,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12345",
			PodUID: "66666666-6666-6666-6666-666666666666", Reason: "pods absent",
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScaledDown,
			DeploymentUID: "77777777-7777-7777-7777-777777777777",
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionRefused,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12346",
			Reason: "worker busy",
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScalingDown,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12347",
			PodUID: "66666666-6666-6666-6666-666666666666",
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionReady,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12348",
			PodUID: "66666666-6666-6666-6666-666666666666",
		},
	}
}

func TestHibernationTransitionWireContract(t *testing.T) {
	got, err := json.MarshalIndent(sampleHibernationTransitions(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(hibernationTransitionFixture), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(hibernationTransitionFixture, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}
	want, err := os.ReadFile(hibernationTransitionFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/protocol/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("hibernation transition wire shape drifted from the golden file.\n got:\n%s\nwant:\n%s", got, want)
	}
}

// The transition names are wire values the api matches on exactly.
func TestHibernationTransitionLiterals(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{TransitionRefused, "refused"},
		{TransitionScalingDown, "scaling_down"},
		{TransitionScaledDown, "scaled_down"},
		{TransitionReady, "ready"},
	} {
		if tc.got != tc.want {
			t.Errorf("transition literal = %q, want %q", tc.got, tc.want)
		}
	}
}
