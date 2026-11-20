package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416 M2: GET /api/worker/runs/{id}/run-env-redact. The service-level ownership and shape rules
// have LiveDB tests in workersvc; these pin the handler's status and header mapping.

type redactStore struct {
	*protocolStore
	rows []store.ListRunEnvSecretReceiptVersionsForRunRow
}

func (s *redactStore) ListRunEnvSecretReceiptVersionsForRun(context.Context, uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error) {
	return s.rows, nil
}

func redactReq(wkr store.Worker, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/"+runID.String()+"/run-env-redact", nil)
	ctx := mw.ContextWithWorker(req.Context(), wkr)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

func TestWorkerRunEnvRedactStatuses(t *testing.T) {
	wkr := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	run := store.Run{ID: uuid.New(), UserID: wkr.UserID, WorkerID: pgconv.UUID(wkr.ID)}
	repo := uuid.New()
	row := func(mutate func(*store.ListRunEnvSecretReceiptVersionsForRunRow)) store.ListRunEnvSecretReceiptVersionsForRunRow {
		r := store.ListRunEnvSecretReceiptVersionsForRunRow{
			VarID: uuid.New(), Rev: 1, Scope: "repo", Name: "SECRET_ONE", RepoID: pgtype.UUID{Bytes: repo, Valid: true},
			Ciphertext: []byte("sealed"), SealedWith: pgtype.Text{String: "dek", Valid: true}, AadVersion: pgtype.Int2{Int16: 2, Valid: true},
		}
		mutate(&r)
		return r
	}
	cases := []struct {
		name     string
		owned    store.Run
		ownedErr error
		rows     []store.ListRunEnvSecretReceiptVersionsForRunRow
		wantCode int
		wantBody string
	}{
		{"no secret receipts", run, nil, nil, http.StatusOK, `"protected":false`},
		{"unknown or deleted run", store.Run{}, pgx.ErrNoRows, nil, http.StatusNotFound, "run not found"},
		{"run of another user", store.Run{ID: run.ID, UserID: uuid.New(), WorkerID: run.WorkerID}, nil, nil, http.StatusNotFound, "run not found"},
		{"vault locked", run, nil, []store.ListRunEnvSecretReceiptVersionsForRunRow{row(func(*store.ListRunEnvSecretReceiptVersionsForRunRow) {})}, http.StatusConflict, "vault_locked"},
		{"receipted version missing", run, nil, []store.ListRunEnvSecretReceiptVersionsForRunRow{row(func(r *store.ListRunEnvSecretReceiptVersionsForRunRow) { r.Ciphertext = nil })}, http.StatusConflict, "run_env_history_unavailable"},
		{"receipted version does not open", run, nil, []store.ListRunEnvSecretReceiptVersionsForRunRow{row(func(r *store.ListRunEnvSecretReceiptVersionsForRunRow) {
			r.SealedWith = pgtype.Text{String: "unknown-marker", Valid: true}
		})}, http.StatusConflict, "run_env_history_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := &protocolStore{ownedRun: tc.owned, ownedErr: tc.ownedErr}
			h := newProtocolHandler(t, &redactStore{protocolStore: ps, rows: tc.rows})
			rec := httptest.NewRecorder()
			h.WorkerRunEnvRedact(rec, redactReq(wkr, run.ID))
			if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("status %d body %q, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestWorkerRunEnvRedactRequiresAWorker(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/x/run-env-redact", nil)
	rec := httptest.NewRecorder()
	h.WorkerRunEnvRedact(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d Cache-Control %q, want 401 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
}

// TestWorkerClaimIsNeverCached: a claim response can carry repo variable values, so every answer of
// the claim route (idle 204, refusals, errors) is no-store. The 200 is written by the same function
// after the same first statement; building a successful claim needs the full claim fixture, which the
// workersvc LiveDB tests own, so the 200 is not driven here.
func TestWorkerClaimIsNeverCached(t *testing.T) {
	t.Run("idle 204", func(t *testing.T) {
		h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
		rec := httptest.NewRecorder()
		h.WorkerClaim(rec, workerReq(http.MethodPost, "", uuid.Nil))
		if rec.Code != http.StatusNoContent || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d Cache-Control %q, want 204 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})
	t.Run("claim error", func(t *testing.T) {
		h := newProtocolHandler(t, &protocolStore{claimErr: context.DeadlineExceeded})
		rec := httptest.NewRecorder()
		h.WorkerClaim(rec, workerReq(http.MethodPost, "", uuid.Nil))
		if rec.Code != http.StatusInternalServerError || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d Cache-Control %q, want 500 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})
	t.Run("bad lane", func(t *testing.T) {
		h := newProtocolHandler(t, &protocolStore{})
		req := workerReq(http.MethodPost, "", uuid.Nil)
		req.URL.RawQuery = "lane=nope"
		rec := httptest.NewRecorder()
		h.WorkerClaim(rec, req)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d Cache-Control %q, want 400 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})
}
