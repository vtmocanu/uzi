package workersvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteIneligibleBeforeAncestryLiveDB(t *testing.T) {
	for _, mode := range []string{"expiredcap", "closedhold", "missingRun"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrerequisiteExecutorLiveDB(t, 1)
			switch mode {
			case "expiredcap":
				f.expireCaptureLiveDB(t)
			case "closedhold", "missingRun":
				// Exercise owner discard without reconstructing publication evidence.
				// Clear the hold's independent live FK before removing the run.
				n, err := f.env.q.DiscardCustodyHoldForOwner(f.env.ctx, store.DiscardCustodyHoldForOwnerParams{
					HoldID: f.m.row.HoldID, RunID: f.m.row.RunID, UserID: f.m.row.UserID,
					ReleaseEvidence: prerequisiteText("owner_discard"),
				})
				if err != nil || n != 1 {
					t.Fatalf("discard hold: rows=%d err=%v", n, err)
				}
				hold, err := f.env.q.LockPrerequisiteHold(f.env.ctx, f.m.row.HoldID)
				if err != nil {
					t.Fatal(err)
				}
				if hold.State != "discarded" || hold.LiveRunID.Valid || hold.LiveWorkerID.Valid {
					t.Fatal("terminal hold fixture retained custody")
				}
				if mode == "missingRun" {
					f.env.exec("DELETE FROM runs WHERE id=$1", f.m.row.RunID)
					var remains bool
					if err := f.env.pool.QueryRow(f.env.ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)", f.m.row.RunID).Scan(&remains); err != nil {
						t.Fatal(err)
					}
					if remains {
						t.Fatal("run fixture was not deleted")
					}
				}
			}
			compares := 0
			f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
				compares++
				return forge.AncestryUnknown, nil
			}}
			f.runLiveDB(t)
			row := f.readLiveDB(t)
			if row.State != "refused" || row.Reason != "archive_unavailable" {
				t.Fatalf("state=%s reason=%s", row.State, row.Reason)
			}
			if compares != 0 || f.builder.builds != 0 || f.h.requests.Load() != 0 || f.h.sends != 0 || row.InvokedAt.Valid || row.LiveRunID.Valid {
				t.Fatalf("compares=%d builds=%d requests=%d sends=%d marker=%t live=%t",
					compares, f.builder.builds, f.h.requests.Load(), f.h.sends, row.InvokedAt.Valid, row.LiveRunID.Valid)
			}
		})
	}
}
