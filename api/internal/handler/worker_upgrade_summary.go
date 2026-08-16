package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerUpgradeSummary serves the count behind the Workers nav badge (PRD #113 M6).
//
// It exists as its OWN endpoint rather than riding the Workers page's data, and that is
// the whole point of the milestone: the page's poll is page-local and visibility-gated, so
// a badge fed from it would be stale or absent exactly when the operator is not looking at
// the Workers page — which is the only situation a nav badge is for. AppShell owns this
// poll, mirroring how the Judge badge sources /me/judge/stats.
//
// Owner-scoped, and scoped by CONSTRUCTION rather than by a filter: the query joins roll
// health through `workers`, the only table that knows an owner, because
// worker_upgrade_reports deliberately carries no user_id. A fleet-scoped count here would
// show every user a tally of OTHER users' failing workers, on every page of the app.
//
// Mounted on RequireUser, like /me/judge/stats, so a CLI token can read it too.
func (h *Handler) WorkerUpgradeSummary(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	summary, err := h.wsvc.UpgradeSummaryForUser(r.Context(), user.ID, h.version, h.startedAt, workersvc.UpgradeParams{})
	if err != nil {
		slog.Error("worker upgrade summary", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, summary)
}
