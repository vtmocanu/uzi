package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerMemoryRoutesLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	wid := fx.onlineWorker("memory HTTP", false)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE workers SET snapshot_register_nonce='http-nonce',protocol_capabilities=$2 WHERE id=$1", wid, []string{capability.WorkerMemoryPressureV1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", id, wid); err != nil {
		t.Fatal(err)
	}
	worker, err := fx.q.GetWorkerByID(fx.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{})
	svc.SetTxBeginner(fx.pool)
	h := &Handler{q: fx.q, wsvc: svc}
	req := workersvc.MemoryReservationRequest{MemoryBinding: workersvc.MemoryBinding{
		RunID: id, WorkerID: wid, RegisterNonce: "http-nonce", ClaimGeneration: 1, InterventionID: uuid.New()},
		Policy: workersvc.MemoryPolicy{Version: 1, MaxInterventions: 1}}
	send := func(lane string, body any, want int) workersvc.MemoryReservation {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := memoryHTTP(h, &worker, id.String(), lane, string(raw))
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", lane, rec.Code, want, rec.Body.String())
		}
		var result workersvc.MemoryReservation
		if want == http.StatusOK {
			if err = json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	missing := req
	missing.Policy = workersvc.MemoryPolicy{}
	send("reserve", missing, http.StatusBadRequest)
	stale := req
	stale.RegisterNonce = "stale"
	send("reserve", stale, http.StatusConflict)
	foreign := newEphemeralFixture(t, false).queuedRun([]string{})
	foreignReq := req
	foreignReq.RunID = foreign
	raw, _ := json.Marshal(foreignReq)
	if rec := memoryHTTP(h, &worker, foreign.String(), "reserve", string(raw)); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign=%d %s", rec.Code, rec.Body.String())
	}
	if _, err = fx.pool.Exec(fx.ctx, "UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", wid); err != nil {
		t.Fatal(err)
	}
	send("reserve", req, http.StatusConflict)
	if _, err = fx.pool.Exec(fx.ctx, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wid, []string{capability.WorkerMemoryPressureV1}); err != nil {
		t.Fatal(err)
	}
	reserved := send("reserve", req, http.StatusOK)
	if !reserved.Authorizing || reserved.Policy != req.Policy {
		t.Fatalf("reservation=%+v", reserved)
	}
	outcome := workersvc.MemoryOutcomeRequest{MemoryBinding: req.MemoryBinding, Outcome: "confirmed_drained"}
	settled := send("outcome", outcome, http.StatusOK)
	if settled.Authorizing || settled.Outcome != "confirmed_drained" {
		t.Fatalf("outcome=%+v", settled)
	}
	if retry := send("reserve", req, http.StatusOK); retry.Authorizing || retry.Outcome != "confirmed_drained" {
		t.Fatalf("revived=%+v", retry)
	}
	conflict := outcome
	conflict.Outcome = "unknown"
	send("outcome", conflict, http.StatusConflict)
	next := req
	next.InterventionID = uuid.New()
	if denied := send("reserve", next, http.StatusOK); denied.Authorizing || denied.Admitted {
		t.Fatalf("denied=%+v", denied)
	}
}
