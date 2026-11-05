package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

var repoAgentSourceHoldCommitRE = regexp.MustCompile(`^([0-9a-f]{40})?$`)

func (h *Handler) WorkerRunRepoAgentSourceHold(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		Commit          string `json:"commit"`
		ClaimGeneration int64  `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil || body.ClaimGeneration <= 0 || !repoAgentSourceHoldCommitRE.MatchString(body.Commit) {
		httpx.Error(w, http.StatusBadRequest, "positive claim_generation and an optional full commit id required")
		return
	}
	run, applied, err := h.wsvc.SetRunRepoAgentSourceHold(r.Context(), wkr, runID, body.ClaimGeneration, body.Commit)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on the repo agent source hold")
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.ErrorReason(w, http.StatusConflict, "repo agent source hold claim is stale", "stale_claim")
		default:
			slog.Error("worker run repo agent source hold", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if applied {
		slog.Warn("run held: repo-agent source revision unreadable on the worker",
			"run_id", runID, "worker_id", wkr.ID, "commit", body.Commit)
	}
	code := http.StatusOK
	if !applied {
		code = http.StatusConflict
	}
	httpx.JSON(w, code, map[string]any{"run": h.workerRunDTO(r.Context(), run)})
}
