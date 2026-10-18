package capability

import (
	"reflect"
	"testing"
)

func TestMemoryProtocolCoexistsWithPublicationAndCode(t *testing.T) {
	got := FilterProtocol([]string{CrossCheckCodeV1, WorkerMemoryPressureV1,
		RecoveryHeldPublicationV1, RecoveryCompletedPublicationV1, ParkRunV1,
		CodexAccountParkV1, WorkerMemoryPressureV1, "unknown"})
	want := []string{WorkerMemoryPressureV1, RecoveryCompletedPublicationV1,
		RecoveryHeldPublicationV1, CodexAccountParkV1, ParkRunV1, CrossCheckCodeV1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("combined protocols=%v want=%v", got, want)
	}
	if got := Filter([]string{WorkerMemoryPressureV1}); len(got) != 0 {
		t.Fatalf("memory protocol became placement authority: %v", got)
	}
}
