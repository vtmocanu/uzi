package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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

// AdminSetJudgeDisposition is the admin cross-user "Mark done" (PRD #1184 M2): PUT
// /api/admin/judge/recommendations/disposition marks every user's OPEN member of each requested
// (category, target) coordinate done, stamped set_via='admin' and set_by_user_id=<the admin>,
// with ON CONFLICT DO NOTHING so a human's existing verdict is never overwritten.
//
// It is mounted in the admin WRITE group (RequireAuth + RequireAdmin), which is cookie-only: a
// uza_ admin_ro Bearer token structurally 401s at RequireAuth before this handler exists, so the
// aggregate stays CLI-readable while the writes are not (§379's read/write split). There is no
// forge limiter — this is a local upsert, no token spend and no forge write.
//
// The body is JudgeAdminDispositionRequest, decoded with DisallowUnknownFields, so a stray
// `scope` or `reason` field is a 400 rather than a silent no-op. Status carries ONLY "done":
// there is no cross-user Dismiss (dismissing another user's recommendation is their judgment), so
// any other value — including "dismissed" — is a 400. Items must be non-empty and are capped at
// the owner coordinate cap (JudgeDispositionMaxItems); dedup and the cap live in the service,
// shared with the owner path. Success is 200 with the updated groups + recomputed all-users
// triage, carrying NO run address (the response has no `settled` list).
func (h *Handler) AdminSetJudgeDisposition(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.JudgeAdminDispositionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		// A stray scope/reason field, an unknown key, or malformed JSON all land here — the
		// strict decoder is what makes `scope: all` a 400 rather than an ignored field.
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// "done" ONLY. "dismissed" (and anything else) is a 400 — the cross-user path offers no
	// Dismiss, by decision. Checked BEFORE the empty-items check so a `status: dismissed` body is
	// unambiguously rejected as the wrong verdict.
	if req.Status != "done" {
		httpx.Error(w, http.StatusBadRequest, "status must be done")
		return
	}
	if len(req.Items) == 0 {
		httpx.Error(w, http.StatusBadRequest, "items required")
		return
	}

	res, err := h.wsvc.AdminMarkDone(r.Context(), user.ID, req.Items)
	if err != nil {
		if errors.Is(err, workersvc.ErrTooManyItems) {
			httpx.Error(w, http.StatusBadRequest, "too many items")
			return
		}
		slog.Error("admin set judge disposition", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// AdminUndoJudgeDisposition is the admin cross-user Undo (PRD #1184 M2): DELETE
// /api/admin/judge/recommendations/disposition removes ONLY the set_via='admin' rows on each
// requested coordinate, across every user, leaving any human done/dismissed verdict intact. Same
// cookie-only admin WRITE group as the PUT above.
//
// The body is the SAME JudgeAdminDispositionRequest, decoded with the same strict decoder (so a
// stray `scope`/`reason` is still a 400), but Status is IGNORED here — the undo is coordinate-only,
// there is no verdict to set. Items must be non-empty. Success is 200 with the same
// attribution-hidden result shape as the PUT. (A DELETE carrying a JSON body is unusual but
// well-formed; net/http reads it regardless of method.)
func (h *Handler) AdminUndoJudgeDisposition(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	_ = user // authorization is the route's RequireAdmin; the undo is coordinate-scoped, not per-user.
	var req apitypes.JudgeAdminDispositionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Items) == 0 {
		httpx.Error(w, http.StatusBadRequest, "items required")
		return
	}

	res, err := h.wsvc.AdminUndoDone(r.Context(), req.Items)
	if err != nil {
		if errors.Is(err, workersvc.ErrTooManyItems) {
			httpx.Error(w, http.StatusBadRequest, "too many items")
			return
		}
		slog.Error("admin undo judge disposition", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}
