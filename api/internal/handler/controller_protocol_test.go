package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const testControllerToken = "uzc_test-controller-credential"

// newHandlerTestBox builds a secretbox with a varied (non-placeholder) key.
func newHandlerTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

func newControllerHandler(t *testing.T, st hostedsvc.Store, enabled bool) *Handler {
	t.Helper()
	box := newHandlerTestBox(t)
	h := &Handler{cfg: config.Config{
		WorkerHostingEnabled:  enabled,
		ControllerTokenSHA256: jointoken.Hash(testControllerToken),
	}}
	if st != nil {
		h.SetHostedSvc(hostedsvc.New(st, box))
	}
	return h
}

// pollStore is the fake the route tests poll against.
type pollStore struct {
	marked []uuid.UUID
	rows   []store.ListHostedWorkersForControllerRow
	// cordoned records the ids passed to CordonHostedWorker, in order.
	cordoned []uuid.UUID
	// cordonRows is what CordonHostedWorker returns (rows affected), letting a
	// contract test drive 204 (>0) vs 404 (0). cordonErr forces the 500 path.
	cordonRows int64
	cordonErr  error
}

func (p *pollStore) ListHostedWorkersForController(context.Context) ([]store.ListHostedWorkersForControllerRow, error) {
	return p.rows, nil
}
func (p *pollStore) UpsertHostedWorkerToken(context.Context, store.UpsertHostedWorkerTokenParams) error {
	return nil
}
func (p *pollStore) MarkHostedWorkerTokenDelivered(_ context.Context, arg store.MarkHostedWorkerTokenDeliveredParams) (int64, error) {
	p.marked = append(p.marked, arg.WorkerID)
	return 1, nil
}
func (p *pollStore) CordonHostedWorker(_ context.Context, id uuid.UUID) (int64, error) {
	p.cordoned = append(p.cordoned, id)
	return p.cordonRows, p.cordonErr
}

// routes builds the real router so these tests exercise the actual mounting +
// middleware, not a hand-wired handler.
func controllerRoutes(h *Handler) http.Handler {
	noLimit := mw.NewLimiter(1000, time.Minute, nil)
	return h.Routes(noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit, noLimit)
}

// The zero-behavior-change criterion at the router: with hosting off the
// controller endpoint does not exist at all — not 401, not 503. A compose stack's
// router is exactly the one it was before this PRD.
func TestControllerRouteIsNotMountedWhenHostingIsDisabled(t *testing.T) {
	h := newControllerHandler(t, nil, false)
	req := httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (the route must not exist with hosting off)", rec.Code)
	}
}

func TestControllerPollRequiresTheControllerToken(t *testing.T) {
	h := newControllerHandler(t, &pollStore{}, true)
	router := controllerRoutes(h)

	for _, tc := range []struct{ name, authz string }{
		{"no header", ""},
		{"wrong token", "Bearer uzc_wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// The endpoint end to end: an authenticated GET returns desired state, asserts
// nothing, and forbids caching (the body carries join-token plaintext).
func TestControllerPollReturnsDesiredStateAndForbidsCaching(t *testing.T) {
	st := &pollStore{}
	h := newControllerHandler(t, st, true)

	req := httptest.NewRequest(http.MethodGet, "/api/controller/poll", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (the body carries join-token plaintext)", got)
	}
	// A poll must never settle delivery: only a worker's registration does.
	if len(st.marked) != 0 {
		t.Fatalf("the poll marked %v as delivered; a poll is a pure read", st.marked)
	}
	var resp hostedsvc.PollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Workers == nil {
		t.Fatal("workers must marshal as [] rather than null")
	}
}

// The poll is a GET; the POST the ack once needed is gone and must not linger.
func TestControllerPollRejectsPost(t *testing.T) {
	h := newControllerHandler(t, &pollStore{}, true)
	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// --- cordon control-write (PRD #422 M4) ------------------------------------

// The cordon endpoint is a CONTROL-WRITE and must sit behind RequireController: no
// token, or the wrong token, is 401 and never reaches the store.
func TestControllerCordonRequiresTheControllerToken(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct{ name, authz string }{
		{"no header", ""},
		{"wrong token", "Bearer uzc_wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &pollStore{cordonRows: 1}
			h := newControllerHandler(t, st, true)
			req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/"+id.String()+"/drain", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			controllerRoutes(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if len(st.cordoned) != 0 {
				t.Fatalf("cordon reached the store despite failed auth: %v", st.cordoned)
			}
		})
	}
}

// A valid token cordoning a known hosted worker: 204, and the store was asked to
// cordon exactly that uuid.
func TestControllerCordonMarksAKnownWorkerDraining(t *testing.T) {
	id := uuid.New()
	st := &pollStore{cordonRows: 1}
	h := newControllerHandler(t, st, true)

	req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/"+id.String()+"/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if len(st.cordoned) != 1 || st.cordoned[0] != id {
		t.Fatalf("cordoned = %v, want exactly [%s]", st.cordoned, id)
	}
}

// An unknown worker (store reports 0 rows affected): 404.
func TestControllerCordonUnknownWorkerIs404(t *testing.T) {
	id := uuid.New()
	st := &pollStore{cordonRows: 0}
	h := newControllerHandler(t, st, true)

	req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/"+id.String()+"/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown worker", rec.Code)
	}
	if len(st.cordoned) != 1 {
		t.Fatalf("cordon should still query the store once; cordoned = %v", st.cordoned)
	}
}

// A malformed workerID path segment: 400, and the store is never touched.
func TestControllerCordonMalformedWorkerIDIs400(t *testing.T) {
	st := &pollStore{cordonRows: 1}
	h := newControllerHandler(t, st, true)

	req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/not-a-uuid/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed worker id", rec.Code)
	}
	if len(st.cordoned) != 0 {
		t.Fatalf("a malformed id must not reach the store: %v", st.cordoned)
	}
}

// A store error surfaces as 500, not a spurious 404.
func TestControllerCordonStoreErrorIs500(t *testing.T) {
	id := uuid.New()
	st := &pollStore{cordonErr: errors.New("db exploded")}
	h := newControllerHandler(t, st, true)

	req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/"+id.String()+"/drain", nil)
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on a store error", rec.Code)
	}
}
