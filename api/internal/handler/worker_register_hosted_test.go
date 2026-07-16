package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// registerStore is a workersvc.Store that satisfies the register path and echoes
// back the Kind the test seeded, so the hosted-vs-external branch is exercised.
type registerStore struct {
	workersvc.Store
	kind string
}

func (r *registerStore) FailWorkerRunsOverCap(context.Context, store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *registerStore) RequeueWorkerRuns(context.Context, store.RequeueWorkerRunsParams) (int64, error) {
	return 0, nil
}
func (r *registerStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.Worker, error) {
	return store.Worker{ID: arg.ID, Status: "online", Kind: r.kind}, nil
}

// noteStore records NoteRegistered's reach into the hosted-token table.
type noteStore struct {
	hostedsvc.Store
	marked []uuid.UUID
	err    error
}

func (n *noteStore) MarkHostedWorkerTokenDelivered(_ context.Context, id uuid.UUID) (int64, error) {
	n.marked = append(n.marked, id)
	return 1, n.err
}

// registerReq builds an authenticated /worker/register request for a worker whose
// row carries the given kind.
func registerReq(workerID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(`{"version":"1.0.0"}`))
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: uuid.New()})
	return req.WithContext(ctx)
}

func newRegisterHandler(t *testing.T, kind string, ns hostedsvc.Store) *Handler {
	t.Helper()
	h := &Handler{wsvc: workersvc.New(&registerStore{kind: kind}, newHandlerTestBox(t), workersvc.Params{})}
	if ns != nil {
		h.SetHostedSvc(hostedsvc.New(ns, newHandlerTestBox(t)))
	}
	return h
}

// A hosted worker registering IS the delivery proof (PRD #58 Decision 3):
// RequireWorker only resolved it by matching sha256(the presented token) against
// workers.token_hash, so the api may now destroy its sealed buffer.
func TestRegisterMarksAHostedWorkersTokenDelivered(t *testing.T) {
	id := uuid.New()
	ns := &noteStore{}
	h := newRegisterHandler(t, "hosted", ns)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(id))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(ns.marked) != 1 || ns.marked[0] != id {
		t.Fatalf("marked = %v, want the registering worker's id", ns.marked)
	}
}

// An ordinary hand-run worker must never touch the hosted-token table: it has no
// row there, and the branch exists so the common path stays untouched.
func TestRegisterDoesNotTouchHostedTokensForAnExternalWorker(t *testing.T) {
	ns := &noteStore{}
	h := newRegisterHandler(t, "external", ns)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(ns.marked) != 0 {
		t.Fatalf("an external worker's register reached the hosted-token table: %v", ns.marked)
	}
}

// The cleanup is NON-FATAL. A worker that has already registered successfully must
// never be failed because the buffer could not be cleared — the TTL sweep is the
// backstop, and failing here would wedge a healthy worker's register-retry loop.
func TestRegisterSucceedsEvenWhenTheHostedCleanupFails(t *testing.T) {
	ns := &noteStore{err: errors.New("db exploded")}
	h := newRegisterHandler(t, "hosted", ns)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a cleanup failure must not fail the registration", rec.Code)
	}
}

// Hosting disabled means hsvc is nil. The register path must not panic on it — this
// is every compose stack.
func TestRegisterWithHostingDisabled(t *testing.T) {
	h := newRegisterHandler(t, "external", nil)
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
