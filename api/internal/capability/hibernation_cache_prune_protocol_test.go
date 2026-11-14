package capability

import (
	"reflect"
	"testing"
)

// hibernation_cache_prune_v1 (PRD #4206) is a protocol-only capability: it is stored from a
// worker's registration, never offered to users and never placement authority.
func TestHibernationCachePruneProtocolOnly(t *testing.T) {
	if HibernationCachePruneV1 != "hibernation_cache_prune_v1" {
		t.Fatalf("wire name changed: %q", HibernationCachePruneV1)
	}
	got := FilterProtocol([]string{HibernationCachePruneV1, DindMaintenanceV1, "hibernation_cache_prune_v2", HibernationCachePruneV1})
	want := []string{DindMaintenanceV1, HibernationCachePruneV1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterProtocol = %v, want %v", got, want)
	}
	if got := Filter([]string{HibernationCachePruneV1}); len(got) != 0 {
		t.Fatalf("became placement authority: %v", got)
	}
	for _, v := range Vocabulary() {
		if v == HibernationCachePruneV1 {
			t.Fatalf("leaked into the user vocabulary")
		}
	}
}
