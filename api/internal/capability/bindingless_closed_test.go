package capability

import (
	"slices"
	"testing"
)

func TestRecoveryBindinglessClosedProtocol(t *testing.T) {
	const token = "recovery_bindingless_closed_v1"
	if RecoveryBindinglessClosedV1 != token {
		t.Fatal(RecoveryBindinglessClosedV1)
	}
	got := FilterProtocol([]string{CodexHarnessV1, token, RecoveryPrerequisiteClosureV1, token, RecoveryHeldPublicationV2, "unknown"})
	want := []string{RecoveryHeldPublicationV2, token, RecoveryPrerequisiteClosureV1, CodexHarnessV1}
	if !slices.Equal(got, want) {
		t.Fatalf("protocol order/allowlist/dedup: %v", got)
	}
	if len(Filter([]string{token})) != 0 || len(SelfReportable([]string{token})) != 0 || slices.Contains(Vocabulary(), token) {
		t.Fatal("read protocol entered scheduler vocabulary")
	}
}
