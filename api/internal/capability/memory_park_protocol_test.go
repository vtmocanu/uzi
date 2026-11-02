package capability

import (
	"reflect"
	"testing"
)

// memory_park_settlement_v1 (issue #3639) is a protocol-only capability: it is stored from a
// worker's registration, never offered to users and never placement authority.
func TestMemoryParkSettlementProtocolOnly(t *testing.T) {
	got := FilterProtocol([]string{MemoryParkSettlementV1, WorkerMemoryPressureV1, "memory_park_settlement_v2", MemoryParkSettlementV1})
	want := []string{WorkerMemoryPressureV1, MemoryParkSettlementV1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterProtocol = %v, want %v", got, want)
	}
	if got := Filter([]string{MemoryParkSettlementV1}); len(got) != 0 {
		t.Fatalf("settlement became placement authority: %v", got)
	}
}
