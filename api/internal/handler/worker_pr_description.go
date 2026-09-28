package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1798 D9: the worker's PR-description artifact routes. Every route is Bearer-only
// (RequireWorker), scoped to a run this worker holds, and fenced on claim_generation by the
// service. The worker sends the RAW fields to stage; the stage response carries the sanitized
// fields, which are the only text the renderer may publish (D7).
//
// Status map: 400 invalid body / over-cap raw field / bad enum or sha; 404 run not held by
// this worker, or no such version on this run; 409 with a machine-readable reason:
// stale_claim (claim generation not live), lock_conflict (the ack's expected_lock_version lost
// the compare-and-swap), version_conflict (the version cannot take this transition, or the
// request names a PR that is not the run's own), too_many_versions (stage past
// workersvc.MaxPrDescPendingVersionsPerRun pending versions in the live claim generation, or
// workersvc.MaxPrDescVersionsPerRun versions in all), run_terminal, repo_required. The service
// checks the claim fence and the caps before it sanitizes, so a stale or capped stage is 409
// even when its fields would be rejected by the sanitizer (an unknown scope kind, a bad
// verification result or sha). The request-shape checks run first and stay 400 whatever the
// fence would say: a missing claim_generation, an over-cap raw field (prDescRawCapOK), an
// unknown source, a bad size, sha or target branch. A deterministic_only stage ignores its raw
// fields entirely (they are discarded, not validated), so they never make it a 400.
// Stage also rides the per-worker proposal limiter (429 when exhausted).

// prDescWorker resolves the authenticated worker and the {id} run param, answering the
// request itself on failure.
func prDescWorker(w http.ResponseWriter, r *http.Request) (store.Worker, uuid.UUID, bool) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return store.Worker{}, uuid.Nil, false
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return store.Worker{}, uuid.Nil, false
	}
	return wkr, runID, true
}

// prDescRawCapOK is the handler's RAW byte-cap gate (like WorkerSetIntentSummary's), applied
// before the service sanitizes: an over-cap field is refused, never truncated into storage.
func prDescRawCapOK(f apitypes.PrDescriptionFields) bool {
	if len(f.Summary) > workersvc.MaxPrDescSummaryRawBytes {
		return false
	}
	if len(f.Changes) > workersvc.MaxPrDescListRawEntries || len(f.ScopeNotes) > workersvc.MaxPrDescListRawEntries ||
		len(f.ReviewPointers) > workersvc.MaxPrDescListRawEntries || len(f.Verification) > workersvc.MaxPrDescListRawEntries {
		return false
	}
	over := func(s string) bool { return len(s) > workersvc.MaxPrDescItemRawBytes }
	if slices.ContainsFunc(f.Changes, over) || slices.ContainsFunc(f.ReviewPointers, over) {
		return false
	}
	for _, n := range f.ScopeNotes {
		if over(n.Text) {
			return false
		}
	}
	for _, v := range f.Verification {
		if over(v.Command) {
			return false
		}
	}
	return true
}

// WorkerStagePrDescription is POST /api/worker/runs/{id}/pr-description/stage.
func (h *Handler) WorkerStagePrDescription(w http.ResponseWriter, r *http.Request) {
	wkr, runID, ok := prDescWorker(w, r)
	if !ok {
		return
	}
	var req apitypes.PrDescriptionStageRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	if req.Source != workersvc.PrDescSourceDeterministic && !prDescRawCapOK(req.Fields) {
		httpx.Error(w, http.StatusBadRequest, "pr description fields must be bounded")
		return
	}
	v, err := h.wsvc.StagePrDescription(r.Context(), wkr, runID, req)
	if err != nil {
		writePrDescriptionError(w, err, "stage")
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.PrDescriptionStageResponse{Version: v})
}

// WorkerBindPrDescription is POST /api/worker/runs/{id}/pr-description/bind.
func (h *Handler) WorkerBindPrDescription(w http.ResponseWriter, r *http.Request) {
	wkr, runID, ok := prDescWorker(w, r)
	if !ok {
		return
	}
	var req apitypes.PrDescriptionBindRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	resp, err := h.wsvc.BindPrDescription(r.Context(), wkr, runID, req)
	if err != nil {
		writePrDescriptionError(w, err, "bind")
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// WorkerLookupPrDescription is POST /api/worker/runs/{id}/pr-description/lookup.
func (h *Handler) WorkerLookupPrDescription(w http.ResponseWriter, r *http.Request) {
	wkr, runID, ok := prDescWorker(w, r)
	if !ok {
		return
	}
	var req apitypes.PrDescriptionLookupRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	resp, err := h.wsvc.LookupPrDescription(r.Context(), wkr, runID, req)
	if err != nil {
		writePrDescriptionError(w, err, "lookup")
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// WorkerAckPrDescription is POST /api/worker/runs/{id}/pr-description/ack.
func (h *Handler) WorkerAckPrDescription(w http.ResponseWriter, r *http.Request) {
	wkr, runID, ok := prDescWorker(w, r)
	if !ok {
		return
	}
	var req apitypes.PrDescriptionAckRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "claim_generation is required")
		return
	}
	resp, err := h.wsvc.AckPrDescription(r.Context(), wkr, runID, req)
	if err != nil {
		writePrDescriptionError(w, err, "ack")
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func writePrDescriptionError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, workersvc.ErrPrDescriptionInvalid):
		httpx.Error(w, http.StatusBadRequest, "pr description request is invalid")
	case errors.Is(err, workersvc.ErrRunNotOwned), errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
	case errors.Is(err, workersvc.ErrPrDescriptionVersionNotFound):
		httpx.Error(w, http.StatusNotFound, "pr description version not found for this run")
	case errors.Is(err, workersvc.ErrPrDescriptionStaleClaim):
		httpx.ErrorReason(w, http.StatusConflict, "the claim generation is not this run's live claim", "stale_claim")
	case errors.Is(err, workersvc.ErrPrDescriptionLockConflict):
		httpx.ErrorReason(w, http.StatusConflict, "the pr description changed since expected_lock_version", "lock_conflict")
	case errors.Is(err, workersvc.ErrPrDescriptionVersionConflict):
		httpx.ErrorReason(w, http.StatusConflict, "the pr description version cannot take this transition", "version_conflict")
	case errors.Is(err, workersvc.ErrPrDescriptionTooManyVersions):
		httpx.ErrorReason(w, http.StatusConflict, "this run has too many pending pr description versions", "too_many_versions")
	case errors.Is(err, workersvc.ErrRunTerminal):
		httpx.ErrorReason(w, http.StatusConflict, "the run has finished", "run_terminal")
	case errors.Is(err, workersvc.ErrSummaryRepoRequired):
		httpx.ErrorReason(w, http.StatusConflict, "this run has no repository", "repo_required")
	default:
		slog.Error("worker pr description "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
