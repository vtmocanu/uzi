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

func (s *pendingOutcomeStore) CancelRunServerSideWithPendingOutcome(_ context.Context, _ store.CancelRunServerSideWithPendingOutcomeParams) (int64, error) {
	return 0, nil
}

func newPendingOutcomeHandler(st workersvc.Store) *Handler {
	// A nonzero heartbeat-stale window so a fresh worker reads as live; the terminal_pending lease
	// (RunHasPendingOutcomeLease) is then the only reason hasLivePoller reports no poller.
	return &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Hour})}
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

// TestCreateRunInputPendingOutcomeCancelRaceConflict pins the 0-row race response: a confirmed
// discard whose pending lease changed while the run stayed active is a typed retryable 409, never
// a false 200 saying the run was cancelled.
func TestCreateRunInputPendingOutcomeCancelRaceConflict(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &pendingOutcomeStore{ownerID: owner.ID, worker: freshWorker(), run: store.Run{
		ID: runID, UserID: owner.ID, Kind: runkind.Issue, Status: "running",
		WorkerID: pgUUIDv(uuid.New()),
	}}
	h := newPendingOutcomeHandler(st)

	w := httptest.NewRecorder()
	h.CreateRunInput(w, inputReq(owner, runID, `{"kind":"cancel","discard_pending_outcome":true}`))

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
	if got.Reason != "outcome_pending_changed" {
		t.Fatalf("reason = %q, want outcome_pending_changed (body: %s)", got.Reason, w.Body.String())
	}
	if strings.TrimSpace(got.Error) == "" {
		t.Error("error message is empty; want retry guidance")
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

// pendingOutcomeRouteStore keeps the existing detail-read fake but implements
// only the lease and guarded write reached by this cancel flow. Unexpected
// queries still panic through runsStore's embedded workersvc.Store.
type pendingOutcomeRouteStore struct {
	*runsStore
	raced       bool
	cancelCalls int
	reloadCalls int
	cancelArg   store.CancelRunServerSideWithPendingOutcomeParams
}

func (s *pendingOutcomeRouteStore) RunHasPendingOutcomeLease(_ context.Context, id uuid.UUID) (bool, error) {
	return id == s.run.ID && s.run.Status == "running" && s.run.WorkerID.Valid, nil
}

func (s *pendingOutcomeRouteStore) CancelRunServerSideWithPendingOutcome(_ context.Context, arg store.CancelRunServerSideWithPendingOutcomeParams) (int64, error) {
	s.cancelCalls++
	s.cancelArg = arg
	if s.raced || arg.ID != s.run.ID || arg.UserID != s.ownerID || s.run.Status != "running" || !s.run.WorkerID.Valid {
		return 0, nil
	}
	s.run.Status = "cancelled"
	s.run.StopKind = pgtype.Text{String: "cancelled", Valid: true}
	s.run.StopReason = arg.StopReason
	s.run.FinishedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return 1, nil
}

func (s *pendingOutcomeRouteStore) GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error) {
	s.reloadCalls++
	return s.runsStore.GetRunByID(ctx, id)
}

func TestCreateRunInputPolicyPendingDiscardRoutes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		confirmed       bool
		raced           bool
		foreignCaller   bool
		admin           bool
		foreignReporter bool
		wantCode        int
		wantReason      string
		wantStatus      string
		wantWrites      int
	}{
		{"confirmed discard", true, false, false, false, false, http.StatusAccepted, "", "cancelled", 1},
		{"unconfirmed discard", false, false, false, false, false, http.StatusConflict, "outcome_pending_confirmation_required", "running", 0},
		{"raced discard", true, true, false, false, false, http.StatusConflict, "outcome_pending_changed", "running", 1},
		{"foreign caller", true, false, true, false, false, http.StatusNotFound, "", "running", 0},
		{"foreign admin caller", true, false, true, true, false, http.StatusNotFound, "", "running", 0},
		{"foreign telemetry cannot suppress lease confirmation", false, false, false, false, true, http.StatusConflict, "outcome_pending_confirmation_required", "running", 0},
		{"confirmed discard uses lease despite foreign telemetry", true, false, false, false, true, http.StatusAccepted, "", "cancelled", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := store.User{ID: uuid.New()}
			worker := freshWorker()
			runID := uuid.New()
			st := &pendingOutcomeRouteStore{raced: tc.raced, runsStore: &runsStore{
				ownerID: owner.ID, worker: worker,
				run: store.Run{ID: runID, UserID: owner.ID, Kind: runkind.Issue, Status: "running", WorkerID: pgUUIDv(worker.ID)},
			}}
			h := newRunsHandler(t, st)
			h.wsvc = newPendingOutcomeHandler(st).wsvc
			reporter := worker.ID
			if tc.foreignReporter {
				reporter = uuid.New()
			}
			if _, err := h.wsvc.Heartbeat(context.Background(), store.Worker{ID: reporter}, nil,
				[]workersvc.OutboxEntry{{
					RunID: runID, PendingTerminal: 1, BlockedReason: "gap_unrecoverable", Since: time.Now(),
					PolicyPendingOutcomeID: uuid.New(), PolicyPendingSourceGeneration: 2,
				}}, nil); err != nil {
				t.Fatalf("seed both pending facts: %v", err)
			}
			caller := owner
			if tc.foreignCaller {
				caller = store.User{ID: uuid.New(), IsAdmin: tc.admin}
			}
			body := `{"kind":"cancel","body":"discard held outcome"}`
			if tc.confirmed {
				body = `{"kind":"cancel","body":"discard held outcome","discard_pending_outcome":true}`
			}
			rec := httptest.NewRecorder()
			h.CreateRunInput(rec, inputReq(caller, runID, body))
			if rec.Code != tc.wantCode {
				t.Fatalf("cancel = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			var response struct {
				ServerSide bool   `json:"server_side"`
				Reason     string `json:"reason"`
				Error      string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode cancel: %v", err)
			}
			if response.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", response.Reason, tc.wantReason)
			}
			if tc.wantCode == http.StatusConflict && strings.TrimSpace(response.Error) == "" {
				t.Fatal("conflict omitted human guidance")
			}
			if st.cancelCalls != tc.wantWrites {
				t.Fatalf("guarded writes = %d, want %d", st.cancelCalls, tc.wantWrites)
			}
			if tc.wantWrites > 0 && (st.cancelArg.ID != runID || st.cancelArg.UserID != owner.ID || !st.cancelArg.StopReason.Valid || st.cancelArg.StopReason.String != "discard held outcome") {
				t.Fatalf("guarded cancel parameters = %+v", st.cancelArg)
			}
			if tc.wantStatus == "cancelled" {
				if !response.ServerSide || st.reloadCalls != 1 {
					t.Fatalf("success server_side = %v, authoritative judge reloads = %d", response.ServerSide, st.reloadCalls)
				}
				if !st.run.FinishedAt.Valid || st.run.StopKind.String != "cancelled" || st.run.StopReason.String != "discard held outcome" {
					t.Fatalf("cancel did not persist terminal fields: status=%s stop_kind=%+v stop_reason=%+v finished_at=%+v", st.run.Status, st.run.StopKind, st.run.StopReason, st.run.FinishedAt)
				}
			} else if response.ServerSide || st.run.FinishedAt.Valid || st.reloadCalls != 0 {
				t.Fatal("refused cancel reported success or invoked terminal follow-up")
			}
			// Read through the detail handler as the owner, rather than treating
			// an acknowledgement or a zero-row write as cancellation evidence.
			detail := httptest.NewRecorder()
			h.GetRun(detail, runReq(owner, runID))
			if detail.Code != http.StatusOK {
				t.Fatalf("detail = %d: %s", detail.Code, detail.Body.String())
			}
			var got struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode authoritative detail: %v", err)
			}
			if got.Run.Status != tc.wantStatus {
				t.Fatalf("authoritative status = %q, want %q", got.Run.Status, tc.wantStatus)
			}
		})
	}
}
