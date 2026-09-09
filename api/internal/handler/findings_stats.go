package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// FindingsStats serves the Findings per-status tally (PRD #1183 M3): GET /api/findings/stats, the
// finding twin of GET /api/me/judge/stats, reusing apitypes.TriageDTO. RequireUser (so `uzi
// findings stats` works from a CLI token), owner-scoped by the query's user_id filter, read-only
// (no forge write, no token spend).
//
// ?repo=<uuid> narrows by coordinate repo — an unparseable id is a 400 (a typo can never look like
// an empty tally), while a well-formed but foreign/unknown repo returns all-zero counts, leaking no
// existence oracle. It IGNORES ?run= entirely (the param is not read): the run anchor narrows the
// LIST, never the counts, the same divergence the Judge page already accepts, so the nav badge, the
// counted tabs and the summary strip are one number per repo scope.
func (h *Handler) FindingsStats(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var repoFilter uuid.UUID
	if raw := r.URL.Query().Get("repo"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo id")
			return
		}
		repoFilter = parsed
	}

	stats, err := h.wsvc.FindingsStats(r.Context(), user.ID, repoFilter)
	if err != nil {
		slog.Error("findings stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}
