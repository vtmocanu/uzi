package handler

import (
	"context"
	"encoding/json"
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
