package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSourceEndDeclinedMissingIdentityCancelLiveDB(t *testing.T) {
	for _, identity := range []string{"missing worker", "missing worker id", "missing released id", "missing released nonce", "empty released nonce"} {
		t.Run(identity, func(t *testing.T) {
			e := newRecoveryEnv(t)
			e.mustExec("UPDATE workers SET snapshot_register_nonce='fresh', last_heartbeat_at=now() WHERE id=$1", e.worker.ID)
			e.mustExec("UPDATE runs SET status='recovery_wait', worker_id=$2, claim_generation=1, claim_released_at=now(), released_worker_id=$2, released_worker_nonce='old', recovery_wait_cause='retained_recovery_adoption_failed', recovery_wait_detail='source_end_declined', started_at=now(), budget_wall_seconds=3600 WHERE id=$1", e.run, e.worker.ID)
			switch identity {
			case "missing worker":
				// Custody keeps its original identity after its live FK is detached.
				e.mustExec("UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1", e.holdID)
				e.mustExec("DELETE FROM workers WHERE id=$1", e.worker.ID)
			case "missing worker id":
				e.mustExec("UPDATE runs SET worker_id=NULL WHERE id=$1", e.run)
			case "missing released id":
				e.mustExec("UPDATE runs SET released_worker_id=NULL WHERE id=$1", e.run)
			case "missing released nonce":
				e.mustExec("UPDATE runs SET released_worker_nonce=NULL WHERE id=$1", e.run)
			case "empty released nonce":
				e.mustExec("UPDATE runs SET released_worker_nonce='' WHERE id=$1", e.run)
			}
			h := e.handler()
			h.cfg.RunTimeout = time.Hour
			h.wsvc.SetTxBeginner(e.pool)
			h.wsvc.SetBackground(func(func()) {})
			before := e.resumeSnapshot()
			row, err := h.q.GetRunByID(e.ctx, e.run)
			if err != nil {
				t.Fatal(err)
			}
			rec := recoveryResumeRequest(h, e.run, e.user)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "register again") || !strings.Contains(rec.Body.String(), "Cancel ends the run") {
				t.Fatalf("identity refusal=%d %s", rec.Code, rec.Body.String())
			}
			after, err := h.q.GetRunByID(e.ctx, e.run)
			if err != nil || !reflect.DeepEqual(row, after) || e.resumeSnapshot() != before {
				t.Fatalf("refused Resume changed row/audit/custody: %v", err)
			}
			var inputs int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", e.run).Scan(&inputs); err != nil || inputs != 0 {
				t.Fatalf("refused Resume inputs=%d err=%v", inputs, err)
			}
			rec = httptest.NewRecorder()
			h.CreateRunInput(rec, inputReq(store.User{ID: e.user, IsActive: true}, e.run, `{"kind":"cancel","body":"end source hold"}`))
			if rec.Code != http.StatusAccepted || e.runStatus() != "cancelled" || e.resumeSnapshot().Holds != before.Holds {
				t.Fatalf("Cancel after refusal=%d %s status=%s", rec.Code, rec.Body.String(), e.runStatus())
			}
			if rec := recoveryResumeRequest(h, e.run, e.user); rec.Code != http.StatusConflict {
				t.Fatalf("Resume reopened cancellation=%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSourceEndDeclinedStateAndResumeLiveDB(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			e := newRecoveryEnv(t)
			e.mustExec("UPDATE workers SET snapshot_register_nonce='source-nonce', last_heartbeat_at=now() WHERE id=$1", e.worker.ID)
			e.mustExec("UPDATE runs SET worker_id=$2, status=$3, claim_generation=1, registration_evidence_generation=1, registration_evidence_nonce='source-nonce', started_at=now(), budget_wall_seconds=3600 WHERE id=$1", e.run, e.worker.ID, status)
			h := e.handler()
			h.cfg.RunTimeout = time.Hour
			h.wsvc.SetTxBeginner(e.pool)
			h.wsvc.SetBackground(func(func()) {})
			post := func(body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", strings.NewReader(body))
				rc := chi.NewRouteContext()
				rc.URLParams.Add("id", e.run.String())
				ctx := context.WithValue(mw.ContextWithWorker(req.Context(), e.worker), chi.RouteCtxKey, rc)
				rec := httptest.NewRecorder()
				h.WorkerRunState(rec, req.WithContext(ctx))
				return rec
			}
			const body = `{"status":"recovery_wait","recovery_cause":"retained_recovery_adoption_failed","recovery_detail":"source_end_declined","claim_generation":1,"register_nonce":"source-nonce","session_id":"source\u0000-session","checkpoint_contains_latest":true}`
			if status == "claimed" {
				e.mustExec("UPDATE runs SET registration_evidence_generation=NULL, registration_evidence_nonce=NULL, plan_md='planned' WHERE id=$1", e.run)
				e.mustExec(`INSERT INTO semantic_planning(run_id, enrollment) VALUES ($1, '{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, e.run)
				rec := post(`{"status":"running","claim_generation":1,"registration_evidence":{"register_nonce":"source-nonce","claim_generation":1,"registration_epoch":0}}`)
				var evidence struct {
					Applied  bool   `json:"applied"`
					Evidence string `json:"registration_evidence"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &evidence); err != nil || rec.Code != http.StatusConflict || evidence.Applied || evidence.Evidence != "recorded" {
					t.Fatalf("guarded running report=%d %s (%v)", rec.Code, rec.Body.String(), err)
				}
				r, err := h.q.GetRunByID(e.ctx, e.run)
				if err != nil || r.Status != "claimed" || !r.RegistrationEvidenceGeneration.Valid || r.RegistrationEvidenceGeneration.Int64 != 1 || r.RegistrationEvidenceNonce.String != "source-nonce" {
					t.Fatalf("guarded evidence row=%+v err=%v", r, err)
				}
			}
			if rec := post(body); rec.Code != http.StatusOK {
				t.Fatalf("first hold=%d: %s", rec.Code, rec.Body.String())
			}
			read := func() store.Run {
				t.Helper()
				r, err := h.q.GetRunByID(e.ctx, e.run)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			held := read()
			if held.SessionID.String != "source-session" || !held.CheckpointContainsLatest.Valid || !held.CheckpointContainsLatest.Bool {
				t.Fatalf("hold lost request checkpoint/session: %+v", held)
			}
			before := e.resumeSnapshot()
			rec := post(body)
			var ack struct {
				Applied *bool `json:"applied"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || rec.Code != http.StatusOK || ack.Applied == nil || *ack.Applied {
				t.Fatalf("exact replay must ACK applied=false: %d %s (%v)", rec.Code, rec.Body.String(), err)
			}
			if !reflect.DeepEqual(read(), held) || e.resumeSnapshot() != before {
				t.Fatal("replay repeated effects")
			}
			if rec := recoveryResumeRequest(h, e.run, e.user); rec.Code != http.StatusConflict {
				t.Fatalf("unchanged registration Resume=%d: %s", rec.Code, rec.Body.String())
			}
			if e.resumeSnapshot() != before {
				t.Fatal("refused Resume changed audit/custody/counters")
			}
			for _, mutation := range []string{
				"UPDATE workers SET last_heartbeat_at=now()-interval '1 day' WHERE id=$1",
				"UPDATE runs SET released_worker_nonce=NULL WHERE worker_id=$1",
				"UPDATE runs SET released_worker_id=NULL WHERE worker_id=$1",
			} {
				e.mustExec(mutation, e.worker.ID)
				unchanged := e.resumeSnapshot()
				if rec := recoveryResumeRequest(h, e.run, e.user); rec.Code != http.StatusConflict {
					t.Fatalf("Resume after %s=%d: %s", mutation, rec.Code, rec.Body.String())
				}
				if e.resumeSnapshot() != unchanged {
					t.Fatal("refused Resume wrote effects")
				}
				e.mustExec("UPDATE runs SET released_worker_id=$2, released_worker_nonce='source-nonce' WHERE id=$1", e.run, e.worker.ID)
			}
			e.mustExec("UPDATE workers SET snapshot_register_nonce='fresh-source-nonce' WHERE id=$1", e.worker.ID)
			if rec := post(body); rec.Code != http.StatusConflict {
				t.Fatalf("stale registration report=%d: %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(read(), held) {
				t.Fatal("stale report changed row")
			}
			if rec := recoveryResumeRequest(h, e.run, e.user); rec.Code != http.StatusOK {
				t.Fatalf("distinct registration Resume=%d: %s", rec.Code, rec.Body.String())
			}
			if got := read(); got.Status != "queued" || got.ClaimGeneration != held.ClaimGeneration ||
				got.ReleasedWorkerID != held.ReleasedWorkerID || got.ReleasedWorkerNonce != held.ReleasedWorkerNonce {
				t.Fatalf("Resume bounds: %+v", got)
			}
			if rec := post(body); rec.Code != http.StatusConflict {
				t.Fatalf("old report after Resume=%d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
