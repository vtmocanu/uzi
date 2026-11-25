package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Fresh resume owner routes (issue #4628): GET and POST /api/runs/{id}/fresh-resume. Mounted in the
// RequireUser /runs group beside the recovery archive routes, owner-scoped like them: the run is
// resolved owner-only through h.wsvc.GetRun first (a foreign run, or an admin on another owner's run, is
// 404), then the service's own owner-scoped reads.

// freshResumeStartBodyLimit bounds the start body: a token (64 hex) and a UUID.
const freshResumeStartBodyLimit int64 = 4 * 1024

// freshResumeReasonMessages are the 409 sentences of a start refusal, keyed by reason. The reason itself
// is the machine-readable field; the sentence is the owner-facing explanation.
var freshResumeReasonMessages = map[string]string{
	apitypes.FreshResumeStatusDisabled:         "fresh resume is not enabled on this instance",
	apitypes.FreshResumeStatusInProgress:       "a fresh resume of this run is already in progress",
	apitypes.FreshResumeStatusIneligible:       "this run is not held in a state a fresh resume can start from",
	apitypes.FreshResumeStatusNoDestination:    "no other persistent hosted worker is online with fresh resume support",
	apitypes.FreshResumeStatusCustodyLimit:     "starting would take you past your custody hold limit",
	apitypes.FreshResumeStatusNoEvidence:       "this run has no saved evidence to resume from",
	apitypes.FreshResumeStatusExpired:          "the saved evidence has expired or is about to",
	apitypes.FreshResumeStatusUnbound:          "the run's checkpoint cannot be tied to a generation, so it might hold newer work than the evidence",
	apitypes.FreshResumeStatusPrereqUnproven:   "the saved evidence's base commits could not be proven on the forge",
	apitypes.FreshResumeStatusAmbiguous:        "several saved copies of this run's work exist and none contains the others",
	apitypes.FreshResumeReasonStaleSelection:   "the selection changed since the preview; review the preview again",
	apitypes.FreshResumeReasonRequestCompleted: "this request already finished; start again from a new preview",
}

// PreviewFreshResume is GET /api/runs/{id}/fresh-resume: the selection a start would use, or the status
// that says why none can. 200 for every status, including disabled: the status is the answer.
func (h *Handler) PreviewFreshResume(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	if !h.requireOwnedRun(w, r, user.ID, runID) {
		return
	}
	dto, err := h.wsvc.PreviewFreshResume(r.Context(), user.ID, runID)
	if err != nil {
		h.writeFreshResumeError(w, "fresh resume preview", err)
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// StartFreshResume is POST /api/runs/{id}/fresh-resume {selection_token, request_id}. 201 with the new
// pending transfer; 200 with replay=true when the request id already created an active one; 409 with a
// reason otherwise (disabled, stale_selection, request_completed, in_progress and the preview statuses).
func (h *Handler) StartFreshResume(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	if !h.requireOwnedRun(w, r, user.ID, runID) {
		return
	}
	var req apitypes.FreshResumeStartRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, freshResumeStartBodyLimit); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	requestID, err := uuid.Parse(req.RequestID)
	if err != nil || requestID == uuid.Nil || requestID.String() != req.RequestID {
		httpx.Error(w, http.StatusBadRequest, "request_id must be a canonical UUID")
		return
	}
	if req.SelectionToken == "" {
		httpx.Error(w, http.StatusBadRequest, "selection_token is required")
		return
	}
	res, err := h.wsvc.StartFreshResume(r.Context(), user.ID, runID, req.SelectionToken, requestID)
	if err != nil {
		h.writeFreshResumeError(w, "fresh resume start", err)
		return
	}
	row := res.Row
	dto := apitypes.FreshResumeStartDTO{
		ID: row.ID.String(), RunID: row.RunID.String(), RequestID: row.RequestID.String(), State: row.State, Replay: res.Replay,
		EvidenceGeneration: row.EvidenceGeneration, Kind: row.EvidenceKind, CheckpointRef: row.CheckpointRef.String, Sha: row.Sha,
		AdmissionDeadline: row.AdmissionDeadline.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if row.CaptureID.Valid {
		dto.CaptureID = uuid.UUID(row.CaptureID.Bytes).String()
	}
	status := http.StatusCreated
	if res.Replay {
		status = http.StatusOK
	}
	httpx.JSON(w, status, dto)
}

// requireOwnedRun resolves the run owner-only (admin refused, like the recovery archive routes) and
// answers 404 itself. It reports whether the caller may continue.
func (h *Handler) requireOwnedRun(w http.ResponseWriter, r *http.Request, userID, runID uuid.UUID) bool {
	if _, err := h.wsvc.GetRun(r.Context(), userID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return false
		}
		slog.Error("fresh resume: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

// writeFreshResumeError maps a fresh resume service error: a refusal is 409 with its reason, an unknown
// run is 404, anything else is logged and 500.
func (h *Handler) writeFreshResumeError(w http.ResponseWriter, what string, err error) {
	var refused *workersvc.FreshResumeRefused
	switch {
	case errors.As(err, &refused):
		msg, ok := freshResumeReasonMessages[refused.Reason]
		if !ok {
			msg = "fresh resume refused"
		}
		httpx.ErrorReason(w, http.StatusConflict, msg, refused.Reason)
	case errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found")
	default:
		slog.Error(what, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// freshResumeInProgressReason is the 409 reason of an owner write refused because a fresh resume
// transfer owns the run's status, worker and claim generation.
const freshResumeInProgressReason = "fresh_resume_in_progress"

// refuseIfFreshResumeActive answers 409 fresh_resume_in_progress and reports true when the owner's run
// has a pending or claimed fresh resume transfer. It is the explicit pre-read of the owner writers whose
// SQL may change neither status, worker_id nor claim_generation (pause_mode, extend, scope ceiling,
// set-token, policy holds), which the guard trigger therefore never sees. A write that does change them
// is still refused by the guard (writeFreshResumeInProgress).
//
// The read is unlocked and the write is a separate statement, so a Start that commits in between is not
// refused here. That race is accepted because each such write is harmless to a pending or claimed
// transfer: extend only adds wall budget; pause is refused by the service's status='running' predicate
// while the transfer is pending, and while it is claimed a racing pause could leave pause columns set,
// which are outside the saved tuple; set-token is re-resolved by the owner-hold refusal in
// lockedParkRefusal (run_credential.go) and, at claim assembly, by the fresh refusal of a disabled
// credential; scope_ceiling is outside the saved tuple, so a refusal never overwrites it; a policy-hold
// Resume changes status and meets the guard. A run that is not the caller's is left to the service's own 404.
func (h *Handler) refuseIfFreshResumeActive(w http.ResponseWriter, r *http.Request, userID, runID uuid.UUID) bool {
	if h.q == nil { // handlers wired without a store (unit tests) have no transfer to read
		return false
	}
	if _, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		slog.Error("fresh resume: owner write pre-read", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return true
	}
	if _, err := h.q.GetActiveFreshResumeForRun(r.Context(), runID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		slog.Error("fresh resume: owner write pre-read", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return true
	}
	httpx.ErrorReason(w, http.StatusConflict, freshResumeInProgressReason, freshResumeInProgressReason)
	return true
}

// writeFreshResumeInProgress answers 409 and reports true when err is the fresh resume guard refusing an
// owner write (store.IsFreshResumeInProgress: SQLSTATE UZ416, rule 1) or the service's own in-progress
// refusal, or the guard's predecessor-excluded rule. It is the one mapping every owner path that
// writes the run through autocommit uses (Resume, Pause, Extend, set-token, policy holds), so a guarded
// write is never a 500.
func writeFreshResumeInProgress(w http.ResponseWriter, err error) bool {
	var refused *workersvc.FreshResumeRefused
	switch {
	case store.IsFreshResumeInProgress(err), errors.As(err, &refused) && refused.Reason == apitypes.FreshResumeStatusInProgress:
		httpx.ErrorReason(w, http.StatusConflict, freshResumeInProgressReason, freshResumeInProgressReason)
		return true
	case store.IsFreshResumePredecessorExcluded(err):
		httpx.ErrorReason(w, http.StatusConflict, "fresh_resume_predecessor_excluded", "fresh_resume_predecessor_excluded")
		return true
	}
	return false
}
