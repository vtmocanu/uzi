package capability

import (
	"reflect"
	"testing"
)

// PRD #4320 module 5: claude_provider_v1 is a protocol capability only: self-reported through
// the protocol vocabulary (so FilterProtocol keeps it, in the server-owned order) and never a
// repo capability.
func TestClaudeProviderV1IsAProtocolCapability(t *testing.T) {
	if ClaudeProviderV1 != "claude_provider_v1" {
		t.Fatalf("wire name drifted: %q", ClaudeProviderV1)
	}
	got := FilterProtocol([]string{ClaudeProviderV1, CrossCheckGatedV1, ClaudeProviderV1, "unknown"})
	if want := []string{CrossCheckGatedV1, ClaudeProviderV1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterProtocol = %v, want %v", got, want)
	}
	if len(Filter([]string{ClaudeProviderV1})) != 0 || len(SelfReportable([]string{ClaudeProviderV1})) != 0 {
		t.Fatal("claude_provider_v1 must not be a repo capability")
	}
}
