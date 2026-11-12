package handler

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// hibernationResult is the 200 body of every hibernation route: the worker's state
// after the operation.
type hibernationResult struct {
	WorkerID string `json:"worker_id"`
	State    string `json:"state"`
	OpID     string `json:"op_id,omitempty"`
}

func newHibernationResult(w store.Worker) hibernationResult {
	r := hibernationResult{WorkerID: w.ID.String(), State: w.HibernationState}
	if w.HibernationOpID.Valid {
		r.OpID = uuid.UUID(w.HibernationOpID.Bytes).String()
	}
	return r
}

// ControllerHibernation is the controller's control-write for one hibernation step
// (PRD #4205), mounted under the controller bearer guard. The body is decoded strictly.
func (h *Handler) ControllerHibernation(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "workerID", "worker")
	if !ok {
		return
	}
	var body hostedsvc.HibernationTransition
	if err := httpx.DecodeJSONStrict(r, &body); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if h.wsvc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	row, err := h.wsvc.ApplyTransition(r.Context(), id, workersvc.HibernationTransition{
		OpID: body.OpID, Transition: body.Transition, DeploymentUID: body.DeploymentUID,
		ResourceVersion: body.ResourceVersion, PodUID: body.PodUID, Reason: body.Reason,
	})
	var verr *workersvc.HibernationValidationError
	switch {
	case errors.As(err, &verr):
		httpx.Error(w, http.StatusBadRequest, verr.Msg)
	case errors.Is(err, workersvc.ErrHibernationConflict):
		httpx.Error(w, http.StatusConflict, "hibernation precondition failed")
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "no such worker")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	default:
		httpx.JSON(w, http.StatusOK, newHibernationResult(row))
	}
}

// hibernationReason accepts an empty body or exactly one strict {reason?} object.
func hibernationReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req *struct {
		Reason *string `json:"reason"`
	}
	err := httpx.DecodeJSONStrictBounded(r, &req, 8<<10)
	if errors.Is(err, io.EOF) {
		return "", true
	}
	if err != nil || req == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid hibernation request")
		return "", false
	}
	if req.Reason == nil {
		return "", true
	}
	return *req.Reason, true
}

// AdminHibernateWorker admits a hibernation (cookie admin write). The actor is the
// session user, never the body.
func (h *Handler) AdminHibernateWorker(w http.ResponseWriter, r *http.Request) {
	h.adminHibernation(w, r, true)
}

// AdminWakeWorker wakes a sleeping worker (cookie admin write).
func (h *Handler) AdminWakeWorker(w http.ResponseWriter, r *http.Request) {
	h.adminHibernation(w, r, false)
}

func (h *Handler) adminHibernation(w http.ResponseWriter, r *http.Request, hibernate bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	reason, ok := hibernationReason(w, r)
	if !ok {
		return
	}
	if h.wsvc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	actor := workersvc.HibernationActor{ID: user.ID, Label: user.DisplayName.String}
	var row store.Worker
	var err error
	if hibernate {
		row, err = h.wsvc.HibernateWorker(r.Context(), actor, id, reason)
	} else {
		row, err = h.wsvc.WakeWorker(r.Context(), &actor, id, reason)
	}
	var refusal *workersvc.HibernationRefusal
	var verr *workersvc.HibernationValidationError
	switch {
	case errors.As(err, &refusal):
		httpx.JSON(w, http.StatusConflict, map[string]string{"error": "worker cannot be hibernated", "reason_code": refusal.Code})
	case errors.As(err, &verr):
		httpx.Error(w, http.StatusBadRequest, verr.Msg)
	case errors.Is(err, workersvc.ErrHibernationConflict):
		httpx.JSON(w, http.StatusConflict, map[string]string{"error": "worker is not hibernating", "reason_code": "not_hibernating"})
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "no such worker")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	default:
		httpx.JSON(w, http.StatusOK, newHibernationResult(row))
	}
}
