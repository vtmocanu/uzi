package hostedsvc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionObserved,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12340",
			PodUID: "66666666-6666-6666-6666-666666666666",
			PVCs: []HibernationPVC{
				{Role: "data", Name: "uzi-hw-data", UID: "88888888-8888-8888-8888-888888888881", StorageClass: "fast", ClassReclaimPolicy: "Delete"},
				{Role: "nix", Name: "uzi-hw-nix", UID: "88888888-8888-8888-8888-888888888882", ClassReclaimPolicy: "unknown"},
				{Role: "dind_data", Name: "uzi-hw-dind", UID: "88888888-8888-8888-8888-888888888883", StorageClass: "slow", ClassReclaimPolicy: "Retain"},
			},
			PodUIDs:              &[]string{"66666666-6666-6666-6666-666666666666"},
			PVCUserPods:          &[]string{},
			DeploymentGeneration: ptr(int64(7)),
			DeploymentReplicas:   ptr(int32(1)),
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScalingDown,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12347",
			PodUID: "66666666-6666-6666-6666-666666666666", FenceGeneration: ptr(int64(9)),
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionScaledDown,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12349",
			PodUID: "66666666-6666-6666-6666-666666666666", PodUIDs: &[]string{},
		},
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionHeld,
			DeploymentUID: "77777777-7777-7777-7777-777777777777",
			PodUID:        "66666666-6666-6666-6666-666666666666",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d bodies, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
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
	for tr, lit := range map[string]string{TransitionRefused: "refused", TransitionScalingDown: "scaling_down", TransitionScaledDown: "scaled_down", TransitionReady: "ready", TransitionObserved: "observed", TransitionHeld: "held"} {
		if tr != lit {
			t.Errorf("transition literal = %q, want %q", tr, lit)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The pointer-slice presence contract on the api side: a nil PodUIDs is omitted, a pointer to
// an empty slice encodes as [], and "pod_uids":[] decodes back to a non-nil empty slice (a
// known-empty set), while an absent key decodes to nil (unproven).
func TestHibernationTransitionPodUIDsPresence(t *testing.T) {
	absent, err := json.Marshal(HibernationTransition{OpID: "o", Transition: TransitionScaledDown})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(absent, []byte("pod_uids")) {
		t.Errorf("nil PodUIDs must be omitted: %s", absent)
	}
	empty, err := json.Marshal(HibernationTransition{OpID: "o", Transition: TransitionScaledDown, PodUIDs: &[]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(empty, []byte(`"pod_uids":[]`)) {
		t.Errorf("pointer to empty slice must encode []: %s", empty)
	}
	var back HibernationTransition
	if err := json.Unmarshal(empty, &back); err != nil {
		t.Fatal(err)
	}
	if back.PodUIDs == nil || len(*back.PodUIDs) != 0 {
		t.Errorf(`"pod_uids":[] must decode to a non-nil empty slice, got %v`, back.PodUIDs)
	}
	var none HibernationTransition
	if err := json.Unmarshal(absent, &none); err != nil {
		t.Fatal(err)
	}
	if none.PodUIDs != nil {
		t.Errorf("an absent pod_uids must decode to nil, got %v", none.PodUIDs)
	}
}
