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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// pendingOutcomeStore is a minimal workersvc.Store for the CreateRunInput cancel-confirmation test
// (PRD #1391 Run B M3d, D13): it owns one running run on a FRESH-heartbeat worker whose executor
// journaled a terminal outcome (RunHasPendingOutcomeLease → true), so hasLivePoller reads it as no
// live poller (the lease, not a stale heartbeat, is the discriminator). It panics on any
// unexercised query via the embedded interface, so a test cannot silently pass through an unread
// path.
type pendingOutcomeStore struct {
	workersvc.Store
	ownerID uuid.UUID
	run     store.Run
	worker  store.Worker
}

func (s *pendingOutcomeStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.ownerID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}

func (s *pendingOutcomeStore) GetWorkerByID(_ context.Context, _ uuid.UUID) (store.Worker, error) {
	return s.worker, nil
}

func (s *pendingOutcomeStore) RunHasPendingOutcomeLease(_ context.Context, id uuid.UUID) (bool, error) {
	return id == s.run.ID, nil
}

func newPendingOutcomeHandler(st workersvc.Store) *Handler {
	// A nonzero heartbeat-stale window so a fresh worker reads as live; the terminal_pending lease
	// (RunHasPendingOutcomeLease) is then the only reason hasLivePoller reports no poller.
	return &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{WorkerHeartbeatStale: time.Hour})}
}

func freshWorker() store.Worker {
	return store.Worker{ID: uuid.New(), LastHeartbeatAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
}

// inputReq (POST /runs/{id}/inputs authenticated as user) is defined in runs_test.go, shared here.

// TestCreateRunInputPendingOutcomeConfirmationRequired: a cancel of a run with a pending outcome,
// sent WITHOUT discard_pending_outcome, is a typed 409 whose body carries the machine-readable
// reason "outcome_pending_confirmation_required" beside the human message — the signal the web
// modal / CLI branch on. Dropping the ErrOutcomePendingConfirmationRequired switch case would
// return a 500, reddening this test.
func TestCreateRunInputPendingOutcomeConfirmationRequired(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &pendingOutcomeStore{ownerID: owner.ID, worker: freshWorker(), run: store.Run{
		ID: runID, UserID: owner.ID, Kind: runkind.Issue, Status: "running",
		WorkerID: pgUUIDv(uuid.New()),
	}}
	h := newPendingOutcomeHandler(st)

	w := httptest.NewRecorder()
	h.CreateRunInput(w, inputReq(owner, runID, `{"kind":"cancel"}`))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}
	var got struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, w.Body.String())
	}
	if got.Reason != "outcome_pending_confirmation_required" {
		t.Fatalf("reason = %q, want outcome_pending_confirmation_required (body: %s)", got.Reason, w.Body.String())
	}
	if strings.TrimSpace(got.Error) == "" {
		t.Errorf("error message is empty; want the human-readable guidance beside the reason")
	}
}

// TestCreateRunInputPendingOutcomeForeignCallerNotFound: an admin-read-only / foreign caller never
// resolves the owner-scoped run, so it 404s rather than reaching the confirmation gate (owner-only,
// nothing discarded for a non-owner).
func TestCreateRunInputPendingOutcomeForeignCallerNotFound(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &pendingOutcomeStore{ownerID: owner.ID, run: store.Run{
		ID: runID, UserID: owner.ID, Kind: runkind.Issue, Status: "running", WorkerID: pgUUIDv(uuid.New()),
	}}
	h := newPendingOutcomeHandler(st)

	w := httptest.NewRecorder()
	h.CreateRunInput(w, inputReq(store.User{ID: uuid.New(), IsAdmin: true}, runID, `{"kind":"cancel","discard_pending_outcome":true}`))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a foreign/admin-ro caller must not cancel; body: %s)", w.Code, w.Body.String())
	}
}
