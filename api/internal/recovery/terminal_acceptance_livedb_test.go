package recovery

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

func terminalHeldV2(e *heldEnv) {
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.w.ID, e.w.ProtocolCapabilities)
	e.exec("UPDATE runs SET status='completed',claim_generation=2,worker_id=NULL WHERE id=$1", e.run)
}

// rawTerminalHold builds inconsistent legacy custody without bypassing foreign keys or checks.
func rawTerminalHold(e *heldEnv, sql string, args ...any) {
	e.t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	for _, q := range []string{"ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard", sql, "ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard"} {
		var a []any
		if q == sql {
			a = args
		}
		if _, err := tx.Exec(e.ctx, q, a...); err != nil {
			e.t.Fatalf("fixture: %v", err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func terminalSnapshot(e *heldEnv) string {
	e.t.Helper()
	var snapshot string
	if err := e.pool.QueryRow(e.ctx, "SELECT row_to_json(h)::text || row_to_json(p)::text FROM recovery_custody_holds h JOIN run_held_publications p ON p.hold_id=h.id WHERE h.id=$1", e.hold).Scan(&snapshot); err != nil {
		e.t.Fatal(err)
	}
	return snapshot
}

func TestHeldFinalTerminalCustodyControlsV2LiveDB(t *testing.T) {
	for _, bad := range []string{"live_custodian", "missing_custodian", "live_run", "missing_live_run", "repo", "future_generation", "original_worker", "user", "lane", "running", "push_secret_blocked", "worker_residue_blocked", "missing_guard", "locked_drift"} {
		t.Run(bad, func(t *testing.T) {
			e := newHeldEnv(t)
			terminalHeldV2(e)
			req := e.request()
			mutate := func() {
				switch bad {
				case "live_custodian", "original_worker":
					other := uuid.New()
					e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", other, e.user, other.String(), other[:])
					column := "live_worker_id"
					if bad == "original_worker" {
						column = "original_worker_id"
					}
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET "+column+"=$2 WHERE id=$1", e.hold, other)
				case "missing_custodian":
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1", e.hold)
				case "live_run":
					other := uuid.New()
					e.exec("INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,issue_title,issue_description,status) VALUES($1,$2,$3,2,'issue','t','d','completed')", other, e.user, e.repo)
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", e.hold, other)
				case "missing_live_run":
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET live_run_id=NULL WHERE id=$1", e.hold)
				case "repo":
					other := uuid.New()
					e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) SELECT $1,connection_id,999,'g/other',web_url,default_branch,true FROM repos WHERE id=$2", other, e.repo)
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET repo_id=$2 WHERE id=$1", e.hold, other)
				case "future_generation":
					// The exact hold exists; the run alone has not reached its generation.
					e.exec("UPDATE runs SET claim_generation=0 WHERE id=$1", e.run)
				case "user":
					other := uuid.New()
					e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", other, other.String()+"@example.com")
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET user_id=$2 WHERE id=$1", e.hold, other)
				case "lane":
					e.w.IsolatedLane = true
					e.exec("UPDATE workers SET isolated_lane=true,kind='hosted',ephemeral=true,hosted_size='small',template_declared='default' WHERE id=$1", e.w.ID)
				case "running":
					e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
				case "push_secret_blocked", "worker_residue_blocked":
					e.exec("UPDATE runs SET fail_origin=$2 WHERE id=$1", e.run, bad)
				case "missing_guard", "locked_drift":
					rawTerminalHold(e, "UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", e.hold)
				}
			}
			var before string
			if bad == "locked_drift" {
				e.svc.heldPreLockHook = func() { mutate(); before = terminalSnapshot(e) }
			} else {
				mutate()
				before = terminalSnapshot(e)
			}
			res, err := e.release(e.w, req)
			if err != nil && !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("unexpected refusal error: %v", err)
			}
			if res.Released || res.HoldsReleased != 0 || (err == nil && !res.Retained) {
				t.Fatalf("invalid FINAL ACK: %+v %v", res, err)
			}
			if before == "" || terminalSnapshot(e) != before {
				t.Fatal("invalid FINAL mutated custody or publication")
			}
			if len(e.settled) != 0 {
				t.Fatal("invalid FINAL settled custody")
			}
			if bad != "locked_drift" && e.lists() != 0 {
				t.Fatalf("ineligible custody reached forge verification: lists=%d", e.lists())
			}
			if bad == "locked_drift" && e.lists() != 1 {
				t.Fatalf("drift did not follow eligible forge precheck: lists=%d", e.lists())
			}
		})
	}
}

func TestRecoveryCompletedKindDispositionsLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "prompt", "task"} {
		t.Run(kind+"/archive", func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE runs SET kind=$2,issue_iid=NULL,pipeline_id=123,pipeline_ref='main',branch='agent/completed-kind' WHERE id=$1", e.run, kind)
			id := e.reserve("completed-kind")
			body := e.upload(id)
			e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
			e.release(e.request(id))
			e.bytes(id, body)
		})
		t.Run(kind+"/settled", func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE runs SET kind=$2,status='completed',issue_iid=NULL,pipeline_id=123,pipeline_ref='main',branch='agent/completed-kind' WHERE id=$1", e.run, kind)
			evidence := "forge_no_output"
			e.release(apitypes.RecoveryReleaseRequest{Generation: &e.gen, ReleaseEvidence: &evidence, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}})
		})
		t.Run(kind+"/held", func(t *testing.T) {
			e := newHeldEnv(t)
			terminalHeldV2(e)
			e.exec("UPDATE runs SET kind=$2,issue_iid=NULL,pipeline_id=123,pipeline_ref='main',branch='agent/completed-kind' WHERE id=$1", e.run, kind)
			e.mustRelease()
		})
	}
}

func TestHeldFinalPersistedExpiryReplayLiveDB(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		name := "configured"
		if shortened {
			name = "owner_shortened"
		}
		t.Run(name, func(t *testing.T) {
			e := newHeldEnv(t)
			terminalHeldV2(e)
			e.mustRelease()
			var ack, expiry time.Time
			if err := e.pool.QueryRow(e.ctx, "SELECT acknowledged_at,expires_at FROM run_held_publications WHERE id=$1", e.pub).Scan(&ack, &expiry); err != nil {
				t.Fatal(err)
			}
			if expiry.Sub(ack) != 36*time.Hour {
				t.Fatalf("initial retention=%s", expiry.Sub(ack))
			}
			if shortened {
				e.exec("UPDATE run_held_publications SET expires_at=acknowledged_at+interval '1 hour' WHERE id=$1", e.pub)
			}
			before := terminalSnapshot(e)
			e.retention = 168 * time.Hour
			e.svc = e.newService()
			e.mustRelease()
			if terminalSnapshot(e) != before {
				t.Fatal("exact replay changed persisted receipt or timestamps")
			}
		})
	}
}
