package workersvc

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The agent reads the same file (fixtures/claims/policy-preserve-claim.json), so the two sides
// cannot drift on the claim keys that carry the preserving constraint.
func TestPolicyPreserveClaimSharedWire(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/policy-preserve-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	head, _ := want["policy_expected_head"].(string)
	if len(want) != 2 || head == "" || want["policy_preserve_only"] != true {
		t.Fatalf("fixture must carry exactly policy_preserve_only=true and a head: %s", raw)
	}
	run := store.Run{PolicyPreserveRequired: true, PolicyRequiredHead: pgconv.Text(head)}
	b, err := json.Marshal(ClaimPayload{PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	picked := map[string]any{}
	for k := range want {
		picked[k] = got[k]
	}
	if !reflect.DeepEqual(picked, want) {
		t.Fatalf("claim keys %v, fixture %v", picked, want)
	}
}

func TestPolicyExpectedHeadOnlyWhilePreserving(t *testing.T) {
	head := pgconv.Text("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	for name, run := range map[string]store.Run{
		"no constraint":           {PolicyRequiredHead: head},
		"constraint without head": {PolicyPreserveRequired: true},
	} {
		if got := policyExpectedHead(run); got != nil {
			t.Fatalf("%s: expected no head, got %q", name, *got)
		}
		b, err := json.Marshal(ClaimPayload{PolicyPreserveOnly: run.PolicyPreserveRequired, PolicyExpectedHead: policyExpectedHead(run)})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["policy_expected_head"]; ok {
			t.Fatalf("%s: head key present", name)
		}
		if _, ok := m["policy_preserve_only"]; ok != run.PolicyPreserveRequired {
			t.Fatalf("%s: preserve key presence %v", name, ok)
		}
	}
}
