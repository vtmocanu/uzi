package capability

import (
	"slices"
	"testing"
)

func TestScopedClaimVocabulary(t *testing.T) {
	got := FilterProtocol([]string{"pending_outcome_scoped_claim_v1", ProviderPolicyHoldV1, "pending_outcome_scoped_claim_v1", "unknown"})
	if !slices.Equal(got, []string{ProviderPolicyHoldV1, "pending_outcome_scoped_claim_v1"}) {
		t.Fatalf("protocol order/filter: %v", got)
	}
	if slices.Contains(Vocabulary(), "pending_outcome_scoped_claim_v1") {
		t.Fatal("protocol became scheduler capability")
	}
}
