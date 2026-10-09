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
	"github.com/vtmocanu/uzi/api/internal/store"
)

type completionRouteForge struct {
	settleFakeForge
	t      *testing.T
	branch string
}

func (f *completionRouteForge) BranchHead(ctx context.Context, projectID int64, branch string) (string, error) {
	if projectID != 2507 || branch != f.branch {
		f.t.Fatalf("own-repository canonical branch: project=%d branch=%q want=%q", projectID, branch, f.branch)
	}
	return f.settleFakeForge.BranchHead(ctx, projectID, branch)
}

func (f *completionRouteForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{SourceBranch: f.branch, HeadSHA: settleHead}, nil
}
func TestCompletedPublicationHandlerLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "mr_rework", "self_improve"} {
		t.Run(kind, func(t *testing.T) { completedPublicationHandler(t, kind) })
	}
}

func completedPublicationHandler(t *testing.T, kind string) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetBackground(func(func()) {})
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 2507, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	branch := "agent/issue-7"
	if kind == "mr_rework" {
		branch = "agent/rework-7"
		cliMustExec(t, pool, "UPDATE runs SET kind='mr_rework',pipeline_ref=$2,mr_iid=17,target_run_id=$1,branch='agent/prior-poison' WHERE id=$1", run, branch)
	}
	if kind == "self_improve" {
		branch = "uzi/self-improve/" + run.String()
		cliMustExec(t, pool, "UPDATE runs SET kind='self_improve',branch='agent/prior-poison',pipeline_ref='agent/pipeline-poison' WHERE id=$1", run)
	}
	worker, hold := uuid.New(), uuid.New()
	token := "completion-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'completion',$3,'online',$4)", worker, owner, sum[:], []string{capability.RecoveryCompletedPublicationV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, owner, repo, run, worker)
	f := &completionRouteForge{t: t, branch: branch, settleFakeForge: settleFakeForge{head: settleHead, verdict: map[string]forge.Ancestry{settlePushed: forge.AncestryAncestor}}}
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
	if kind == "self_improve" || kind == "mr_rework" {
		var request map[string]any
		if err := json.Unmarshal(wire.Request, &request); err != nil {
			t.Fatal(err)
		}
		request["branch"] = "agent/worker-poison"
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		body = string(encoded)
	}
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
	if receipt.HoldID != hold.String() || receipt.RunID != run.String() || receipt.WorkerID != worker.String() || receipt.OwnerID != owner.String() || receipt.Generation != 1 || receipt.FinalHead != settlePushed || receipt.ObservedBranchHead != settleHead || receipt.RepoID != repo.String() || receipt.ConnectionID != conn.String() || receipt.Branch != branch || receipt.MRIID == nil || *receipt.MRIID != 17 || f.compareCalls != 1 {
		t.Fatalf("exact ACK authority: %+v comparison calls=%d", receipt, f.compareCalls)
	}
	expected := wire.Ack.Receipt
	expected.Branch = branch
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
	for _, phase := range []string{"original", "changed_status_claim", "deleted"} {
		t.Run(phase, func(t *testing.T) {
			switch phase {
			case "changed_status_claim":
				cliMustExec(t, pool, "UPDATE runs SET status='failed',claim_generation=2,worker_id=NULL WHERE id=$1", run)
			case "deleted":
				n, err := store.New(pool).DeleteForgeConnectionForUser(context.Background(), store.DeleteForgeConnectionForUserParams{ID: conn, UserID: owner})
				if err != nil || n != 1 {
					t.Fatalf("delete released connection: rows=%d err=%v", n, err)
				}
				var exists bool
				if err := pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)", run).Scan(&exists); err != nil || exists {
					t.Fatalf("run deletion not demonstrated: exists=%v err=%v", exists, err)
				}
			}
			replay := call(token, body)
			var replayAck struct {
				Run                         apitypes.RunDTO                       `json:"run"`
				CompletedPublicationReceipt *apitypes.CompletedPublicationReceipt `json:"completed_publication_receipt"`
			}
			if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &replayAck) != nil {
				t.Fatalf("exact authenticated receipt replay: %d %s", replay.Code, replay.Body.String())
			}
			a, _ := json.Marshal(receipt)
			b, _ := json.Marshal(replayAck.CompletedPublicationReceipt)
			if string(a) != string(b) || replayAck.Run.ID != run.String() || replayAck.Run.Status != "completed" || replayAck.Run.WorkerID == nil || *replayAck.Run.WorkerID != worker.String() || replayAck.Run.Kind != "" {
				t.Fatalf("receipt/frozen completed ACK differs: %s", replay.Body.String())
			}
			var changedRequest map[string]any
			if err := json.Unmarshal([]byte(body), &changedRequest); err != nil {
				t.Fatal(err)
			}
			changedRequest["head"] = settleOther
			wrongHead, _ := json.Marshal(changedRequest)
			if wrong := call(token, string(wrongHead)); strings.Contains(wrong.Body.String(), "completed_publication_receipt") {
				t.Fatalf("mismatched request head received receipt: %s", wrong.Body.String())
			}
			if f.compareCalls != 1 {
				t.Fatal("stored replay consulted forge")
			}
		})
	}
	changed := strings.Replace(body, settlePushed, settleOther, 1)
	if wrong := call(token, changed); strings.Contains(wrong.Body.String(), "completed_publication_receipt") {
		t.Fatalf("changed head received receipt: %s", wrong.Body.String())
	}
}
