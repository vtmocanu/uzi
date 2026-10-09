package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

type completionRouteForge struct{ settleFakeForge }

func (*completionRouteForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{SourceBranch: "agent/issue-7", HeadSHA: settleHead}, nil
}
func TestCompletedPublicationHandlerLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetBackground(func(func()) {})
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 2507, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker, hold := uuid.New(), uuid.New()
	token := "completion-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'completion',$3,'online',$4)", worker, owner, sum[:], []string{capability.RecoveryCompletedPublicationV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, owner, repo, run, worker)
	f := &completionRouteForge{settleFakeForge: settleFakeForge{head: settleHead, verdict: map[string]forge.Ancestry{settlePushed: forge.AncestryAncestor}}}
	h.wsvc.SetForges(settleForgeBuilder{f: f})
	raw, err := os.ReadFile("../../../fixtures/completed-publication/state-ack.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Request json.RawMessage `json:"request"`
		Ack     struct {
			Receipt apitypes.CompletedPublicationReceipt `json:"completed_publication_receipt"`
		} `json:"ack"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	body := string(wire.Request)
	call := func(auth, payload string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/state", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+auth)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	if rec := call("invalid", body); rec.Code != 401 {
		t.Fatalf("unauthenticated completion: %d %s", rec.Code, rec.Body.String())
	}
	rec := call(token, body)
	var ack struct {
		CompletedPublicationReceipt *apitypes.CompletedPublicationReceipt `json:"completed_publication_receipt"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &ack) != nil || ack.CompletedPublicationReceipt == nil {
		t.Fatalf("completion ACK: %d %s", rec.Code, rec.Body.String())
	}
	receipt := ack.CompletedPublicationReceipt
	if receipt.HoldID != hold.String() || receipt.RunID != run.String() || receipt.WorkerID != worker.String() || receipt.OwnerID != owner.String() || receipt.Generation != 1 || receipt.FinalHead != settlePushed || receipt.ObservedBranchHead != settleHead || receipt.RepoID != repo.String() || receipt.ConnectionID != conn.String() || receipt.Branch != "agent/issue-7" || receipt.MRIID == nil || *receipt.MRIID != 17 || f.compareCalls != 1 {
		t.Fatalf("exact ACK authority: %+v comparison calls=%d", receipt, f.compareCalls)
	}
	expected := wire.Ack.Receipt
	expected.HoldID = hold.String()
	expected.RunID = run.String()
	expected.OwnerID = owner.String()
	expected.WorkerID = worker.String()
	expected.RepoID = repo.String()
	expected.ConnectionID = conn.String()
	actualJSON, _ := json.Marshal(receipt)
	expectedJSON, _ := json.Marshal(expected)
	if string(actualJSON) != string(expectedJSON) {
		t.Fatalf("shared literal receipt drift: got %s want %s", actualJSON, expectedJSON)
	}
	h.wsvc.SetForges(nil)
	replay := call(token, body)
	var replayAck struct {
		CompletedPublicationReceipt *apitypes.CompletedPublicationReceipt `json:"completed_publication_receipt"`
	}
	if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &replayAck) != nil {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	a, _ := json.Marshal(receipt)
	b, _ := json.Marshal(replayAck.CompletedPublicationReceipt)
	if string(a) != string(b) {
		t.Fatalf("receipt replay differs: %s", replay.Body.String())
	}
	changed := strings.Replace(body, settlePushed, settleOther, 1)
	if wrong := call(token, changed); strings.Contains(wrong.Body.String(), "completed_publication_receipt") {
		t.Fatalf("changed head received receipt: %s", wrong.Body.String())
	}
}
