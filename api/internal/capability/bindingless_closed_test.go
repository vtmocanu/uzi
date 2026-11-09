package capability

import (
	"slices"
	"testing"
)

func TestRecoveryRunOpenHoldsProtocol(t *testing.T) {
	const name = "recovery_run_open_holds_v1"
	if RecoveryRunOpenHoldsV1 != name {
		t.Fatal(RecoveryRunOpenHoldsV1)
	}
	got := FilterProtocol([]string{CodexHarnessV1, name, name, RecoveryBindinglessClosedV1, "unknown"})
	want := []string{RecoveryBindinglessClosedV1, name, CodexHarnessV1}
	if !slices.Equal(got, want) {
		t.Fatalf("protocol order/allowlist/dedup: %v", got)
	}
	if len(Filter([]string{name})) != 0 || len(SelfReportable([]string{name})) != 0 || slices.Contains(Vocabulary(), name) {
		t.Fatal("read protocol entered scheduler vocabulary")
	}
	// protocolVocabulary and protocolOrder stay in lockstep for this name.
	if _, ok := protocolVocabulary[name]; !ok || !slices.Contains(protocolOrder, name) {
		t.Fatal("token missing from vocabulary or order")
	}
}

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
