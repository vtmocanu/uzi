package handler

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// SetCrossCheck updates the current user's plan-stage consent. An omitted plan
// field preserves its value, leaving room for other stages on this route.
func (h *Handler) SetCrossCheck(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Plan *bool `json:"plan"`
	}
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Plan != nil && *req.Plan {
		usable, err := h.wsvc.BothHarnessesUsable(r.Context(), user.ID)
		if err != nil {
			slog.Error("check cross-check credentials", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !usable {
			httpx.Error(w, http.StatusConflict, "both Claude and Codex credentials must be usable")
			return
		}
	}
	// The returned value comes from the same statement that applies the optional
	// update; no setter is generated for this column in the current store slice.
	var enabled bool
	err := h.pool.QueryRow(r.Context(), `UPDATE users
SET plan_cross_check_enabled = COALESCE($2::boolean, plan_cross_check_enabled)
WHERE id = $1 RETURNING plan_cross_check_enabled`, user.ID, req.Plan).Scan(&enabled)
	if err != nil {
		if err == pgx.ErrNoRows {
			httpx.Error(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("set cross-check consent", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	updated, err := h.q.GetUserByID(r.Context(), user.ID)
	if err != nil {
		slog.Error("read cross-check user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	response := map[string]any{"user": toDTO(updated)}
	if enabled && !updated.EphemeralWorkersEnabled {
		// Both directions need cross_check_v1: a Claude lead is checked on Codex (a
		// codex_harness_v1 worker), and a Codex lead on Claude (PRD #2460, a worker that
		// advertises cross_check_codex_lead_v1). Each is probed separately so the warning
		// names the family that has no capable worker.
		var codexCheckerAvailable, claudeCheckerAvailable bool
		err = h.pool.QueryRow(r.Context(), `SELECT
COALESCE(bool_or('codex_harness_v1' = ANY(protocol_capabilities)), false),
COALESCE(bool_or('cross_check_codex_lead_v1' = ANY(protocol_capabilities)), false)
FROM workers WHERE user_id = $1 AND status = 'online'
AND draining_since IS NULL AND NOT ephemeral AND NOT isolated_lane
AND 'cross_check_v1' = ANY(protocol_capabilities)`, user.ID).Scan(&codexCheckerAvailable, &claudeCheckerAvailable)
		if err != nil {
			slog.Error("check cross-check workers", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		switch {
		case !codexCheckerAvailable:
			response["warning"] = "No online worker can run Codex plan cross-checks; enable ephemeral workers or upgrade a worker."
		case !claudeCheckerAvailable:
			response["warning"] = "No online worker can run Claude plan cross-checks for Codex-lead runs; enable ephemeral workers or upgrade a worker."
		}
	}
	httpx.JSON(w, http.StatusOK, response)
}
