package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The destination worker's fresh resume routes (issue #4628), mounted in the /api/worker group:
//
//	GET  /runs/{id}/fresh-resume/{frid}/bundle?claim_generation=
//	POST /runs/{id}/fresh-resume/{frid}/abandon   {claim_generation}
//	POST /runs/{id}/fresh-resume/{frid}/seeded    {claim_generation, attempt_id, live_generation}
//	GET  /runs/{id}/fresh-resume/{frid}/state?live_generation=
//
// They ignore the UZI_FRESH_RESUME gate (a transfer the gate no longer admits is still the destination's
// to fetch, abandon and report on). Only the state route is open to any transfer state; the others are
// fenced on the calling worker, its claim generation and the transfer being claimed. A lane worker never
// reaches them (laneWorkerAllowlist does not list them).

// freshWorkerBodyLimit bounds the abandon and seeded bodies: a few integers and an attempt id.
const freshWorkerBodyLimit int64 = 4 * 1024

// queryInt64 reads an int64 query parameter. A missing one is (0, true) when optional; a malformed or
// non-positive one is answered 400 here.
func queryInt64(w http.ResponseWriter, r *http.Request, name string, optional bool) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" && optional {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		httpx.Error(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}
	return v, true
}

// freshWorkerIDs parses the run and transfer ids of a destination route.
func freshWorkerIDs(w http.ResponseWriter, r *http.Request) (runID, frid uuid.UUID, ok bool) {
	if runID, ok = httpx.PathUUID(w, r, "id", "run"); !ok {
		return runID, frid, false
	}
	frid, ok = httpx.PathUUID(w, r, "frid", "fresh resume")
	return runID, frid, ok
}

// writeFreshWorkerError maps the destination routes' service errors. A fenced route treats an unknown
// transfer like any other fence failure (409 not_claimed: the worker stops); the state route's 404 is
// notFound.
func writeFreshWorkerError(w http.ResponseWriter, what string, err error, notFound bool) {
	switch {
	case errors.Is(err, workersvc.ErrFreshTransferNotFound) && notFound:
		httpx.ErrorReason(w, http.StatusNotFound, "fresh resume transfer not found", "fresh_resume_not_found")
	case errors.Is(err, workersvc.ErrFreshNotClaimed), errors.Is(err, workersvc.ErrFreshTransferNotFound):
		httpx.ErrorReason(w, http.StatusConflict, "this transfer is not claimed by this worker at this generation", apitypes.FreshResumeReasonNotClaimed)
	case errors.Is(err, workersvc.ErrFreshAttemptMismatch):
		httpx.ErrorReason(w, http.StatusConflict, "the receipt does not match the seeded attempt", apitypes.FreshResumeReasonAttemptMismatch)
	case errors.Is(err, workersvc.ErrFreshNoBundle):
		httpx.ErrorReason(w, http.StatusConflict, "this transfer's evidence is a checkpoint ref, not a bundle", apitypes.FreshResumeReasonNoBundle)
	case errors.Is(err, workersvc.ErrFreshLocksBusy), errors.Is(err, workersvc.ErrFreshResumeUnavailable):
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "fresh resume is busy; retry shortly", "fresh_resume_busy")
	default:
		slog.Error(what, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// WorkerFreshResumeBundle streams the transfer's unsealed capture to its destination worker. The fence
// (claimed for this worker at claim_generation, evidence kind capture) is taken under the locks and
// released before the stream starts; the bytes are the owner download's own stream.
func (h *Handler) WorkerFreshResumeBundle(w http.ResponseWriter, r *http.Request) {
	svc := h.recovery()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(svc.RequestDeadline()))
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, frid, ok := freshWorkerIDs(w, r)
	if !ok {
		return
	}
	gen, ok := queryInt64(w, r, "claim_generation", false)
	if !ok {
		return
	}
	captureID, err := h.wsvc.FreshResumeBundleCapture(r.Context(), wkr, runID, frid, gen)
	if err != nil {
		writeFreshWorkerError(w, "fresh resume bundle", err, false)
		return
	}
	err = svc.Download(r.Context(), w, wkr.UserID, runID, captureID)
	if err == nil {
		return
	}
	if errors.Is(err, recovery.ErrStreamAborted) {
		slog.Error("fresh resume bundle aborted mid-stream", "run_id", runID.String(), "capture_id", captureID.String(), "transfer", frid.String())
		return
	}
	mapRecoveryError(w, err)
}

// WorkerFreshResumeAbandon gives the transfer back: refused "abandoned", the run restored to its held
// recovery_wait.
func (h *Handler) WorkerFreshResumeAbandon(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, frid, ok := freshWorkerIDs(w, r)
	if !ok {
		return
	}
	var req apitypes.FreshResumeAbandonRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, freshWorkerBodyLimit); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration < 1 {
		httpx.Error(w, http.StatusBadRequest, "claim_generation must be a positive integer")
		return
	}
	if err := h.wsvc.AbandonFreshResume(r.Context(), wkr, runID, frid, req.ClaimGeneration); err != nil {
		writeFreshWorkerError(w, "fresh resume abandon", err, false)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.FreshResumeAbandonDTO{ID: frid.String(), State: "refused"})
}

