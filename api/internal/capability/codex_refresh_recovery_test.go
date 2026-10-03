package capability

import (
	"slices"
	"testing"
)

func TestCodexRefreshRecoveryProtocolOnly(t *testing.T) {
	if got := FilterProtocol([]string{CodexRefreshRecoveryV1, CodexRefreshRecoveryV1}); len(got) != 1 || got[0] != CodexRefreshRecoveryV1 {
		t.Fatalf("protocol flag lost: %v", got)
	}
	if got := Filter([]string{CodexRefreshRecoveryV1}); len(got) != 0 {
		t.Fatalf("scheduler flag: %v", got)
	}
	if got := SelfReportable([]string{CodexRefreshRecoveryV1}); len(got) != 0 {
		t.Fatalf("self report flag: %v", got)
	}
	if slices.Contains(Vocabulary(), CodexRefreshRecoveryV1) {
		t.Fatal("flag leaked into vocabulary")
	}
}
