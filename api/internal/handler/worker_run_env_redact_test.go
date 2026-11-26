package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4416 M2: GET /api/worker/runs/{id}/run-env-redact, the route's mapping of the service
// answer. The ownership rules and the two 409 causes against a database are in workersvc.

type fakeRunEnvRedactor struct {
	res   workersvc.RunEnvRedactResponse
	err   error
	calls int
	gotW  uuid.UUID
	gotR  uuid.UUID
}

func (f *fakeRunEnvRedactor) RunEnvRedact(_ context.Context, wkr store.Worker, runID uuid.UUID) (workersvc.RunEnvRedactResponse, error) {
	f.calls++
	f.gotW, f.gotR = wkr.ID, runID
	return f.res, f.err
}

func redactReq(withWorker bool, wkr store.Worker, id string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/x/run-env-redact", nil)
	ctx := req.Context()
	if withWorker {
		ctx = mw.ContextWithWorker(ctx, wkr)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

func TestWorkerRunEnvRedactMapsTheServiceAnswer(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/claims/run-env-redact-response.json")
	if err != nil {
		t.Fatal(err)
	}
	var protected workersvc.RunEnvRedactResponse
	if err := json.Unmarshal(raw, &protected); err != nil {
		t.Fatal(err)
	}
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	runID := uuid.New()
	for _, tc := range []struct {
		name       string
		res        workersvc.RunEnvRedactResponse
		err        error
		wantStatus int
		wantReason string
		wantBody   string
	}{
		{"unprotected", workersvc.RunEnvRedactResponse{}, nil, http.StatusOK, "", `{"protected":false}`},
		{"protected", protected, nil, http.StatusOK, "", string(raw)},
		{"notOwned", workersvc.RunEnvRedactResponse{}, workersvc.ErrRunNotOwned, http.StatusNotFound, "run_env_run_not_found", ""},
		{"redactClosed", workersvc.RunEnvRedactResponse{}, workersvc.ErrRunEnvRedactClosed, http.StatusConflict, "run_env_redact_closed", ""},
		{"vaultLocked", workersvc.RunEnvRedactResponse{}, workersvc.ErrRunEnvVaultLocked, http.StatusConflict, "vault_locked", ""},
		{"historyUnavailable", workersvc.RunEnvRedactResponse{}, workersvc.ErrRunEnvHistoryUnavailable, http.StatusConflict, "run_env_history_unavailable", ""},
		{"internal", workersvc.RunEnvRedactResponse{}, errors.New("database exploded"), http.StatusInternalServerError, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRunEnvRedactor{res: tc.res, err: tc.err}
			h := &Handler{runEnvRedactSvc: fake}
			rec := httptest.NewRecorder()
			h.WorkerRunEnvRedact(rec, redactReq(true, wkr, runID.String()))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store on every answer", got)
			}
			if fake.gotW != wkr.ID || fake.gotR != runID {
				t.Fatalf("the service saw worker %s run %s", fake.gotW, fake.gotR)
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if tc.wantReason != "" && (body["reason"] != tc.wantReason || body["code"] != tc.wantReason) {
				t.Fatalf("error body = %s, want reason and code %q", rec.Body.String(), tc.wantReason)
			}
			if tc.wantBody != "" {
				var want any
				if err := json.Unmarshal([]byte(tc.wantBody), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(want, any(body)) {
					t.Fatalf("body = %s, want %s", rec.Body.String(), tc.wantBody)
				}
			}
			if tc.name == "internal" && rec.Body.String() != "" && json.Valid(rec.Body.Bytes()) {
				if body["error"] != "internal error" {
					t.Fatalf("an internal failure leaked %s", rec.Body.String())
				}
			}
		})
	}
}

func TestWorkerRunEnvRedactRefusesWithoutWorkerOrWithBadID(t *testing.T) {
	fake := &fakeRunEnvRedactor{}
	h := &Handler{runEnvRedactSvc: fake}
	rec := httptest.NewRecorder()
	h.WorkerRunEnvRedact(rec, redactReq(false, store.Worker{}, uuid.NewString()))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Cache-Control") != "no-store" || fake.calls != 0 {
		t.Fatalf("no worker: status=%d cache=%q calls=%d", rec.Code, rec.Header().Get("Cache-Control"), fake.calls)
	}
	rec = httptest.NewRecorder()
	h.WorkerRunEnvRedact(rec, redactReq(true, store.Worker{ID: uuid.New()}, "not-a-uuid"))
	if rec.Code != http.StatusBadRequest || fake.calls != 0 {
		t.Fatalf("bad id: status=%d calls=%d", rec.Code, fake.calls)
	}
}
