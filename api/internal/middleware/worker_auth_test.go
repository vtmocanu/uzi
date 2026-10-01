package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// Issue #1989: only a missing token row is a credential rejection. A store failure
// (a database outage behind a live api) is a retryable 503, so the worker rides it out
// instead of failing its run; an unknown token and a hash mismatch stay 401.
func TestRequireWorkerLookupFailureStatus(t *testing.T) {
	_, otherHash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cases := []struct {
		name string
		st   WorkerStore
		want int
	}{
		{"store failure is retryable", errWorkerStore{err: errors.New("dial tcp: connect: connection refused")}, http.StatusServiceUnavailable},
		{"context.Canceled lookup error on a live request returns 503", errWorkerStore{err: fmt.Errorf("lookup: %w", context.Canceled)}, http.StatusServiceUnavailable},
		{"wrapped no-rows is unauthorized", errWorkerStore{err: fmt.Errorf("lookup: %w", pgx.ErrNoRows)}, http.StatusUnauthorized},
		{"hash mismatch is unauthorized", rowWorkerStore{worker: store.Worker{ID: uuid.New(), TokenHash: otherHash}}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("next handler reached on a failed worker lookup")
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", nil)
			req.Header.Set("Authorization", "Bearer uzw_whatever")
			rec := httptest.NewRecorder()
			RequireWorker(tc.st)(next).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A store failure on a live request is an outage and is logged, even when the error
// itself unwraps to context.DeadlineExceeded (a database dial or pool timeout). A
// request whose own context is already done is the client going away: still a 503,
// but not logged.
func TestRequireWorkerLogsStoreFailureOnlyForLiveRequests(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dialTimeout := errWorkerStore{err: fmt.Errorf("dial tcp: i/o timeout: %w", context.DeadlineExceeded)}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	serve := func(ctx context.Context) int {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/worker/heartbeat", nil)
		req.Header.Set("Authorization", "Bearer uzw_whatever")
		rec := httptest.NewRecorder()
		RequireWorker(dialTimeout)(next).ServeHTTP(rec, req)
		return rec.Code
	}

	if code := serve(context.Background()); code != http.StatusServiceUnavailable {
		t.Fatalf("live request: status = %d, want 503", code)
	}
	if !strings.Contains(buf.String(), "worker auth: token lookup failed") {
		t.Fatalf("live request: store failure not logged; log = %q", buf.String())
	}

	buf.Reset()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if code := serve(cancelled); code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled request: status = %d, want 503", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("cancelled request: logged %q, want nothing", buf.String())
	}
}

// rowWorkerStore returns its row for any hash, so the middleware's constant-time
// re-check is the only thing standing between a mismatched row and the handler.
type rowWorkerStore struct{ worker store.Worker }

func (r rowWorkerStore) GetWorkerByTokenHash(context.Context, []byte) (store.Worker, error) {
	return r.worker, nil
}

type errWorkerStore struct{ err error }

func (e errWorkerStore) GetWorkerByTokenHash(context.Context, []byte) (store.Worker, error) {
	return store.Worker{}, e.err
}
