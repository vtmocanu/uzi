package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// completionDecisionRequest is the POST /api/runs/{id}/completion/decision body (PRD #1226 M5, D7 +
// #1227 M1): the owner/admin decision on a completion-blocked run. Decision is one of:
//
//   - "continue" (#1226, owner-only): resume with optional guidance (capped by MaxGuidanceBytes).
//   - "partial" (#1227, owner/admin): reduce scope to the keep milestone-id set, with a required
//     reason and the fenced contract_revision.
//   - "accept" (#1227, owner/admin): accept the criteria criterion-id set, with a required reason
//     and the fenced contract_revision.
//
// Any other value is a 400. The service (DecideCompletion) performs the ID validation, revision
// fence and transaction; the handler validates the request SHAPE (decision domain, reason cap +
// non-empty for partial/accept) and maps the typed errors to status codes.
type completionDecisionRequest struct {
	Decision         string   `json:"decision"`
	Guidance         string   `json:"guidance"`
	Keep             []string `json:"keep"`
	Criteria         []string `json:"criteria"`
	Reason           string   `json:"reason"`
	ContractRevision int      `json:"contract_revision"`
}

// ContinueCompletionDecision records the owner's decision on a completion-blocked run (PRD
// #1226 M5, D7 + #1227 M1) and resumes it. It is mounted RequireUser so the CLI's uzc_ Bearer
// reaches it — NOT the cookie-only RequireAuth group. ALL three decisions (continue, partial,
// accept) are OWNER-SCOPED: the service authorizes via the owner-only GetRun and hides a foreign
// run — including a read-only admin_ro (uza_) Bearer, which keeps IsAdmin=true through RequireUser —
// as 404, so no admin_ro token can WRITE (reduce/accept scope) on another user's run.
//
// It validates the request SHAPE here, then hands the dispatch, ID validation, revision fence and
// writes to workersvc.DecideCompletion so web and CLI (one endpoint) cannot drift. (The handler
// method name is unchanged for its existing route mount; it now covers all three decisions.)
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
	switch req.Decision {
	case "continue":
		if len(req.Guidance) > MaxGuidanceBytes {
			httpx.Error(w, http.StatusBadRequest, "guidance is too long")
			return
		}
	case "partial", "accept":
		if len(req.Reason) > MaxGuidanceBytes {
			httpx.Error(w, http.StatusBadRequest, "reason is too long")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			httpx.Error(w, http.StatusBadRequest, "reason is required")
			return
		}
	default:
		httpx.Error(w, http.StatusBadRequest, "decision must be one of \"continue\", \"partial\", \"accept\"")
		return
	}

	run, err := h.wsvc.DecideCompletion(r.Context(), user.ID, runID, workersvc.CompletionDecisionInput{
		Decision:         req.Decision,
		Guidance:         req.Guidance,
		Keep:             req.Keep,
		Criteria:         req.Criteria,
		Reason:           req.Reason,
		ContractRevision: req.ContractRevision,
	})
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrCompletionNotBlocked):
			httpx.Error(w, http.StatusConflict, err.Error())
		case errors.Is(err, workersvc.ErrCompletionRevisionConflict):
			httpx.Error(w, http.StatusConflict, err.Error())
		case errors.Is(err, workersvc.ErrCompletionDecisionInvalid):
			httpx.Error(w, http.StatusBadRequest, err.Error())
		default:
			slog.Error("completion decision", "run_id", runID, "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.clock())})
}
