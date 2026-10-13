package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCompletedPublicationFencesLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "self_improve"} {
		t.Run(kind, func(t *testing.T) { completedPublicationFences(t, kind) })
	}
}

func completedPublicationFences(t *testing.T, kind string) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, []string{capability.RecoveryCompletedPublicationV1})
	run := e.seedLegacyRunningRun(t, wid)
	hold := uuid.New()
	gen := int64(1)
	head := strings.Repeat("a", 40)
	branch := "agent/issue-1"
	if kind == "self_improve" {
		branch = selfImproveBranch(run)
		e.exec(t, "UPDATE runs SET kind='self_improve',branch='agent/poisoned' WHERE id=$1", run)
	}
	mr := int64(7)
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
	e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
	svc := e.permitService(t)
	w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: []string{capability.RecoveryCompletedPublicationV1}}
	req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
	result, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
	if err != nil || !result.Applied || result.CompletedPublicationReason != "ancestry_unknown" {
		t.Fatalf("retained completion: %+v %v", result, err)
	}
	h, err := e.q.GetCompletedPublicationHold(e.ctx, store.GetCompletedPublicationHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.CompletionIdentity) == 0 {
		t.Fatal("completed transition did not stamp identity")
	}
	zero := func(sql string, args ...any) {
		t.Helper()
		tag, err := e.pool.Exec(e.ctx, sql, args...)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("database fence accepted write: %s rows=%d err=%v", sql, tag.RowsAffected(), err)
		}
	}
	zero("UPDATE recovery_custody_holds SET completion_identity='{}' WHERE id=$1", hold)
	zero("UPDATE recovery_custody_holds SET completion_identity=NULL WHERE id=$1", hold)
	zero(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded,completion_identity)
 VALUES($1,$2,$3,$4,8,'open',$5,'ident',true,$6)`, uuid.New(), e.userID, e.repoID, run, wid, h.CompletionIdentity)
	zero(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,completed_publication_receipt)
 VALUES($1,$2,$3,$4,9,'open',$5,'ident','{}')`, uuid.New(), e.userID, e.repoID, run, wid)
	zero(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,status,completion_final_head) VALUES($1,$2,$3,'issue',99,'completed',$4)`, uuid.New(), e.userID, e.repoID, head)
	for _, partial := range []string{"{}", "null", `{"observed_branch_head":null}`, `{"observed_branch_head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`} {
		t.Run("partial_"+partial, func(t *testing.T) {
			if _, err := e.pool.Exec(e.ctx, "UPDATE recovery_custody_holds SET completed_publication_receipt=$2::jsonb WHERE id=$1", hold, partial); err == nil {
				t.Fatal("null-safe receipt constraint accepted partial receipt")
			}
		})
	}
	changed := strings.Repeat("c", 40)
	bad := req
	bad.CompletionFinalHead = &changed
	denied, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, bad)
	if err != nil {
		t.Fatal(err)
	}
	if denied.Applied || denied.CompletedPublicationReceipt != nil {
		t.Fatalf("changed duplicate direct completion accepted: %+v", denied)
	}
	var storedHead string
	if err := e.pool.QueryRow(e.ctx, "SELECT completion_final_head FROM runs WHERE id=$1", run).Scan(&storedHead); err != nil || storedHead != head {
		t.Fatalf("changed final identity: %s %v", storedHead, err)
	}
	// Direct SQL release must recheck the binding, independently of the query predicate.
	e.exec(t, "UPDATE repos SET forge_project_id=2 WHERE id=$1", e.repoID)
	zero(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,released_at=now(),
 final_disposition='completed_publication',release_evidence='completed_publication',completed_publication_reason=NULL,
 completed_publication_receipt=completion_identity||jsonb_build_object('observed_branch_head',$2::text) WHERE id=$1`, hold, head)
	e.exec(t, "UPDATE repos SET forge_project_id=1 WHERE id=$1", e.repoID)
	// A trigger can refuse with zero affected rows. Verification must retain and never fabricate ACK evidence.
	e.exec(t, `CREATE FUNCTION test_publication_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='`+hold.String()+`' AND NEW.completed_publication_receipt IS NOT NULL THEN RETURN NULL; END IF; RETURN NEW; END $$`)
	e.exec(t, "CREATE TRIGGER test_publication_refuse BEFORE UPDATE ON recovery_custody_holds FOR EACH ROW EXECUTE FUNCTION test_publication_refuse()")
	svc.SetForges(settleUnitBuilder{f: &publicationForge{t: t, projectID: 1, mrIID: 7, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryAncestor}})
	refused, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
	e.exec(t, "DROP TRIGGER test_publication_refuse ON recovery_custody_holds")
	e.exec(t, "DROP FUNCTION test_publication_refuse()")
	if err != nil || !refused.Applied || refused.CompletedPublicationReceipt != nil || refused.CompletedPublicationReason != "identity_changed" {
		t.Fatalf("zero-row refusal fabricated receipt: %+v %v", refused, err)
	}
	success, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
	if err != nil || success.CompletedPublicationReceipt == nil {
		t.Fatalf("retry release: %+v %v", success, err)
	}
	zero("UPDATE recovery_custody_holds SET state='open',live_worker_id=$2,live_run_id=$3,final_disposition='settled',release_evidence='publication',completed_publication_receipt=NULL WHERE id=$1", hold, wid, run)
	zero("UPDATE recovery_custody_holds SET completed_publication_receipt='{}' WHERE id=$1", hold)
	zero("UPDATE recovery_custody_holds SET completed_publication_reason='forge_timeout' WHERE id=$1", hold)
	n, err := e.q.RecordCompletedPublicationRefusal(e.ctx, store.RecordCompletedPublicationRefusalParams{HoldID: hold, RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen, Identity: h.CompletionIdentity, Reason: pgconv.Text("forge_timeout")})
	if err != nil || n != 0 {
		t.Fatal("delayed refusal modified released hold")
	}
	n, err = e.q.ReleaseFinalInventoryHold(e.ctx, store.ReleaseFinalInventoryHoldParams{
		ID: hold, RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen,
		FinalDisposition: "settled", ReleaseEvidence: "publication",
		FinalCoverageDigest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	})
	if err != nil || n != 0 {
		t.Fatalf("delayed fallback mutated publication release: rows=%d err=%v", n, err)
	}
	got, _ := json.Marshal(success.CompletedPublicationReceipt)
	var receipt []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT completed_publication_receipt FROM recovery_custody_holds WHERE id=$1", hold).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal(receipt, &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatal("immutable receipt changed")
	}
}
