package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// RunsInProgressCount serves the count behind the Runs nav badge (PRD #239).
//
// Like the Workers upgrade summary and the Judge stats, it is its OWN lightweight
// endpoint rather than riding the Runs page's data: AppShell owns the poll so the
// badge stays live wherever the operator is, not just on the Runs page. The response
// is a single integer, not run rows, backed by one indexed count(*).
//
// Owner-scoped by the query's user_id filter, and scoped to the same kind-set the
// /runs page lists (chat and judge excluded, Decision 4), so the badge can never count
// a run the page would not show — it is a strict non-terminal subset of that list.
//
// Mounted on RequireUser, like /me/judge/stats and /me/workers/upgrade-summary, so a
// CLI token can read it too.
func (h *Handler) RunsInProgressCount(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	count, err := h.q.CountInProgressRunsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("count in-progress runs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"count": count})
}
