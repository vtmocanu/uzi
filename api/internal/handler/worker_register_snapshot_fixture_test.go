package handler

import (
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #1742 seam: fixtures/worker-register-snapshot/finalize-resume.json is the exact
// active_snapshot a restarted worker sends on register when it holds a finalize-pending record.
// It must decode through the REAL lenient parseActiveSnapshot into FinalizeResume, with the
// pending-terminal half empty. Run with -count=1 after a fixture-only edit (fixtures/ is outside
// this package's cache key).
const workerRegisterSnapshotFixture = "../../../fixtures/worker-register-snapshot/finalize-resume.json"

func TestWorkerRegisterSnapshotFinalizeResumeFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile(workerRegisterSnapshotFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	snap := parseActiveSnapshot(raw, uuid.New())
	if snap == nil {
		t.Fatal("parseActiveSnapshot dropped the register fixture")
	}
	want := []workersvc.FinalizeResumeEntry{{RunID: "11111111-1742-4000-8000-000000000001", ClaimGeneration: 3}}
	if len(snap.FinalizeResume) != 1 || snap.FinalizeResume[0] != want[0] {
		t.Fatalf("FinalizeResume = %+v, want %+v", snap.FinalizeResume, want)
	}
	if len(snap.Active) != 0 || snap.PendingOverflow || snap.RegisterNonce != "" || snap.SnapshotEpoch != 1 {
		t.Fatalf("snapshot = %+v, want epoch 1, no nonce, empty active, no overflow", snap)
	}
}
