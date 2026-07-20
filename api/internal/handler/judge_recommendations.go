package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// JudgeRecommendations serves the Judge menu's grouped backlog (PRD #98 M1, Decision 1):
// every recommendation across all of the caller's runs, deduped by (category, target),
// each group carrying its per-run occurrences, open_count, run_count and the most-recent
// rationale preview.
//
// Mounted on RequireUser next to /me/judge/stats, so `uzi review backlog` (M7) works from
// a CLI token. Owner-scoped by the query's user_id filter — IsAdmin is never consulted, so
// a uza_ admin_ro token reads its OWN backlog and nothing else. Read-only: no token spend,
// no forge write.
//
// ?bucket=todo|filed|done|dismissed|all (default todo) filters by the GROUP rollup;
// ?run=<uuid> is the notification deep-link anchor (/judge?run={id}) and keeps only groups
// that recur in that run — pushed down into the query's owner-scoped WHERE, so an anchor
// naming another user's run matches nothing. Both are validated here — an unknown bucket or
// an unparseable run id is a 400 rather than a silently-ignored filter, so a typo in a CLI
// flag can never look like an empty backlog. A well-formed but unknown/foreign run id is
// NOT an error: it returns an empty list, leaking no existence oracle.
//
// The pull is bounded by a hard row cap; the response's `truncated` flag says when it bit,
// so no consumer presents a cut backlog as complete. `triage` is never affected by either
// filter or by truncation — it is the canonical GET /me/judge/stats aggregate.
func (h *Handler) JudgeRecommendations(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "todo" // the backlog's reason to exist: what still needs triage
	}
	if !workersvc.JudgeBacklogBuckets[bucket] {
		httpx.Error(w, http.StatusBadRequest, "invalid bucket")
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

	backlog, err := h.wsvc.JudgeRecommendationBacklog(r.Context(), user.ID, bucket, runAnchor)
	if err != nil {
		slog.Error("judge recommendation backlog", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, backlog)
}
