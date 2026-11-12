package hostedsvc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The CONSUMER half of the hibernation transition wire contract (PRD #4205). The body
// flows controller to api, so the golden lives under controller/ and the controller's
// test marshals it; this test parses it with DisallowUnknownFields and checks each
// field by name, so drift in either module reddens a build gate. Reads a file outside
// the api module: run with -count=1.
const hibernationTransitionGolden = "../../../controller/internal/protocol/testdata/controller_hibernation_transition_wire.json"

func TestHibernationTransitionConsumerContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(hibernationTransitionGolden))
	if err != nil {
		t.Fatalf("read the controller's golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/protocol/... in controller/): %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var got []HibernationTransition
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the api cannot decode the controller's own golden: %v", err)
	}
	want := []HibernationTransition{
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScaledDown,
			DeploymentUID: "deployment-uid", ResourceVersion: "12345",
			PodUID: "66666666-6666-6666-6666-666666666666", Reason: "pods absent",
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScalingDown,
			DeploymentUID: "deployment-uid",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d bodies, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("body %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Re-marshal and compare bytes: the api must emit the same shape it accepts.
	again, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(again)+"\n" != string(raw) {
		t.Errorf("api re-marshal differs from the controller's golden.\n got:\n%s\nwant:\n%s", again, raw)
	}
	for tr, lit := range map[string]string{TransitionRefused: "refused", TransitionScalingDown: "scaling_down", TransitionScaledDown: "scaled_down", TransitionReady: "ready"} {
		if tr != lit {
			t.Errorf("transition literal = %q, want %q", tr, lit)
		}
	}
}
