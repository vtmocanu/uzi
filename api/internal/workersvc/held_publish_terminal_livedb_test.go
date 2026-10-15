package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type heldReadFailureStore struct {
	Store
	heldReader
	fail bool
}

func (s *heldReadFailureStore) GetHeldPublicationHold(ctx context.Context, p store.GetHeldPublicationHoldParams) (store.RecoveryCustodyHold, error) {
	if s.fail {
		return store.RecoveryCustodyHold{}, errors.New("hold observation unavailable")
	}
	return s.heldReader.GetHeldPublicationHold(ctx, p)
}

func TestHeldTerminalPresendReadFailureLiveDB(t *testing.T) {
	f := newHeldFix(t)
	q := &heldReadFailureStore{Store: f.svc.q, heldReader: f.svc.q.(heldReader)}
	f.svc.q = q
	f.svc.heldBeforeSendHook = func() { q.fail = true }
	got, err := f.publish(f.req())
	_, sends, _ := f.fake.counts()
	row, found := f.row()
	if err != nil || got.State != "invoked" || sends != 0 || !found || !row.CreateInvokedAt.Valid {
		t.Fatalf("unknown hold status must preserve marker without send: result=%+v err=%v sends=%d row=%+v", got, err, sends, row)
	}
	q.fail = false
	got, err = f.publish(f.req())
	_, sends, _ = f.fake.counts()
	if err != nil || got.State != "invoked" || sends != 0 {
		t.Fatalf("unknown invocation retried: %+v err=%v sends=%d", got, err, sends)
	}
}

func TestHeldTerminalSettlementOrderingLiveDB(t *testing.T) {
	for _, markerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "settlement_first", true: "marker_first"}[markerFirst], func(t *testing.T) {
			f := newHeldFix(t)
			f.worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
			f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.worker.ID, f.worker.ProtocolCapabilities)
			settle := func() {
				f.e.exec(t, "UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',live_worker_id=NULL,live_run_id=NULL WHERE id=$1", f.hold)
			}
			if markerFirst {
				f.svc.heldBeforeSendHook = settle
			} else {
				settle()
			}
			res, err := f.publish(f.req())
			_, sends, _ := f.fake.counts()
			if sends != 0 {
				t.Fatalf("sends=%d", sends)
			}
			row, found := f.row()
			if !markerFirst {
				if err == nil || found {
					t.Fatalf("settlement admitted: %+v %v row=%v", res, err, found)
				}
				return
			}
			if err != nil || !found || !row.CreateInvokedAt.Valid || row.State != "invoked" {
				t.Fatalf("marker lost: %+v %v", row, err)
			}
			// An absent readback retains unknown invocation; a later exact ref resolves it without send.
			got := f.svc.heldReconcile(f.e.ctx, row)
			if got.State != "invoked" {
				t.Fatalf("absent ref: %+v", got)
			}
			f.fake.setRef(f.ref(), heldTestTip)
			got = f.svc.heldReconcile(f.e.ctx, row)
			if got.State != "created" {
				t.Fatalf("readback: %+v", got)
			}
			_, sends, _ = f.fake.counts()
			if sends != 0 {
				t.Fatalf("reconcile sent: %d", sends)
			}
		})
	}
}

func TestHeldTerminalCreateV2LiveDB(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "completed"} {
		for _, gen := range []int{1, 2} {
			t.Run(status+map[int]string{1: "/current", 2: "/older"}[gen], func(t *testing.T) {
				f := newHeldFix(t)
				f.worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
				f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.worker.ID, f.worker.ProtocolCapabilities)
				f.e.exec(t, "UPDATE runs SET status=$2,claim_generation=$3,worker_id=NULL WHERE id=$1", f.run, status, gen)
				got, err := f.publish(f.req())
				_, sends, _ := f.fake.counts()
				if err != nil || got.State != "created" || sends != 1 {
					t.Fatalf("V2 create: %+v err=%v sends=%d", got, err, sends)
				}
			})
		}
	}
}

func TestHeldTerminalLockedCapabilityLossLiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
	f.e.exec(t, "UPDATE runs SET status='cancelled' WHERE id=$1", f.run)
	// The caller's V2 advertisement cannot override the locked V1 worker row.
	_, err := f.publish(f.req())
	if err == nil {
		t.Fatal("stale capabilities admitted")
	}
	if _, found := f.row(); found {
		t.Fatal("marker created after capability loss")
	}
}
