package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func exactHoldRoute(h *Handler, worker store.Worker, run uuid.UUID, query string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/runs/{id}/recovery-holds", func(w http.ResponseWriter, req *http.Request) {
		h.WorkerListRecoveryHolds(w, req.WithContext(mw.ContextWithWorker(req.Context(), worker)))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/"+run.String()+"/recovery-holds"+query, nil))
	return rec
}

func TestExactRecoveryHoldRouteLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	worker := e.worker
	worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1, "recovery_held_publication_v2", capability.RecoveryInventoryV1}
	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE runs SET worker_id=$2, claim_generation=1 WHERE id=$1", e.run, worker.ID)
	observe := func(w store.Worker, query string) map[string]any {
		t.Helper()
		rec := exactHoldRoute(h, w, e.run, query)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := observe(worker, ""); body["exact_hold"] != nil {
		t.Fatalf("default widened: %v", body)
	}
	body := observe(worker, "?generation=1")
	exact, ok := body["exact_hold"].(map[string]any)
	if !ok || exact["hold_id"] != e.holdID.String() || exact["repo_id"] != e.repo.String() || exact["state"] != "open" || exact["current_claim_generation"] != float64(1) {
		t.Fatalf("exact guarded open observation missing: %v", body)
	}
	foreign := worker
	foreign.ID = uuid.New()
	foreignUser := worker
	foreignUser.UserID = uuid.New()
	lane := worker
	lane.IsolatedLane = true
	for _, w := range []store.Worker{foreign, foreignUser, lane} {
		if body := observe(w, "?generation=1"); body["exact_hold"] != nil {
			t.Fatalf("foreign scope disclosed: %v", body)
		}
	}
	for _, query := range []string{"?generation=0", "?generation=-1", "?generation=x", "?generation=2", "?generation=1&generation=2"} {
		if body := observe(worker, query); body["exact_hold"] != nil {
			t.Fatalf("invalid/missing generation: %v", body)
		}
	}
	v1 := worker
	v1.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
	if body := observe(v1, "?generation=1"); body["exact_hold"] != nil || len(body["holds"].([]any)) != 1 {
		t.Fatalf("V1 default changed: %v", body)
	}
	exec("UPDATE runs SET worker_id=NULL, claim_generation=2, status='failed' WHERE id=$1", e.run)
	if body := observe(worker, "?generation=1"); body["exact_hold"] == nil {
		t.Fatalf("original custody lost: %v", body)
	}
	exec("UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL, release_evidence='owner_discard', released_at=now() WHERE id=$1", e.holdID)
	body = observe(worker, "?generation=1")
	exact, ok = body["exact_hold"].(map[string]any)
	if !ok || exact["state"] != "discarded" || exact["release_evidence"] != "owner_discard" || exact["final_receipt"] != nil {
		t.Fatalf("closed observation fabricated FINAL: %v", body)
	}
	if body := observe(worker, ""); len(body["holds"].([]any)) != 0 {
		t.Fatalf("default closed list = %v", body)
	}
}

// Saved FINAL is observation only, even with the publication write feature switched off.
func TestExactRecoveryHoldFinalReceiptRouteLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	worker := e.worker
	worker.ProtocolCapabilities = []string{"recovery_held_publication_v2"}
	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET worker_id=$2, claim_generation=1, status='failed' WHERE id=$1", e.run, worker.ID); err != nil {
		t.Fatal(err)
	}
	q := store.New(e.pool)
	pub, err := q.UpsertHeldPublication(e.ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: 1, HoldID: e.holdID, UserID: e.user, RepoID: e.repo, WorkerID: worker.ID,
		Ref: "refs/uzi-held/" + e.run.String() + "/1", Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.MarkHeldCreateInvoked(e.ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = q.RecordHeldCreateOutcome(e.ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: pub.ID}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ReleaseHeldPublicationHold(e.ctx, store.ReleaseHeldPublicationHoldParams{
		ID: e.holdID, RunID: e.run, UserID: e.user, Generation: 1, WorkerID: worker.ID, PublicationID: pub.ID, Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	}); err != nil || n != 1 {
		t.Fatalf("release=%d %v", n, err)
	}
	rec := exactHoldRoute(h, worker, e.run, "?generation=1")
	var body struct {
		ExactHold *struct {
			State           string `json:"state"`
			ReleaseEvidence string `json:"release_evidence"`
			FinalReceipt    *struct {
				PublicationID string `json:"publication_id"`
			} `json:"final_receipt"`
		} `json:"exact_hold"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body.ExactHold == nil || body.ExactHold.State != "released" || body.ExactHold.ReleaseEvidence != "held_publication" || body.ExactHold.FinalReceipt == nil || body.ExactHold.FinalReceipt.PublicationID != pub.ID.String() {
		t.Fatalf("saved FINAL missing: %d %s", rec.Code, rec.Body)
	}
	var publicationState string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM run_held_publications WHERE id=$1", pub.ID).Scan(&publicationState); err != nil {
		t.Fatal(err)
	}
	if publicationState != "created" {
		t.Fatalf("observation acknowledged publication: %s", publicationState)
	}
}
