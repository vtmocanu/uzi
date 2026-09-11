package handler

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// AdminJudgeRecommendations serves the admin "All users" grouped backlog (PRD #1184 M1): every
// recommendation across EVERY user's runs, deduped by (category, target), attribution hidden.
// It is the cross-user twin of JudgeRecommendations, mounted in the admin READ group
// (RequireUser + RequireAdminRO), so a uza_ admin_ro token reads it and a masked uzc_/session
// non-admin is 403 before the handler runs. The owner endpoint is untouched, so its "IsAdmin is
// never consulted" invariant holds.
//
// ?bucket=todo|filed|done|dismissed|all (default todo) filters by the GROUP rollup;
// ?category= is a comma-separated list of recommendation labels (OR within the set), validated
// against the ingest-time taxonomy and pushed down before the row cap. There is deliberately NO
// ?run= anchor here — an anchor names a run, which is attribution. Both params are validated
// the same way as the owner handler (an unknown bucket or category is a 400, an absent/empty
// ?category= is "all labels"), so a typo can never look like an empty backlog.
func (h *Handler) AdminJudgeRecommendations(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = workersvc.BucketTodo
	}
	if !workersvc.ValidJudgeBacklogBucket(bucket) {
		httpx.Error(w, http.StatusBadRequest, "invalid bucket")
		return
	}

	// NORMALIZE BEFORE VALIDATING, DEDUP AS WE PARSE — the same closed-taxonomy contract as the
	// owner handler (judge_recommendations.go): drop empty tokens (so a present-but-empty
	// ?category= is "all labels", not a 400), 400 on any unknown label, and keep the result NIL
	// (never []string{}) so it maps to SQL NULL / all labels rather than an empty array that
	// matches nothing. Duplicated rather than shared because the owner handler must stay
	// byte-identical.
	var categories []string
	if raw := r.URL.Query().Get("category"); raw != "" {
		seen := make(map[string]bool)
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if !workersvc.ValidRecommendationCategory(tok) {
				httpx.Error(w, http.StatusBadRequest, "invalid category")
				return
			}
			if seen[tok] {
				continue
			}
			seen[tok] = true
			categories = append(categories, tok)
		}
	}

	backlog, err := h.wsvc.AdminJudgeRecommendationBacklog(r.Context(), bucket, categories)
	if err != nil {
		slog.Error("admin judge recommendation backlog", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, backlog)
}

// AdminJudgeStats serves the admin "All users" triage strip (PRD #1184 M1): the cross-user
// tally, bucketed through the same shared ladder as the owner strip. Mounted in the admin READ
// group; a uza_ token reads it. Attribution-free by construction — it is a count.
func (h *Handler) AdminJudgeStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stats, err := h.wsvc.AdminJudgeTriageStats(r.Context())
	if err != nil {
		slog.Error("admin judge triage stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// AdminJudgeCategoryStats serves the admin "All users" filter-chip counts (PRD #1184 M1): the
// cross-user bucket → category → count matrix. Mounted in the admin READ group. There is no
// ?run= anchor on the admin path, so unlike JudgeCategoryStats this handler parses no params.
func (h *Handler) AdminJudgeCategoryStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stats, err := h.wsvc.AdminJudgeCategoryStats(r.Context())
	if err != nil {
		slog.Error("admin judge category stats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}
