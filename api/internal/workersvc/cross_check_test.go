package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestPlanCrossCheckCandidateDigestCanonicalStoredFields(t *testing.T) {
	c := PlanCrossCheckCandidate{
		PlanMd: "Plan", Milestones: json.RawMessage(`[{"z":2, "a":1}]`),
		RequiredCapabilities: []string{"b", "a", "b"}, RequiredTools: []string{"read"},
		SizeClass: "small", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlanningDiff: "diff",
	}
	got, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	const want = "15016c8daeac4667d1f2fe4bb1120b2e47faf653f78d19c6664ca90f374804e7"
	if hex.EncodeToString(got) != want {
		t.Fatalf("digest = %x, want %s", got, want)
	}
	c.Milestones = json.RawMessage(`[{"a":1,"z":2}]`)
	c.RequiredCapabilities = []string{"a", "b"}
	same, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(same) != want {
		t.Fatalf("equivalent stored candidate digest = %x", same)
	}
	c.PlanningDiff = "changed"
	changed, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(changed) == want {
		t.Fatal("changed planning diff retained approval digest")
	}
}
