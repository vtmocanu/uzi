package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

// This regression uses only pre-existing service/test interfaces so it can execute
// unchanged against the unfixed base. The fourth counted report must hold, not fail.
func TestDiskCapOwnerHoldRegressionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	gen := int64(1)
	for i := 0; i < 3; i++ {
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(gen, nil)); err != nil || !applied {
			t.Fatalf("timed park %d: applied=%v err=%v", i+1, applied, err)
		}
		gen = e.dpResume(t, id, w)
	}
	hold := mhOpenHold(t, e, id, gen, w)
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(gen, nil))
	if err != nil || !applied {
		t.Fatalf("fourth counted report: applied=%v err=%v", applied, err)
	}
	if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "data_volume_full" || run.RecoveryRetryNotBefore.Valid || run.DiskParkCount != 3 || run.FailOrigin.Valid || run.FinishedAt.Valid {
		t.Fatalf("fourth counted report: status=%s cause=%v retry=%v count=%d origin=%v finished=%v; want owner-held recovery_wait/data_volume_full with NULL retry, count 3 and no failure", run.Status, run.RecoveryWaitCause, run.RecoveryRetryNotBefore, run.DiskParkCount, run.FailOrigin, run.FinishedAt)
	}
	if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
		t.Fatalf("custody=%s, want open", state)
	}
	duplicate, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(gen, nil))
	if err != nil || applied || duplicate.Status != "recovery_wait" || duplicate.DiskParkCount != 3 || duplicate.RecoveryRetryNotBefore.Valid {
		t.Fatalf("lost ACK: applied=%v run=%+v err=%v", applied, duplicate, err)
	}
}
