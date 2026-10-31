package capability

import (
	"reflect"
	"testing"
)

// PRD #3708: cross_check_gated_v1 is a protocol capability only: self-reported through the
// protocol vocabulary (so FilterProtocol keeps it, in the server-owned order) and never a
// repo capability.
func TestCrossCheckGatedV1IsAProtocolCapability(t *testing.T) {
	if CrossCheckGatedV1 != "cross_check_gated_v1" {
		t.Fatalf("wire name drifted: %q", CrossCheckGatedV1)
	}
	got := FilterProtocol([]string{CrossCheckGatedV1, CrossCheckCodexLeadV1, CrossCheckGatedV1, "unknown"})
	if want := []string{CrossCheckCodexLeadV1, CrossCheckGatedV1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterProtocol = %v, want %v", got, want)
	}
	if len(Filter([]string{CrossCheckGatedV1})) != 0 || len(SelfReportable([]string{CrossCheckGatedV1})) != 0 {
		t.Fatal("cross_check_gated_v1 must not be a repo capability")
	}
}
