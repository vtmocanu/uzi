package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// completionDecisionRequest is the POST /api/runs/{id}/completion/decision body (PRD #1226 M5,
// D7): the owner/admin decision on a completion-blocked run, with optional guidance. This child
// supports ONLY decision="continue"; any other value is a 400 (the other decisions are reserved
// for a later child). Guidance is optional and capped by MaxGuidanceBytes, like StartRunRework's.
type completionDecisionRequest struct {
	Decision string `json:"decision"`
	Guidance string `json:"guidance"`
}

// ContinueCompletionDecision records the owner's CONTINUE decision on a completion-blocked run
// (PRD #1226 M5, D7) and resumes it. It is the sibling of ResumeRunNow: owner-scoped (a
// foreign/absent run is 404 before any write, enforced by GetRunByIDForUser inside the service),
// mounted RequireUser so the CLI's uzc_ Bearer reaches it — NOT the cookie-only RequireAuth group.
//
// It validates the request shape (decision + guidance cap) HERE, then hands the run-state dispatch
// and the writes to workersvc.ContinueCompletionDecision so web and CLI (one endpoint) cannot
// drift. The service resolves BOTH completion-blocked states — the live `awaiting_input`
// completion-question window and the `paused` hold — and 409s (ErrCompletionNotBlocked) a run in
// neither.
func (h *Handler) ContinueCompletionDecision(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req completionDecisionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Decision != "continue" {
		httpx.Error(w, http.StatusBadRequest, "decision must be \"continue\"")
		return
	}
	if len(req.Guidance) > MaxGuidanceBytes {
		httpx.Error(w, http.StatusBadRequest, "guidance is too long")
		return
	}

	run, err := h.wsvc.ContinueCompletionDecision(r.Context(), user.ID, runID, req.Guidance)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrCompletionNotBlocked):
			httpx.Error(w, http.StatusConflict, err.Error())
		default:
			slog.Error("continue completion decision", "run_id", runID, "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.clock())})
}
