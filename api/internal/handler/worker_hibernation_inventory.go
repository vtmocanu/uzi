package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxHibernationInventoryBody bounds the inventory body: 10,000 entries and the docker census
// fit well inside it, and the canonical entries JSON is separately held to 1 MiB.
const maxHibernationInventoryBody = 1<<20 + 1<<18

// pgCheckViolation is the SQLSTATE of a CHECK constraint failure.
const pgCheckViolation = "23514"

// hibernationInventoryResult is the 200 body of both inventory routes.
type hibernationInventoryResult struct {
	Status string `json:"status"`
}

// respondHibernationInventoryError maps a service error to the worker-facing status. A database
// CHECK violation (the stored-form size bounds the validator cannot see) is the worker's body, so a 400.
func respondHibernationInventoryError(w http.ResponseWriter, err error) {
	var verr *workersvc.HibernationValidationError
	var pgerr *pgconn.PgError
	switch {
	case errors.As(err, &verr):
		httpx.Error(w, http.StatusBadRequest, verr.Msg)
	case errors.Is(err, workersvc.ErrHibernationConflict):
		httpx.Error(w, http.StatusConflict, "hibernation precondition failed")
	case errors.As(err, &pgerr) && pgerr.Code == pgCheckViolation:
		httpx.Error(w, http.StatusBadRequest, "invalid inventory")
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "no such worker")
	default:
		slog.Error("worker hibernation inventory", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// WorkerHibernationInventory records the volume inventory a preparing worker took (PRD #4208 M1),
// or one of its non-inventory statuses. The worker is the authenticated bearer; nothing in the body
// names it. The identity (op, pod, register nonce, volumes, resource version) must equal what the
// api recorded from the controller's observation, or the answer is 409 and nothing is written.
func (h *Handler) WorkerHibernationInventory(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	var req workersvc.InventoryRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, maxHibernationInventoryBody); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if h.wsvc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if req.IsStatus() {
		rep, err := req.PrepareStatus()
		if err == nil {
			err = h.wsvc.ReportHibernationInventoryStatus(r.Context(), wkr.ID, rep)
		}
		if err != nil {
			respondHibernationInventoryError(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, hibernationInventoryResult{Status: rep.Status})
		return
	}
	rep, err := req.PrepareInventory()
	if err == nil {
		err = h.wsvc.AcceptHibernationInventory(r.Context(), wkr.ID, rep)
	}
	if err != nil {
		respondHibernationInventoryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, hibernationInventoryResult{Status: workersvc.InventoryAccepted})
}

// WorkerHibernationExclusionFinal records the worker's last exclusion report against an accepted
// inventory.
func (h *Handler) WorkerHibernationExclusionFinal(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	var req workersvc.ExclusionFinalRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 8<<10); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if h.wsvc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rep, err := req.Prepare()
	if err == nil {
		err = h.wsvc.RecordHibernationExclusionFinal(r.Context(), wkr.ID, rep)
	}
	if err != nil {
		respondHibernationInventoryError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, hibernationInventoryResult{Status: "recorded"})
}
