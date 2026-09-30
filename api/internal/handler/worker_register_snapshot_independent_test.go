package handler

import (
	"testing"

	"github.com/google/uuid"
)

// Issue #1742: the lenient parseActiveSnapshot decodes finalize_resume independently of active,
// so a semantically invalid Active list (which the service later drops) cannot take a valid
// finalize list down with it at the wire layer.
func TestParseActiveSnapshotKeepsFinalizeListBesideInvalidActive(t *testing.T) {
	raw := []byte(`{"snapshot_epoch":1,"active":[{"run_id":"not-a-uuid","claim_generation":-1,"phase":"bogus","terminal_pending":true}],` +
		`"pending_overflow":true,"finalize_resume":[{"run_id":"11111111-1742-4000-8000-000000000001","claim_generation":3}]}`)
	snap := parseActiveSnapshot(raw, uuid.New())
	if snap == nil {
		t.Fatal("snapshot dropped")
	}
	if len(snap.Active) != 1 || len(snap.FinalizeResume) != 1 || snap.FinalizeResume[0].ClaimGeneration != 3 {
		t.Fatalf("snapshot = %+v, want both lists decoded", snap)
	}
}

func TestParseActiveSnapshotWithoutFinalizeListIsUnchanged(t *testing.T) {
	snap := parseActiveSnapshot([]byte(`{"snapshot_epoch":1,"active":[],"pending_overflow":false}`), uuid.New())
	if snap == nil || snap.FinalizeResume != nil {
		t.Fatalf("snapshot = %+v, want nil FinalizeResume", snap)
	}
}
