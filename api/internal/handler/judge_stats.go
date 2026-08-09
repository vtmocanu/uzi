package handler

import (
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
)

// JudgeStats serves the caller's global judge-triage tally — the "across all your runs"
// strip (PRD #94 Decision 8). Owner-scoped (the query filters run_reviews.user_id = caller)
// and mounted on RequireUser so `uzi review stats` works from a CLI token. The counts are
// bucketed by the SAME Go ladder as the per-review DTO, so the strip and the bar agree.
func (h *Handler) JudgeStats(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stats, err := h.wsvc.JudgeTriageStats(r.Context(), user.ID)
	if err != nil {
		slog.Error("judge triage stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// JudgeCategoryStats serves the caller's per-category GROUP counts — the Judge filter-chip
// counts (PRD #244). A SEPARATE endpoint from JudgeStats so the nav badge (which reads only
// TriageCounts.todo from /me/judge/stats) is structurally unreachable from category data.
// Owner-scoped (the query filters run_reviews.user_id = caller) and mounted on RequireUser
// like /me/judge/stats. The count is whole-backlog, uncapped and triage-invariant, so the
// Judge page fetches it once on mount.
func (h *Handler) JudgeCategoryStats(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stats, err := h.wsvc.JudgeCategoryStats(r.Context(), user.ID)
	if err != nil {
		slog.Error("judge category stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}
