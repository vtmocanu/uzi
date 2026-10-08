package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMemoryArchiveRetainsCustodyLiveDB(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		name := "legacy"
		if guarded {
			name = "guarded"
		}
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, 2)
			f.capture(f.holds[0], f.runs[0], f.owner, "available")
			f.capture(f.holds[1], f.runs[1], f.owner, "available")
			// Select before parking to cover a delayed reconciler candidate too.
			f.exec("UPDATE recovery_custody_holds SET inventory_guarded=$2 WHERE id=$1", f.holds[0], guarded)
			f.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_memory_pressure', claim_released_at=now() WHERE id=$1", f.runs[0])
			holds, err := f.q.ListReleasableCustodyHolds(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			control := false
			for _, h := range holds {
				if h.ID == f.holds[0] {
					t.Fatal("memory custody selected for implicit archive release")
				}
				if h.ID == f.holds[1] {
					control = true
				}
			}
			if !control {
				t.Fatal("ordinary available archive not selected")
			}
			n, err := f.q.ReleaseCustodyHold(f.ctx, store.ReleaseCustodyHoldParams{
				ID: f.holds[0], ReleaseEvidence: pgtype.Text{String: "archive", Valid: true}})
			if err != nil || n != 0 {
				t.Fatalf("memory release rows=%d err=%v", n, err)
			}
			assertExhaustionCustodyRetained(t, f)
			n, err = f.q.ReleaseCustodyHold(f.ctx, store.ReleaseCustodyHoldParams{
				ID: f.holds[1], ReleaseEvidence: pgtype.Text{String: "archive", Valid: true}})
			if err != nil || n != 1 {
				t.Fatalf("ordinary release rows=%d err=%v", n, err)
			}
		})
	}
}
