package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestInterruptedCallsRouteDeadline(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		called := false
		handler := interruptedCallsDeadline(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > time.Second {
				t.Fatal("route reads lack a one-second deadline")
			}
			if cancelled && r.Context().Err() != context.Canceled {
				t.Fatal("route replaced parent cancellation")
			}
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		cancel()
		if !called {
			t.Fatal("deadline middleware skipped the handler")
		}
	}
}

func TestWorkerInterruptedCallsMalformed(t *testing.T) {
	h := &Handler{wsvc: workersvc.New(nil, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
	router := chi.NewRouter()
	router.Get("/runs/{id}/interrupted-calls", h.WorkerInterruptedCalls)
	for _, tc := range []struct {
		name, id, query, nonce string
		auth                   bool
		want                   int
	}{
		{"unauthenticated", uuid.New().String(), "?claim_generation=2", "nonce", false, 401},
		{"invalid path", "bad", "?claim_generation=2", "nonce", true, 400},
		{"missing generation", uuid.New().String(), "", "nonce", true, 400},
		{"first claim", uuid.New().String(), "?claim_generation=1", "nonce", true, 400},
		{"invalid generation", uuid.New().String(), "?claim_generation=abc", "nonce", true, 400},
		{"missing nonce", uuid.New().String(), "?claim_generation=2", "", true, 400},
		{"invalid nonce", uuid.New().String(), "?claim_generation=2", string([]byte{0xff}), true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/runs/"+tc.id+"/interrupted-calls"+tc.query, nil)
			req.Header.Set("X-Worker-Register-Nonce", tc.nonce)
			if tc.auth {
				req = req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New()}))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
