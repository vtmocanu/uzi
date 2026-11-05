package recovery

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestDiskCustodyLegacyReconcilerSelectionLiveDB(t *testing.T) {
	e := newLegacyInventoryEnv(t)
	capture := e.reserve("disk-reconciler")
	body := e.upload(capture)
	selected := func() bool {
		t.Helper()
		rows, err := e.q.ListReleasableCustodyHolds(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == e.hold {
				return true
			}
		}
		return false
	}
	// Positive control: the available archive qualifies this exact hold before the park.
	if !selected() {
		t.Fatal("available capture did not qualify legacy control")
	}
	e.exec("UPDATE runs SET status='recovery_wait',recovery_wait_cause='data_volume_full',recovery_retry_not_before=NULL,claim_generation=2 WHERE id=$1", e.run)
	if selected() {
		t.Fatal("reconciler selected older-generation disk-held source")
	}
	if n, err := e.q.ReleaseCustodyHold(e.ctx, store.ReleaseCustodyHoldParams{ID: e.hold, ReleaseEvidence: pgconv.TextOrNull("archive")}); err != nil || n != 0 {
		t.Fatalf("legacy per-hold release=%d/%v", n, err)
	}
	e.bytes(capture, body)
	// Only NULL timing is owner-held; the existing finite disk retry is eligible.
	e.exec("UPDATE runs SET recovery_retry_not_before=now()+interval '1 hour' WHERE id=$1", e.run)
	if !selected() {
		t.Fatal("finite disk retry lost ordinary archive eligibility")
	}
	e.exec("UPDATE runs SET recovery_retry_not_before=NULL WHERE id=$1", e.run)
	e.exec("UPDATE runs SET status='cancelled',recovery_wait_cause=NULL WHERE id=$1", e.run)
	if !selected() {
		t.Fatal("cancellation did not restore legacy eligibility")
	}
	if n, err := e.q.ReleaseCustodyHold(e.ctx, store.ReleaseCustodyHoldParams{ID: e.hold, ReleaseEvidence: pgconv.TextOrNull("archive")}); err != nil || n != 1 {
		t.Fatalf("legacy release after cancellation=%d/%v", n, err)
	}
	e.bytes(capture, body)
}

func TestDiskCustodyClosureFinalRefusedLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	for _, afterListing := range []bool{false, true} {
		name := "before listing"
		if afterListing {
			name = "after listing"
		}
		t.Run(name, func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			park := func() {
				fx.exec("UPDATE runs SET claim_generation=2,status='recovery_wait',recovery_wait_cause='data_volume_full',recovery_retry_not_before=NULL,claim_released_at=NULL WHERE id=$1", fx.run)
			}
			if afterListing {
				fx.svc.finalClosureHook = func(stage string) {
					if stage == "observed" {
						park()
					}
				}
			} else {
				park()
			}
			fx.reject(fx.w, fx.request(fx.capture), ErrNotAuthorized)
			fx.assertOpen()
			fx.bytes(fx.capture, fx.body)
			fx.svc.finalClosureHook = nil
			fx.exec("UPDATE runs SET status='cancelled',recovery_wait_cause=NULL WHERE id=$1", fx.run)
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			if err != nil || !ack.Released || ack.HoldsReleased != 1 {
				t.Fatalf("closure release after cancellation=%+v/%v", ack, err)
			}
			fx.assertReleased()
		})
	}
}
