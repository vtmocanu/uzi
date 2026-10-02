package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// decisionsMemoEnabled reads the decisions_memo_enabled admin kill-switch, failing closed: a nil
// settings cache or any read error counts as disabled (issue #2083).
func (h *Handler) decisionsMemoEnabled(r *http.Request) bool {
	if h.settings == nil {
		return false
	}
	on, err := h.settings.DecisionsMemoEnabled(r.Context())
	return err == nil && on
}

// WorkerSaveDecisionsMemo stores the run's decisions memo (issue #2083). Status mapping:
//
//	400 claim_generation_required  missing or negative claim_generation
//	400                            malformed body, empty body, or body over 8192 bytes once sanitized
//	401                            no worker on the request
//	404                            run not held by this worker (ErrRunNotOwned)
//	409 decisions_memo_disabled    setting off, unreadable, or no settings cache
//	409 claim_not_current          the fenced statement wrote nothing: stale generation, released
//	                               claim, run not running, a kind without a memo, or no repo
//	204                            stored (the body is never echoed)
//
// The body is never logged.
func (h *Handler) WorkerSaveDecisionsMemo(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req apitypes.DecisionsMemoWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration < 0 {
		httpx.ErrorReason(w, http.StatusBadRequest, "claim_generation is required", "claim_generation_required")
		return
	}
	if !h.decisionsMemoEnabled(r) {
		httpx.ErrorReason(w, http.StatusConflict, "the decisions memo is disabled", "decisions_memo_disabled")
		return
	}
	if _, err := h.wsvc.SaveDecisionsMemo(r.Context(), wkr, runID, *req.ClaimGeneration, req.Body); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrDecisionsMemoClaimNotCurrent):
			httpx.ErrorReason(w, http.StatusConflict, "the run's claim is not current", "claim_not_current")
		case errors.Is(err, workersvc.ErrDecisionsMemoEmpty):
			httpx.Error(w, http.StatusBadRequest, "decisions memo body must be non-empty")
		case errors.Is(err, workersvc.ErrDecisionsMemoTooLarge):
			httpx.Error(w, http.StatusBadRequest, "decisions memo body must be at most 8192 bytes")
		default:
			slog.Error("worker save decisions memo", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WorkerGetDecisionsMemo returns the prior decisions memo for a resumed mr_rework run (issue
// #2083). Status mapping:
//
//	400 claim_generation_required  missing, non-numeric or negative ?claim_generation=
//	401                            no worker on the request
//	200 {enabled:false,memo:null}  setting off, unreadable, or no settings cache (checked before
//	                               the run is read, so the answer reveals nothing about the run)
//	404                            run not held by this worker (ErrRunNotOwned)
//	409 claim_not_current          generation is not the run's, or its claim was released
//	200 {enabled:true,memo:null}   not an mr_rework run, no lineage, or no compatible memo
//	200 {enabled:true,memo:{...}}  the newest completed generation-compatible memo on the lineage
//
// The body is never logged.
func (h *Handler) WorkerGetDecisionsMemo(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	gen, err := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if err != nil || gen < 0 {
		httpx.ErrorReason(w, http.StatusBadRequest, "claim_generation is required", "claim_generation_required")
		return
	}
	if !h.decisionsMemoEnabled(r) {
		httpx.JSON(w, http.StatusOK, apitypes.DecisionsMemoReadResponse{Enabled: false})
		return
	}
	memo, err := h.wsvc.ResolveDecisionsMemo(r.Context(), wkr, runID, gen)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrDecisionsMemoClaimNotCurrent):
			httpx.ErrorReason(w, http.StatusConflict, "the run's claim is not current", "claim_not_current")
		default:
			slog.Error("worker get decisions memo", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	resp := apitypes.DecisionsMemoReadResponse{Enabled: true}
	if memo != nil {
		resp.Memo = &apitypes.DecisionsMemoDTO{Format: memo.Format, Body: memo.Body, SourceRunID: memo.SourceRunID.String()}
	}
	httpx.JSON(w, http.StatusOK, resp)
}
