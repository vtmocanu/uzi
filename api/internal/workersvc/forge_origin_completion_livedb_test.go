package workersvc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise both real completion transactions, including the stamp trigger that
// precedes forge_origin_reservation_lifecycle and the later source intent write.
func TestForgeCompletionServiceSuccessLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "mr_rework", "self_improve"} {
		for _, permit := range []bool{false, true} {
			modes := []string{"exact", "no_head", "new_failure", "wrong_version"}
			if kind == "issue" {
				modes = append(modes, "legacy", "downgraded", "wrong_mapping", "wrong_generation", "peer", "broker", "cancel", "report_only", "source_failure")
			}
			if permit {
				modes = append(modes, "permit_refused")
			}
			for _, mode := range modes {
				t.Run(fmt.Sprintf("%s/permit=%t/%s", kind, permit, mode), func(t *testing.T) {
					f := seedForgeCompletion(t, kind, permit)
					e, w, id := f.e, f.w, f.id
					var sibling uuid.UUID
					if mode == "exact" {
						sibling = e.seedWorker(t, nil)
						e.exec(t, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
						 SELECT user_id,$2,origin,cooled_until,last_reason,observed_at FROM forge_origin_cooldowns WHERE worker_id=$1`, w.ID, sibling)
					}
					var identityAbsent bool
					if err := e.pool.QueryRow(e.ctx, "SELECT completion_identity IS NULL FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&identityAbsent); err != nil || !identityAbsent {
						t.Fatalf("initial identity absent=%v err=%v", identityAbsent, err)
					}
					switch mode {
					case "no_head":
						f.req.CompletionFinalHead = nil
					case "legacy":
						f.w.ProtocolCapabilities = nil
						e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", w.ID)
					case "downgraded":
						e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", w.ID)
					case "new_failure":
						e.exec(t, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", w.ID)
					case "wrong_mapping":
						e.exec(t, "UPDATE run_forge_origins SET connection_id=$2 WHERE run_id=$1", id, uuid.New())
					case "wrong_version":
						e.exec(t, "UPDATE run_forge_origins SET reservation_version=8 WHERE run_id=$1", id)
					case "wrong_generation":
						e.exec(t, "UPDATE run_forge_origins SET generation=2 WHERE run_id=$1", id)
					case "peer":
						peer := e.fpSeedRunning(t, w.ID, 1)
						e.exec(t, "UPDATE forge_origin_cooldowns SET reservation_run_id=$2 WHERE worker_id=$1", w.ID, peer)
					case "broker":
						// Broker admission carries no direct-forge claim snapshot.
						e.exec(t, "DELETE FROM run_forge_origins WHERE run_id=$1", id)
					case "cancel":
						e.exec(t, "UPDATE runs SET status='cancelled',stop_kind='cancelled' WHERE id=$1", id)
					case "report_only":
						f.req = reportOnlyRequest()
					case "permit_refused":
						e.exec(t, "DELETE FROM run_completion_permits WHERE run_id=$1", id)
					case "source_failure":
						f.req.SourcePublication = &SourcePublicationReport{Version: 1, PublishedHead: *f.req.CompletionFinalHead}
						f.svc.SetTxBeginner(sourceStampFailureBeginner{e.pool, "BindSourcePublicationIntent"})
					}
					result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.w, id, f.req)
					if mode == "source_failure" {
						status, stamp, consumed := f.stored(t)
						var untouched bool
						readErr := e.pool.QueryRow(e.ctx, `SELECT h.completion_identity IS NULL AND r.completion_final_head IS NULL
       AND c.reservation_run_id=r.id AND c.reservation_generation=r.claim_generation
       AND c.reservation_incarnation=m.reservation_incarnation AND c.reservation_version=m.reservation_version
       AND c.observation_version=c.reservation_observation_version
       FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
       JOIN run_forge_origins m ON m.run_id=r.id
       JOIN forge_origin_cooldowns c ON c.worker_id=r.worker_id AND c.origin=m.origin
       WHERE r.id=$1`, id).Scan(&untouched)
						if err == nil || !strings.Contains(err.Error(), "injected BindSourcePublicationIntent write failure") || result.Applied ||
							status != "running" || len(stamp) != 0 || consumed || readErr != nil || !untouched || e.originCount(t, w.ID) != 1 {
							t.Fatalf("rollback: result=%+v err=%v status=%s stamp=%s consumed=%v untouched=%v read=%v", result, err, status, stamp, consumed, untouched, readErr)
						}
						return
					}
					if mode == "permit_refused" {
						var intact bool
						readErr := e.pool.QueryRow(e.ctx, `SELECT r.status='running' AND r.completion_final_head IS NULL
						 AND h.completion_identity IS NULL AND c.reservation_run_id=r.id AND c.reservation_generation=r.claim_generation
						 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
						 JOIN forge_origin_cooldowns c ON c.worker_id=r.worker_id WHERE r.id=$1`, id).Scan(&intact)
						if err != nil || result.Applied || readErr != nil || !intact {
							t.Fatalf("refused permit changed completion: %+v err=%v intact=%v read=%v", result, err, intact, readErr)
						}
					} else if mode != "cancel" && (err != nil || !result.Applied) {
						t.Fatalf("completion: %+v %v", result, err)
					}
					wantClear := mode == "exact"
					if got := e.originCount(t, w.ID); (got == 0) != wantClear {
						t.Fatalf("cooldown count=%d want clear=%v result=%+v err=%v", got, wantClear, result, err)
					}
					if mode == "no_head" || mode == "legacy" || mode == "downgraded" || mode == "report_only" {
						var released bool
						if err := e.pool.QueryRow(e.ctx, `SELECT reservation_run_id IS NULL AND reservation_generation IS NULL
       AND reservation_incarnation IS NULL AND reservation_version IS NULL AND reservation_observation_version IS NULL
       FROM forge_origin_cooldowns WHERE worker_id=$1`, w.ID).Scan(&released); err != nil || !released {
							t.Fatalf("no-proof completion retained reservation: released=%v err=%v", released, err)
						}
					}
					if mode == "cancel" {
						var consumed bool
						if err := e.pool.QueryRow(e.ctx, "SELECT EXISTS(SELECT 1 FROM run_completion_permits WHERE run_id=$1 AND consumed_at IS NOT NULL)", id).Scan(&consumed); err != nil || consumed || result.Applied || e.runStatus(t, id) != "cancelled" {
							t.Fatalf("cancelled completion applied=%v consumed=%v err=%v", result.Applied, consumed, err)
						}
					}
					if wantClear {
						if e.originCount(t, sibling) != 1 {
							t.Fatal("completion cleared another worker's cooldown")
						}
						var stamped bool
						if err := e.pool.QueryRow(e.ctx, `SELECT r.status='completed' AND h.completion_identity IS NOT NULL
       AND h.completed_publication_receipt IS NULL
       AND ($2::boolean = EXISTS(SELECT 1 FROM run_completion_permits p WHERE p.run_id=r.id AND p.consumed_at IS NOT NULL))
       FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id WHERE r.id=$1`, id, permit).Scan(&stamped); err != nil || !stamped {
							t.Fatalf("committed identity/permit without receipt=%v err=%v", stamped, err)
						}
						if result.CompletedPublicationReceipt != nil || result.CompletedPublicationReason != "ancestry_unknown" {
							t.Fatalf("ancestry refusal must preserve completion: %+v", result)
						}
					}
				})
			}
		}
	}
}

func seedForgeCompletion(t *testing.T, kind string, permit bool) sourcePublicationFixture {
	t.Helper()
	e := setupInterlockLiveDB(t)
	caps := []string{capability.RecoveryCompletedPublicationV1, "completion_interlock_v1"}
	wid := e.seedWorker(t, caps)
	id := e.seedLegacyRunningRun(t, wid)
	if permit {
		id = e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	}
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
	var iid int64
	if err := e.pool.QueryRow(e.ctx, "SELECT issue_iid FROM runs WHERE id=$1", id).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	branch, head := agentIssueBranch(iid), strings.Repeat("a", 40)
	if kind == "self_improve" {
		branch = selfImproveBranch(id)
		e.exec(t, "UPDATE runs SET kind='self_improve' WHERE id=$1", id)
	}
	if kind == "mr_rework" {
		branch = "agent/rework"
		e.exec(t, "UPDATE runs SET kind='mr_rework',pipeline_ref=$2,mr_iid=7,target_run_id=$1 WHERE id=$1", id, branch)
	}
	hold := uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, id, wid)
	bindForgeSuccessReservation(t, e, id, wid)
	svc := e.permitService(t)
	w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}
	gen, mr := int64(1), int64(7)
	req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
	if permit {
		req.Head = &head
		p, err := svc.RequestCompletionPermit(e.ctx, w, id, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
		if err != nil || !p.Granted {
			t.Fatalf("permit: %+v %v", p, err)
		}
	}
	svc.SetForges(settleUnitBuilder{f: &publicationForge{t: t, projectID: 1, mrIID: 7, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryUnknown}})
	return sourcePublicationFixture{e: e, svc: svc, w: w, id: id, req: req}
}
