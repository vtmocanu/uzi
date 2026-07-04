package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeWorkerStore matches a token hash against one registered worker.
type fakeWorkerStore struct {
	wantHash []byte
	worker   store.Worker
}

func (f *fakeWorkerStore) GetWorkerByTokenHash(_ context.Context, tokenHash []byte) (store.Worker, error) {
	if f.wantHash != nil && bytes.Equal(tokenHash, f.wantHash) {
		return f.worker, nil
	}
	return store.Worker{}, pgx.ErrNoRows
}

func TestRequireWorkerAcceptsValidToken(t *testing.T) {
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	wantID := uuid.New()
	// The stored row carries the same hash (SELECT * in the real query), which
	// the middleware re-checks in constant time.
	ws := &fakeWorkerStore{wantHash: hash, worker: store.Worker{ID: wantID, TokenHash: hash}}

	var gotID uuid.UUID
	h := RequireWorker(ws)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wkr, ok := WorkerFromContext(r.Context())
		if !ok {
			t.Error("worker not in context")
		}
		gotID = wkr.ID
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotID != wantID {
		t.Fatalf("context worker id = %s, want %s", gotID, wantID)
	}
}

func TestRequireWorkerRejectsMissingAndBadTokens(t *testing.T) {
	_, hash, _ := jointoken.Generate()
	st := &fakeWorkerStore{wantHash: hash, worker: store.Worker{ID: uuid.New()}}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireWorker(st)

	cases := []struct{ name, authz string }{
		{"no header", ""},
		{"wrong scheme", "Basic abc"},
		{"unknown token", "Bearer uzw_this-token-was-never-issued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()
			h(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// The lookup must not leak whether a token exists via a different error type.
func TestRequireWorkerTreatsAllLookupFailuresAsUnauthorized(t *testing.T) {
	st := errWorkerStore{err: errors.New("db exploded")}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer uzw_whatever")
	rec := httptest.NewRecorder()
	RequireWorker(st)(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

type errWorkerStore struct{ err error }

func (e errWorkerStore) GetWorkerByTokenHash(context.Context, []byte) (store.Worker, error) {
	return store.Worker{}, e.err
}
