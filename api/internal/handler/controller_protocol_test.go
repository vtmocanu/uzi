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

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

const testControllerToken = "uzc_test-controller-credential"

func newControllerHandler(t *testing.T, st hostedsvc.Store, enabled bool) *Handler {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
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
	acked []uuid.UUID
	rows  []store.ListHostedWorkersForControllerRow
}

func (p *pollStore) ListHostedWorkersForController(context.Context) ([]store.ListHostedWorkersForControllerRow, error) {
	return p.rows, nil
}
func (p *pollStore) CreateHostedWorkerToken(context.Context, store.CreateHostedWorkerTokenParams) error {
	return nil
}
func (p *pollStore) DeleteHostedWorkerToken(_ context.Context, id uuid.UUID) (int64, error) {
	p.acked = append(p.acked, id)
	return 1, nil
}

// routes builds the real router so these tests exercise the actual mounting +
// middleware, not a hand-wired handler.
func controllerRoutes(h *Handler) http.Handler {
	noLimit := mw.NewLimiter(1000, time.Minute, nil)
	return h.Routes(noLimit, noLimit, noLimit, noLimit, noLimit, noLimit)
}

// The zero-behavior-change criterion at the router: with hosting off the
// controller endpoint does not exist at all — not 401, not 503. A compose stack's
// router is exactly the one it was before this PRD.
func TestControllerRouteIsNotMountedWhenHostingIsDisabled(t *testing.T) {
	h := newControllerHandler(t, nil, false)
	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", strings.NewReader(`{}`))
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
			req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", strings.NewReader(`{}`))
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

// The endpoint end to end: an authenticated poll acks and returns desired state.
func TestControllerPollAcksAndReturnsDesiredState(t *testing.T) {
	id := uuid.New()
	st := &pollStore{}
	h := newControllerHandler(t, st, true)

	body := `{"materialized":["` + id.String() + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(st.acked) != 1 || st.acked[0] != id {
		t.Fatalf("acked = %v, want the request's id", st.acked)
	}
	var resp hostedsvc.PollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Workers == nil {
		t.Fatal("workers must marshal as [] rather than null")
	}
}

// A malformed ack is a 400, not a 500: it is the caller's error, and silently
// skipping it would leave a token sealed forever with no signal.
func TestControllerPollRejectsAMalformedAck(t *testing.T) {
	h := newControllerHandler(t, &pollStore{}, true)
	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", strings.NewReader(`{"materialized":["nope"]}`))
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	controllerRoutes(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
