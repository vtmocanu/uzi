package capability

import (
	"reflect"
	"testing"
)

func TestFilterProtocolPrerequisiteClosure(t *testing.T) {
	const literal = "recovery_prerequisite_closure_v1"
	if RecoveryPrerequisiteClosureV1 != literal {
		t.Fatal(RecoveryPrerequisiteClosureV1)
	}
	want := []string{RecoveryHeldPublicationV2, literal, CodexHarnessV1}
	got := FilterProtocol([]string{CodexHarnessV1, literal, RecoveryHeldPublicationV2, literal, "unknown"})
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(FilterProtocol(got), want) {
		t.Fatalf("protocol roundtrip/order/dedup: %v", got)
	}
	if len(Filter([]string{literal})) != 0 || len(SelfReportable([]string{literal})) != 0 {
		t.Fatal("protocol leaked to scheduler")
	}
	for _, v := range Vocabulary() {
		if v == literal {
			t.Fatal("scheduler vocabulary")
		}
	}
}
