package capability

import (
	"reflect"
	"testing"
)

func TestPolicyHoldProtocolVocabulary(t *testing.T) {
	want := []string{CredentialSwitchV1, MilestoneReconciliationClaimV1, ProviderPolicyHoldV1, WallParkV1}
	got := FilterProtocol([]string{WallParkV1, ProviderPolicyHoldV1, MilestoneReconciliationClaimV1, CredentialSwitchV1, ProviderPolicyHoldV1, "provider_policy_hold_v2"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("protocol=%v want=%v", got, want)
	}
	if len(Filter([]string{ProviderPolicyHoldV1})) != 0 {
		t.Fatal("policy protocol entered scheduling vocabulary")
	}
}
