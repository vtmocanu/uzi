package capability

import (
	"slices"
	"testing"
)

func TestRecoveryTerminalPreservationProtocol(t *testing.T) {
	const token = "recovery_terminal_preservation_v1"
	if RecoveryTerminalPreservationV1 != token {
		t.Fatal(RecoveryTerminalPreservationV1)
	}
	got := FilterProtocol([]string{CodexHarnessV1, token, RecoveryPrerequisiteClosureV1, token, "unknown"})
	want := []string{RecoveryPrerequisiteClosureV1, token, CodexHarnessV1}
	if !slices.Equal(got, want) {
		t.Fatalf("protocol order/allowlist/dedup: %v", got)
	}
	if len(Filter([]string{token})) != 0 || len(SelfReportable([]string{token})) != 0 || slices.Contains(Vocabulary(), token) {
		t.Fatal("terminal preservation protocol entered the scheduler vocabulary")
	}
	// protocolVocabulary and protocolOrder stay in lockstep for this name.
	if _, ok := protocolVocabulary[token]; !ok || !slices.Contains(protocolOrder, token) {
		t.Fatal("token missing from vocabulary or order")
	}
}
