package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The controller and worker credentials are separate trust domains, and this file
// pins the separation from both sides.
//
// Why it matters: RequireWorker resolves its bearer token through
// GetWorkerByTokenHash with NO kind filter, so ANY row in `workers` whose hash
// matches authenticates the whole /api/worker/* surface — including
// POST /api/worker/runs/claim, whose response carries the owner's DECRYPTED forge
// PAT and Anthropic token. If the controller's credential were ever stored as a
// workers row (even one tagged kind='hosted'), the controller would silently gain
// the ability to claim runs and harvest those secrets. A `kind` column would not
// save it; only a separate credential domain does.
//
// M1's controller credential lives in config as a sha256 and touches no table at
// all, so the isolation is structural. These tests keep it that way: if a future
// change routes controller auth through the workers table, one of them fails.

// oneWorkerStore authenticates exactly one worker join token.
type oneWorkerStore struct {
	hash   []byte
	worker store.Worker
}

func (o *oneWorkerStore) GetWorkerByTokenHash(_ context.Context, tokenHash []byte) (store.Worker, error) {
	if bytes.Equal(tokenHash, o.hash) {
		return o.worker, nil
	}
	return store.Worker{}, pgx.ErrNoRows
}

// A controller credential must not authenticate the worker surface.
func TestControllerTokenIsRejectedByRequireWorker(t *testing.T) {
	const controllerToken = "uzc_the-controller-credential"
	// A fully populated worker store: some worker exists, just not one whose hash
	// is the controller's.
	workerToken, workerHash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	st := &oneWorkerStore{hash: workerHash, worker: store.Worker{ID: uuid.New(), TokenHash: workerHash}}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/claim", nil)
	req.Header.Set("Authorization", "Bearer "+controllerToken)
	rec := httptest.NewRecorder()
	RequireWorker(st)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: the controller credential must never reach the worker claim surface", rec.Code)
	}
	// Sanity: the store DOES authenticate a real worker token, so the 401 above is
	// the controller token being rejected, not a store that refuses everything.
	req = httptest.NewRequest(http.MethodPost, "/api/worker/runs/claim", nil)
	req.Header.Set("Authorization", "Bearer "+workerToken)
	rec = httptest.NewRecorder()
	RequireWorker(st)(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("control case: a real worker token got %d, want 200", rec.Code)
	}
}

// A worker join token must not authenticate the controller surface — otherwise
// any user could mint a worker and read every hosted worker's pending join token.
func TestWorkerTokenIsRejectedByRequireController(t *testing.T) {
	workerToken, _, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	const controllerToken = "uzc_the-controller-credential"

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/api/controller/poll", nil)
	req.Header.Set("Authorization", "Bearer "+workerToken)
	rec := httptest.NewRecorder()
	RequireController(jointoken.Hash(controllerToken))(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a worker token must never authenticate the controller poll", rec.Code)
	}
}
