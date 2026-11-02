package recovery

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Pair the worker replacement controls with the real transaction: replaceable
// means the old thin capture is already fenced, not a promise to fence later.
func TestRecoveryClosureFinalReplacementFenceLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	for _, accepted := range []bool{false, true} {
		name := "unaccepted"
		if accepted {
			name = "committed receipt wins"
		}
		t.Run(name, func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			_, manifest := closureManifest()
			req := apitypes.RecoveryReconcileRequest{
				Generation: fx.gen, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64),
				Checksum: manifest.Checksum, ByteSize: manifest.ByteSize,
			}
			if accepted {
				ack, err := fx.release(fx.w, fx.request(fx.capture))
				if err != nil || !ack.Released {
					t.Fatalf("initial release: %+v %v", ack, err)
				}
			}
			fx.exec("UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", fx.capture)
			before := fx.snapshot("recovery_custody_holds", fx.hold)
			response, err := fx.svc.Reconcile(fx.ctx, fx.w, fx.run, fx.capture, req)
			if err != nil {
				t.Fatal(err)
			}
			if accepted {
				if response.Outcome != "accepted" || response.FinalReceipt == nil || response.FinalReceipt.CaptureID != fx.capture.String() {
					t.Fatalf("committed thin receipt was replaced: %+v", response)
				}
			} else {
				if response.Outcome != "replaceable" || response.FinalReceipt != nil {
					t.Fatalf("unaccepted thin capture not replaceable: %+v", response)
				}
				var state, reason string
				if err := fx.pool.QueryRow(fx.ctx, "SELECT state,reason FROM recovery_captures WHERE id=$1", fx.capture).Scan(&state, &reason); err != nil || state != "expired" || reason != "unaccepted_capture_replaced" {
					t.Fatalf("replaceable returned before committed fence: state=%q reason=%q err=%v", state, reason, err)
				}
				fx.assertOpen()
			}
			if after := fx.snapshot("recovery_custody_holds", fx.hold); after != before {
				t.Fatal("reconciliation changed custody")
			}
		})
	}
}
