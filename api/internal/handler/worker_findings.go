package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// workerFindingRequest is the body of POST /api/worker/runs/{id}/findings (PRD #333
// M2). The worker sends only the finding's content — NEVER a user/repo id; the service
// derives (user_id, repo_id) from the claimed run (D2/D3). `location` is a
// symbol-anchored coordinate the service canonicalises; `title`/`description` are
// untrusted agent text the service renders inert before storing.
type workerFindingRequest struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Location    string   `json:"location"`
	Labels      []string `json:"labels"`
	Confidence  string   `json:"confidence"`
}

// WorkerCreateFinding is the worker's report_incidental_issue tool (M2, D2): it records
// one off-task bug the agent noticed mid-run. It NEVER writes the forge — capture goes
// worker→api like proposals, and filing stays human-gated (R3). The run must be the
// worker's user's run, non-terminal, and with a repo; caps bound the text and the
// per-run count. Bounded by the per-worker proposal rate limiter on the route. Returns
// the created finding id so the emitted stream card can act on it.
func (h *Handler) WorkerCreateFinding(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req workerFindingRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Byte-cap validation on the RAW input (mirrors WorkerCreateProposal); the service
	// sanitises + canonicalises after this gate. Caps are upper bounds on what the agent
	// may send, not on the stored (sanitised) length.
	if req.Title == "" || len(req.Title) > workersvc.MaxFindingTitleBytes {
		httpx.Error(w, http.StatusBadRequest, "title must be non-empty and at most 255 characters")
		return
	}
	if req.Description == "" {
		httpx.Error(w, http.StatusBadRequest, "description must be non-empty")
		return
	}
	if len(req.Description) > workersvc.MaxIssueDescriptionBytes {
		httpx.Error(w, http.StatusBadRequest, "description is too large")
		return
	}
	if req.Location == "" || len(req.Location) > workersvc.MaxFindingLocationBytes {
		httpx.Error(w, http.StatusBadRequest, "location must be non-empty and at most 512 characters")
		return
	}
	if len(req.Confidence) > workersvc.MaxFindingConfidenceBytes {
		httpx.Error(w, http.StatusBadRequest, "confidence is too long")
		return
	}
	if len(req.Labels) > workersvc.MaxProposalLabels {
		httpx.Error(w, http.StatusBadRequest, "too many labels")
		return
	}
	for _, l := range req.Labels {
		if l == "" || len(l) > workersvc.MaxProposalLabelBytes {
			httpx.Error(w, http.StatusBadRequest, "each label must be non-empty and at most 255 characters")
			return
		}
	}

	finding, err := h.wsvc.CreateFinding(r.Context(), wkr, runID, workersvc.CreateFindingRequest{
		Title:       req.Title,
		Description: req.Description,
		Location:    req.Location,
		Labels:      req.Labels,
		Confidence:  req.Confidence,
	})
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker's user")
		case errors.Is(err, workersvc.ErrFindingRepoRequired):
			httpx.Error(w, http.StatusConflict, "this run has no repository, so a finding cannot be recorded")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "the run has finished; cannot record a finding")
		case errors.Is(err, workersvc.ErrFindingLocationInvalid):
			httpx.Error(w, http.StatusBadRequest, "location is empty after canonicalisation")
		case errors.Is(err, workersvc.ErrFindingCapReached):
			// The tool surfaces this exact message to the agent as its soft ack.
			httpx.Error(w, http.StatusTooManyRequests, "finding cap reached for this run")
		default:
			slog.Error("worker create finding", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"id": finding.ID.String()})
}
