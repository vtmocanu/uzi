package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
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

// JudgeCategoryStats serves the caller's Judge filter-chip counts — a bucket → category →
// count matrix scoped to the selected triage tab (PRD #270). A SEPARATE endpoint from
// JudgeStats so the nav badge (which reads only TriageCounts.todo from /me/judge/stats) is
// structurally unreachable from category data. Owner-scoped (the query filters
// run_reviews.user_id = caller) and mounted on RequireUser like /me/judge/stats.
//
// ?run=<uuid> is the notification deep-link anchor (/judge?run={id}), parsed exactly like the
// backlog handler: an unparseable value is a 400, while a well-formed but unknown/foreign run
// id matches nothing and returns empty tallies, leaking no existence oracle. The bucket tab is
// NOT a query param here — the response carries every bucket's counts at once, so the frontend
// switches tabs without a refetch, but the counts ARE tab-scoped and triage-variant, so the
// page refetches this after a triage action.
func (h *Handler) JudgeCategoryStats(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var runAnchor uuid.UUID
	if raw := r.URL.Query().Get("run"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid run id")
			return
		}
		runAnchor = parsed
	}
	stats, err := h.wsvc.JudgeCategoryStats(r.Context(), user.ID, runAnchor)
	if err != nil {
		slog.Error("judge category stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}
