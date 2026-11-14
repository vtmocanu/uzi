package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		{
			OpID: "55555555-5555-5555-5555-555555555555", Transition: TransitionObserved,
			DeploymentUID: "77777777-7777-7777-7777-777777777777", ResourceVersion: "12340",
			PodUID: "66666666-6666-6666-6666-666666666666",
			PVCs: []HibernationPVC{
				{Role: PVCRoleData, Name: "uzi-hw-data", UID: "88888888-8888-8888-8888-888888888881", StorageClass: "fast", ClassReclaimPolicy: ReclaimPolicyDelete},
				{Role: PVCRoleNix, Name: "uzi-hw-nix", UID: "88888888-8888-8888-8888-888888888882", ClassReclaimPolicy: ReclaimPolicyUnknown},
				{Role: PVCRoleDindData, Name: "uzi-hw-dind", UID: "88888888-8888-8888-8888-888888888883", StorageClass: "slow", ClassReclaimPolicy: ReclaimPolicyRetain},
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
}

func ptr[T any](v T) *T { return &v }

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
		{TransitionObserved, "observed"},
		{TransitionHeld, "held"},
		{InventoryGateObserve, "observe"},
		{InventoryGateAwait, "await"},
		{InventoryGatePermit, "permit"},
	} {
		if tc.got != tc.want {
			t.Errorf("transition literal = %q, want %q", tc.got, tc.want)
		}
	}
}

// A known-empty set and an unobserved one are different facts: a nil pointer is omitted, a
// pointer to an empty slice encodes as [].
func TestHibernationTransitionPodUIDsPresence(t *testing.T) {
	absent, err := json.Marshal(HibernationTransition{OpID: "o", Transition: TransitionScaledDown})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), "pod_uids") || strings.Contains(string(absent), "pvc_user_pods") {
		t.Errorf("nil pointer slices must be omitted: %s", absent)
	}
	empty, err := json.Marshal(HibernationTransition{OpID: "o", Transition: TransitionScaledDown, PodUIDs: &[]string{}, PVCUserPods: &[]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"pod_uids":[]`) || !strings.Contains(string(empty), `"pvc_user_pods":[]`) {
		t.Errorf("pointer to an empty slice must encode []: %s", empty)
	}
}

func TestHibernationProtocolVersion(t *testing.T) {
	if HibernationProtocolVersion != 2 {
		t.Errorf("HibernationProtocolVersion = %d, want 2 (PRD #4208 M1 inventory)", HibernationProtocolVersion)
	}
}
