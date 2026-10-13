package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 step B through the worker route: a held FINAL posted to
// /api/worker/runs/{id}/archives/release reaches recovery.Service and answers JSON. The forge
// listing is not reachable from here, so these cases cover the answers that precede it: the
// shape gate, the exact-replay ACK, the receipt conflict and the capability retain. The listing
// and every locked branch are pinned in the recovery package.

const (
	heldRouteTip    = "3f2a9c41d8e75b60a1c4f2e9b7d03a58c6e1f4b2"
	heldRouteDigest = "9b74c9897bac770ffc029102a200c5de0a9b1e3d5f7c2a4b6d8e0f1a3c5e7b92"
)

func heldRouteRelease(t *testing.T, h *Handler, wkr store.Worker, run uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), wkr)))
		})
	}).Post("/api/worker/runs/{id}/archives/release", h.WorkerRecoveryRelease)
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/archives/release", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestWorkerRecoveryReleaseHeldFinalRouteLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	caps := []string{capability.RecoveryInventoryV1, capability.RecoveryHeldPublicationV1}
	exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, e.worker.ID, caps)
	exec(`UPDATE runs SET worker_id=$2, claim_generation=1 WHERE id=$1`, e.run, e.worker.ID)
	exec(`UPDATE runs SET status='failed' WHERE id=$1`, e.run)
	q := store.New(e.pool)
	pub, err := q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: 1, HoldID: e.holdID, UserID: e.user, RepoID: e.repo, WorkerID: e.worker.ID,
		Ref: pushbroker.HeldRef(e.run, 1), Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.MarkHeldCreateInvoked(ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: pub.ID}); err != nil {
		t.Fatal(err)
	}

	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	// No forge is wired (SetHeldPublication was never called), the production default for a
	// service built without the worker service.
	body := func(mutate func(*apitypes.RecoveryReleaseRequest)) apitypes.RecoveryReleaseRequest {
		g := int64(1)
		req := apitypes.RecoveryReleaseRequest{Generation: &g, FinalDisposition: &apitypes.RecoveryFinalDisposition{
			Kind: recovery.FinalKindHeldPublication, PublicationID: pub.ID.String(), SourceSha: heldRouteTip, CoverageDigest: heldRouteDigest,
		}}
		if mutate != nil {
			mutate(&req)
		}
		return req
	}
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
		}
		return m
	}
	worker := e.worker
	worker.ProtocolCapabilities = caps

	// A malformed identity is a 400 with the stable reason.
	rec := heldRouteRelease(t, h, worker, e.run, body(func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.PublicationID = "nope" }))
	if rec.Code != http.StatusBadRequest || decode(rec)["reason"] != "bad_request" {
		t.Fatalf("malformed = %d %s", rec.Code, rec.Body)
	}

	// Switch off and no forge: an open hold is retained as unsupported, a 200 and never a 400.
	rec = heldRouteRelease(t, h, worker, e.run, body(nil))
	if m := decode(rec); rec.Code != http.StatusOK || m["retained"] != true || m["reason"] != recovery.HeldReleaseUnsupported || m["released"] != false {
		t.Fatalf("open hold, unwired = %d %s", rec.Code, rec.Body)
	}
	if e.holdState() != "open" {
		t.Fatalf("hold = %s, want open", e.holdState())
	}

	// A hold released against the publication (what step B commits) is answered by the exact
	// replay, even for a worker whose capabilities are gone.
	if n, err := q.ReleaseHeldPublicationHold(ctx, store.ReleaseHeldPublicationHoldParams{
		ID: e.holdID, RunID: e.run, UserID: e.user, Generation: 1, WorkerID: e.worker.ID,
		PublicationID: pub.ID, Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	}); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if _, err := q.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{ID: pub.ID, RetentionSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	degraded := e.worker
	rec = heldRouteRelease(t, h, degraded, e.run, body(nil))
	m := decode(rec)
	if rec.Code != http.StatusOK || m["released"] != true || m["holds_released"] != float64(1) || m["generation"] != float64(1) || m["retained"] != nil {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body)
	}

	// A different identity under the same hold is a conflict.
	rec = heldRouteRelease(t, h, degraded, e.run, body(func(r *apitypes.RecoveryReleaseRequest) {
		r.FinalDisposition.CoverageDigest = strings.Repeat("1", 64)
	}))
	if rec.Code != http.StatusConflict || decode(rec)["reason"] != "manifest_conflict" {
		t.Fatalf("mismatch = %d %s", rec.Code, rec.Body)
	}

	// Another worker is refused.
	foreign := store.Worker{ID: uuid.New(), UserID: e.user, ProtocolCapabilities: caps}
	rec = heldRouteRelease(t, h, foreign, e.run, body(nil))
	if rec.Code != http.StatusForbidden || decode(rec)["reason"] != "not_authorized" {
		t.Fatalf("foreign worker = %d %s", rec.Code, rec.Body)
	}
}
