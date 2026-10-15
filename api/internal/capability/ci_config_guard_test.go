package capability

import (
	"reflect"
	"testing"
)

// TestCIConfigGuardProtocolSeparation pins ci_config_guard_v1 (PRD #2865) as a worker
// PROTOCOL capability: a worker may self-report it and it is stored, but it never leaks into
// the repo-selectable vocabulary or the self-reportable scheduler capabilities.
func TestCIConfigGuardProtocolSeparation(t *testing.T) {
	if CIConfigGuardV1 != "ci_config_guard_v1" {
		t.Fatalf("wire name = %q", CIConfigGuardV1)
	}
	got := FilterProtocol([]string{CIConfigGuardV1, CompletionInterlockV1, CIConfigGuardV1, "unknown"})
	if !reflect.DeepEqual(got, []string{CompletionInterlockV1, CIConfigGuardV1}) {
		t.Fatalf("protocol filter = %v", got)
	}
	if len(Filter([]string{CIConfigGuardV1})) != 0 || len(SelfReportable([]string{CIConfigGuardV1})) != 0 {
		t.Fatal("ci_config_guard_v1 leaked into repo selectable capabilities")
	}
}
