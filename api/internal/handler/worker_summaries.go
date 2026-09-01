package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// workerIntentSummaryRequest is the body of POST /api/worker/runs/{id}/summary/intent
// (PRD #362 M1): the plain-English "what this run will implement" summary. The worker
// sends only the text — the service derives (user, repo) from the claimed run.
type workerIntentSummaryRequest struct {
	Summary string `json:"summary"`
}

// workerPlanSummaryRequest is the body of POST /api/worker/runs/{id}/summary/plan (PRD
// #362 M1): the plan summary, the tagged deltas, and the plan_md the summary was
// generated from (the Decision 3 stale-write guard value).
type workerPlanSummaryRequest struct {
	Summary string                     `json:"summary"`
	Deltas  []apitypes.RunSummaryDelta `json:"deltas"`
	PlanMd  string                     `json:"plan_md"`
}

// WorkerSetIntentSummary persists a run's intent summary (PRD #362 M1), modeled on
// WorkerCreateFinding. Auth via the worker context; the service enforces the
// owner/non-terminal/has-repo guards and the idempotent-on-set skip (a second post for a
// run that already has an intent summary is a no-op SUCCESS, Decision 3). On a real write
// the service emits the live-update WS frame.
func (h *Handler) WorkerSetIntentSummary(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workerIntentSummaryRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Byte-cap validation on the RAW input (mirrors WorkerCreateFinding); the service
	// sanitises after this gate.
	if req.Summary == "" || len(req.Summary) > workersvc.MaxSummaryBytes {
		httpx.Error(w, http.StatusBadRequest, "summary must be non-empty and bounded")
		return
	}

	if _, _, err := h.wsvc.SetIntentSummary(r.Context(), wkr, runID, req.Summary); err != nil {
		writeSummaryError(w, err, "intent")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// WorkerSetPlanSummary persists a run's plan summary + deltas (PRD #362 M1). Auth via the
// worker context; the service enforces the owner/non-terminal/has-repo guards, validates
// the deltas (Decision 6 → 400 on an unknown kind / oversize list / empty text), and
// applies the Decision 3 stale-write guard against the posted plan_md (a superseded plan
// → 409, never a run failure). On a successful write it emits the live-update WS frame.
func (h *Handler) WorkerSetPlanSummary(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workerPlanSummaryRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Summary == "" || len(req.Summary) > workersvc.MaxSummaryBytes {
		httpx.Error(w, http.StatusBadRequest, "summary must be non-empty and bounded")
		return
	}
	// plan_md is the stale-write guard value; an empty one cannot match a real plan and
	// would silently reject every write, so require it up front.
	if req.PlanMd == "" {
		httpx.Error(w, http.StatusBadRequest, "plan_md is required")
		return
	}

	if _, err := h.wsvc.SetPlanSummary(r.Context(), wkr, runID, req.Summary, req.Deltas, req.PlanMd); err != nil {
		writeSummaryError(w, err, "plan")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeSummaryError maps a summary service error to its HTTP status, shared by both
// endpoints. A stale plan is a 409 conflict (a superseded plan, not a run failure);
// invalid deltas are a 400; an unknown default is logged and 500.
func writeSummaryError(w http.ResponseWriter, err error, kind string) {
	switch {
	case errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker's user")
	case errors.Is(err, workersvc.ErrSummaryRepoRequired):
		httpx.Error(w, http.StatusConflict, "this run has no repository, so a summary cannot be recorded")
	case errors.Is(err, workersvc.ErrRunTerminal):
		httpx.Error(w, http.StatusConflict, "the run has finished; cannot record a summary")
	case errors.Is(err, workersvc.ErrSummaryDeltasInvalid):
		httpx.Error(w, http.StatusBadRequest, "summary deltas are invalid")
	case errors.Is(err, workersvc.ErrSummaryPlanStale):
		httpx.Error(w, http.StatusConflict, "the plan changed since this summary was generated")
	default:
		slog.Error("worker set "+kind+" summary", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