// WorkerFreshResumeSeeded is the seeded receipt (see workersvc.FreshResumeReceipt).
func (h *Handler) WorkerFreshResumeSeeded(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, frid, ok := freshWorkerIDs(w, r)
	if !ok {
		return
	}
	var req apitypes.FreshResumeSeededRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, freshWorkerBodyLimit); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration < 1 || req.AttemptID == "" || len(req.AttemptID) > 64 || req.LiveGeneration < 0 {
		httpx.Error(w, http.StatusBadRequest, "claim_generation, attempt_id and live_generation are required")
		return
	}
	dto, err := h.wsvc.FreshResumeReceipt(r.Context(), wkr, runID, frid, req.ClaimGeneration, req.AttemptID, req.LiveGeneration)
	if err != nil {
		writeFreshWorkerError(w, "fresh resume seeded", err, false)
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// WorkerFreshResumeState answers what became of a transfer for its destination: any state, fenced only on
// the run, its owner and the destination being the caller. 404 when the caller has no such transfer.
func (h *Handler) WorkerFreshResumeState(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, frid, ok := freshWorkerIDs(w, r)
	if !ok {
		return
	}
	live, ok := queryInt64(w, r, "live_generation", true)
	if !ok {
		return
	}
	dto, err := h.wsvc.FreshResumeState(r.Context(), wkr, runID, frid, live)
	if err != nil {
		writeFreshWorkerError(w, "fresh resume state", err, true)
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// refuseFreshPreSeedOnGuard handles a hold or park writer's error: when it is the fresh resume guard's
// refusal (a pending or claimed transfer blocks the paused write) and the run's transfer is claimed for
// this worker, it writes the typed pre-seed 409 and reports true. run is the row the service returned
// with the error; when it is not the addressed run the row is read here. Any other error is "no".
func (h *Handler) refuseFreshPreSeedOnGuard(w http.ResponseWriter, r *http.Request, wkr store.Worker, runID uuid.UUID, run store.Run, err error) bool {
	if !store.IsFreshResumeInProgress(err) {
		return false
	}
	if run.ID != runID {
		var getErr error
		run, getErr = h.q.GetRunByID(r.Context(), runID)
		if getErr != nil {
			return false
		}
	}
	return h.refuseFreshPreSeed(w, r, wkr, run)
}

// refuseFreshPreSeed is the shared answer to a destination's park or fail report that an excluded writer
// turned into zero rows (every park and fail writer skips a run with a pending or claimed transfer): when
// the run's transfer is claimed for this worker, the ordinary no-op reply would be misread as "the run
// moved on", so the worker gets 409 {run, reason: fresh_resume_pre_seed} instead and abandons the
// transfer. It reports whether it wrote that reply. An unreadable probe is "no": the caller's own reply
// stands.
func (h *Handler) refuseFreshPreSeed(w http.ResponseWriter, r *http.Request, wkr store.Worker, run store.Run) bool {
	if h.q == nil { // handlers wired without a store (unit tests) have no transfer to probe
		return false
	}
	row, err := h.q.GetActiveFreshResumeForRun(r.Context(), run.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("fresh resume: pre-seed probe", "error", err, "run", run.ID.String())
		}
		return false
	}
	if row.State != "claimed" || row.DestinationWorkerID != pgconv.UUID(wkr.ID) {
		return false
	}
	httpx.JSON(w, http.StatusConflict, map[string]any{
		"run":    h.workerRunDTO(r.Context(), run),
		"reason": apitypes.FreshResumeReasonPreSeed,
	})
	return true
}
