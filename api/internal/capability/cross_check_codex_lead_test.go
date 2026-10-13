package capability

import (
	"reflect"
	"testing"
)

func TestCrossCheckCodexLeadProtocolSeparation(t *testing.T) {
	got := FilterProtocol([]string{CrossCheckCodexLeadV1, CrossCheckPinsV1, CrossCheckCodexLeadV1, "unknown"})
	if !reflect.DeepEqual(got, []string{CrossCheckPinsV1, CrossCheckCodexLeadV1}) {
		t.Fatalf("protocol order = %v", got)
	}
	if CrossCheckCodexLeadV1 != "cross_check_codex_lead_v1" {
		t.Fatalf("wire name drifted: %q", CrossCheckCodexLeadV1)
	}
	if len(Filter([]string{CrossCheckCodexLeadV1})) != 0 || len(SelfReportable([]string{CrossCheckCodexLeadV1})) != 0 {
		t.Fatal("codex-lead checker protocol leaked into repo selectable capabilities")
	}
}
