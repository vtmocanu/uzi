package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The real application router and worker middleware must reach the transaction.
// Owner consent remains unavailable in this deliberately non-ready M1 slice.
func TestPolicyHoldWorkerRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	ctx := context.Background()
	owner := cliSeedUser(t, pool, false)
	repo := cliSeedOwnedRepo(t, pool, owner)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := h.q.CreateWorker(ctx, store.CreateWorkerParams{UserID: owner, Name: "policy-fixture", TokenHash: hash, AnthropicBindMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker.ID, []string{capability.ProviderPolicyHoldV1})
	t.Cleanup(func() {
		_, e := pool.Exec(context.Background(), "DELETE FROM users WHERE id=$1", owner)
		if e != nil {
			t.Error(e)
		}
	})
	run := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,harness,worker_id,claim_generation,session_id) VALUES($1,$2,$3,3607,'policy fixture','desc','issue','running','codex',$4,1,'original-session')`, run, owner, repo, worker.ID)
	tag, phase, corr := "cyberPolicy", "implementation", "pr-00000000000000000000000000000000-1"
	req := workersvc.PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 1, ClaimGeneration: 1, Detail: "refused", ContinuationMode: "same_conversation", PolicyTag: &tag, Phase: &phase, CorrelationID: &corr}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/worker/runs/" + run.String() + "/policy-hold"
	if r := bearerReqBody(router, http.MethodPost, path, "", string(b)); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", r.Code)
	}
	for _, want := range []string{"applied", "already_applied"} {
		r := bearerReqBody(router, http.MethodPost, path, token, string(b))
		if r.Code != http.StatusOK {
			t.Fatalf("worker report = %d: %s", r.Code, r.Body.String())
		}
		var reply workersvc.PolicyHoldReply
		if err := json.Unmarshal(r.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Disposition != want || reply.OutcomeID != req.OutcomeID || reply.HoldReason != "codex_policy_hold" {
			t.Fatalf("report %+v, want %s", reply, want)
		}
	}
	row, err := h.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: run, UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "paused" || !row.PolicyPreserveRequired || row.PolicyRefusalCount != 1 || row.SessionID.String != "original-session" || row.ClaimReleasedAt.Valid {
		t.Fatal("report/replay changed hold preservation")
	}
	bad := string(b[:len(b)-1]) + ",\"execution\":{}}"
	if r := bearerReqBody(router, http.MethodPost, path, token, bad); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", r.Code)
	}
	other := cliSeedUser(t, pool, false)
	t.Cleanup(func() {
		_, e := pool.Exec(context.Background(), "DELETE FROM users WHERE id=$1", other)
		if e != nil {
			t.Error(e)
		}
	})
	otherToken, otherHash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	otherWorker, err := h.q.CreateWorker(ctx, store.CreateWorkerParams{UserID: other, Name: "foreign-policy-fixture", TokenHash: otherHash, AnthropicBindMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", otherWorker.ID, []string{capability.ProviderPolicyHoldV1})
	r := bearerReqBody(router, http.MethodPost, path, otherToken, string(b))
	if r.Code != http.StatusConflict {
		t.Fatalf("foreign worker = %d: %s", r.Code, r.Body.String())
	}
	var reply workersvc.PolicyHoldReply
	if err := json.Unmarshal(r.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Disposition != "pending" || reply.ObservedStatus != "" {
		t.Fatal("foreign worker learned run state")
	}
}
