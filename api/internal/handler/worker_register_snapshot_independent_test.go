package handler

import (
	"testing"

	"github.com/google/uuid"
)

// Issue #1742: parseActiveSnapshot decodes finalize_resume independently of active (a semantically
// invalid Active list, which the service later drops, cannot take a valid finalize list down with
// it at the wire layer), and a wrongly TYPED finalize_resume drops only the finalize list, never
// the rest of the snapshot (ActiveSnapshot.UnmarshalJSON).
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

// A finalize_resume of the wrong wire type must drop only the finalize list: valid Active
// terminal_pending leases and pending_overflow (#1391) survive.
func TestParseActiveSnapshotWrongTypedFinalizeKeepsRestOfSnapshot(t *testing.T) {
	const runID = "11111111-1742-4000-8000-000000000002"
	for name, fin := range map[string]string{
		"string generation": `[{"run_id":"` + runID + `","claim_generation":"3"}]`,
		"float generation":  `[{"run_id":"` + runID + `","claim_generation":3.5}]`,
		"numeric run_id":    `[{"run_id":7,"claim_generation":3}]`,
		"object not array":  `{"run_id":"` + runID + `"}`,
		"scalar":            `"x"`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"snapshot_epoch":0,"active":[{"run_id":"` + runID + `","claim_generation":2,"phase":"running","terminal_pending":true}],` +
				`"pending_overflow":true,"finalize_resume":` + fin + `}`)
			snap := parseActiveSnapshot(raw, uuid.New())
			if snap == nil {
				t.Fatal("whole snapshot dropped by a malformed finalize_resume")
			}
			if len(snap.Active) != 1 || !snap.Active[0].TerminalPending || snap.Active[0].ClaimGeneration != 2 || !snap.PendingOverflow {
				t.Fatalf("snapshot = %+v, want Active and pending_overflow intact", snap)
			}
			if len(snap.FinalizeResume) != 0 {
				t.Fatalf("FinalizeResume = %+v, want empty", snap.FinalizeResume)
			}
		})
	}
}

// A wrongly typed field OUTSIDE finalize_resume still drops the whole snapshot, as before.
func TestParseActiveSnapshotWrongTypedActiveStillDropsSnapshot(t *testing.T) {
	if snap := parseActiveSnapshot([]byte(`{"active":"nope"}`), uuid.New()); snap != nil {
		t.Fatalf("snapshot = %+v, want nil", snap)
	}
}
