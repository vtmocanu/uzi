package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"

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

func TestHeldTerminalCustodyControlsV2LiveDB(t *testing.T) {
	for _, bad := range []string{"live_custodian", "missing_custodian", "live_run", "missing_live_run", "repo", "future_generation", "original_worker", "user", "lane", "running", "push_secret_blocked", "worker_residue_blocked", "missing_guard", "locked_drift"} {
		t.Run(bad, func(t *testing.T) {
			f := newHeldFix(t)
			f.worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
			f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.worker.ID, f.worker.ProtocolCapabilities)
			f.e.exec(t, "UPDATE runs SET status='completed',claim_generation=2,worker_id=NULL WHERE id=$1", f.run)
			req := f.req()
			mutate := func() {
				switch bad {
				case "live_custodian", "original_worker":
					other := f.e.seedWorker(t, f.worker.ProtocolCapabilities)
					column := "live_worker_id"
					if bad == "original_worker" {
						column = "original_worker_id"
					}
					f.rawHold("UPDATE recovery_custody_holds SET "+column+"=$2 WHERE id=$1", f.hold, other)
				case "missing_custodian":
					f.rawHold("UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1", f.hold)
				case "live_run":
					other := f.e.seedLegacyRunningRun(t, f.worker.ID)
					f.rawHold("UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", f.hold, other)
				case "missing_live_run":
					f.rawHold("UPDATE recovery_custody_holds SET live_run_id=NULL WHERE id=$1", f.hold)
				case "repo":
					other := uuid.New()
					f.e.exec(t, "INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) SELECT $1,connection_id,999,'g/other',web_url,default_branch,true FROM repos WHERE id=$2", other, f.e.repoID)
					f.rawHold("UPDATE recovery_custody_holds SET repo_id=$2 WHERE id=$1", f.hold, other)
				case "future_generation":
					// Keep exact request/hold/publication identity; only the run is behind.
					f.e.exec(t, "UPDATE runs SET claim_generation=0 WHERE id=$1", f.run)
				case "user":
					other := uuid.New()
					f.e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@example.com")
					f.rawHold("UPDATE recovery_custody_holds SET user_id=$2 WHERE id=$1", f.hold, other)
				case "lane":
					f.worker.IsolatedLane = true
					f.e.exec(t, "UPDATE workers SET isolated_lane=true,kind='hosted',ephemeral=true,hosted_size='small',template_declared='default' WHERE id=$1", f.worker.ID)
				case "running":
					f.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", f.run)
				case "push_secret_blocked", "worker_residue_blocked":
					f.e.exec(t, "UPDATE runs SET fail_origin=$2 WHERE id=$1", f.run, bad)
				case "missing_guard", "locked_drift":
					f.rawHold("UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", f.hold)
				}
			}
			if bad == "locked_drift" {
				if res, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, req); err != nil || res != nil {
					t.Fatalf("admission: %+v %v", res, err)
				}
				f.fake.prepareFn = func(pushbroker.HeldPackOptions) error { mutate(); return nil }
			} else {
				mutate()
			}
			if _, err := f.publish(req); err == nil {
				t.Fatal("invalid terminal custody admitted")
			}
			if _, sends, _ := f.fake.counts(); sends != 0 {
				t.Fatalf("sends=%d", sends)
			}
			var rows int
			if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM run_held_publications WHERE run_id=$1", f.run).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("markers=%d err=%v", rows, err)
			}
		})
	}
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
