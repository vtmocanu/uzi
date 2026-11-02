package capability

import (
	"slices"
	"testing"
)

func TestPolicyHoldVocabulary(t *testing.T) {
	got := FilterProtocol([]string{ProviderPolicyHoldV1, ProviderPolicyHoldV1})
	if len(got) != 1 || got[0] != ProviderPolicyHoldV1 {
		t.Fatalf("protocol filter: %v", got)
	}
	if slices.Contains(Vocabulary(), ProviderPolicyHoldV1) {
		t.Fatal("protocol capability became scheduler override")
	}
}
