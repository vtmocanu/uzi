package handler

import (
	"bytes"
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
	// rotatedHash is what RegisterWorker's RETURNING * hands back — set to a
	// DIFFERENT value than the authenticated worker's to simulate a rotation
	// committing mid-request.
	rotatedHash []byte
}

func (r *registerStore) FailWorkerRunsOverCap(context.Context, store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *registerStore) RequeueWorkerRuns(context.Context, store.RequeueWorkerRunsParams) (int64, error) {
	return 0, nil
}

// RegisterWorker returns the POST-register row. rotatedHash simulates a rotation
// committing during this round trip: RETURNING * would carry the NEW hash, which is
// exactly the value the handler must NOT pass to NoteRegistered.
func (r *registerStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.Worker, error) {
	return store.Worker{ID: arg.ID, Status: "online", Kind: r.kind, TokenHash: r.rotatedHash}, nil
}

// noteStore records NoteRegistered's reach into the hosted-token table, including
// WHICH hash the handler passed — the load-bearing detail (it must be the one auth
// proved, never a post-register re-read).
type noteStore struct {
	hostedsvc.Store
	marked     []uuid.UUID
	provedHash []byte
	err        error
}

func (n *noteStore) MarkHostedWorkerTokenDelivered(_ context.Context, arg store.MarkHostedWorkerTokenDeliveredParams) (int64, error) {
	n.marked = append(n.marked, arg.WorkerID)
	n.provedHash = arg.ProvedTokenHash
	return 1, n.err
}

// registerReq builds an authenticated /worker/register request. authHash is the
// hash RequireWorker resolved this caller by — the proof.
func registerReq(workerID uuid.UUID, authHash []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(`{"version":"1.0.0"}`))
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: uuid.New(), TokenHash: authHash})
	return req.WithContext(ctx)
}

func newRegisterHandler(t *testing.T, kind string, ns hostedsvc.Store, rotatedHash []byte) *Handler {
	t.Helper()
	h := &Handler{wsvc: workersvc.New(&registerStore{kind: kind, rotatedHash: rotatedHash}, newHandlerTestBox(t), workersvc.Params{})}
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
	authHash := []byte("the-hash-auth-proved")
	ns := &noteStore{}
	h := newRegisterHandler(t, "hosted", ns, authHash)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(id, authHash))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if len(ns.marked) != 1 || ns.marked[0] != id {
		t.Fatalf("marked = %v, want the registering worker's id", ns.marked)
	}
	if !bytes.Equal(ns.provedHash, authHash) {
		t.Fatalf("proved hash = %q, want the hash auth resolved this caller by", ns.provedHash)
	}
}

// The handler must qualify the destroy with the hash AUTH PROVED, never with the
// one RegisterWorker's RETURNING * hands back — that is a fresh read which would
// already reflect a rotation committed during the round trip, so passing it would
// let this request destroy a token it never held. Here the two deliberately differ:
// the caller authenticated with authHash while the post-register row carries
// rotatedHash.
func TestRegisterQualifiesTheDestroyWithTheProvedHashNotThePostRegisterRead(t *testing.T) {
	authHash := []byte("hash-the-caller-actually-proved")
	rotatedHash := []byte("hash-a-rotation-committed-mid-flight")
	ns := &noteStore{}
	h := newRegisterHandler(t, "hosted", ns, rotatedHash)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New(), authHash))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if bytes.Equal(ns.provedHash, rotatedHash) {
		t.Fatal("the handler passed RegisterWorker's post-register hash; a mid-flight rotation would then destroy a token this request never proved")
	}
	if !bytes.Equal(ns.provedHash, authHash) {
		t.Fatalf("proved hash = %q, want the authenticated context's hash %q", ns.provedHash, authHash)
	}
}

// An ordinary hand-run worker must never touch the hosted-token table: it has no
// row there, and the branch exists so the common path stays untouched.
func TestRegisterDoesNotTouchHostedTokensForAnExternalWorker(t *testing.T) {
	ns := &noteStore{}
	h := newRegisterHandler(t, "external", ns, nil)

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New(), []byte("h")))

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
	h := newRegisterHandler(t, "hosted", ns, []byte("h"))

	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New(), []byte("h")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a cleanup failure must not fail the registration", rec.Code)
	}
}

// Hosting disabled means hsvc is nil. The register path must not panic on it — this
// is every compose stack.
func TestRegisterWithHostingDisabled(t *testing.T) {
	h := newRegisterHandler(t, "external", nil, nil)
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, registerReq(uuid.New(), []byte("h")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
