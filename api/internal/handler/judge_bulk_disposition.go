package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// BulkSetDispositions applies one triage verdict to every member coordinate of the given
// groups (PRD #98 M2, Decision 3) — the Judge menu's per-group Mark done / Dismiss and the
// multi-select bar, in a single round-trip.
//
// Mounted on RequireUser beside the rest of /me/judge, so `uzi review resolve|dismiss`
// (M7) drives it from a CLI token. It is a local upsert applied N times: no token spend, no
// forge write, nothing that needs a limiter beyond the item cap.
//
// OWNER-ONLY BY CONSTRUCTION — the service resolves members with a `user_id = caller`
// WHERE and there is no ownership branch to get wrong; IsAdmin is never consulted, so a
// uza_ admin_ro token disposes its own rows and silently matches nothing on anyone else's.
// That silence is the point: a coordinate that does not exist and one that belongs to
// another user are indistinguishable in the response (#94 Decision 5's one-404 rule). There
// is deliberately no per-item status array — with coordinates there is no id to 404 on, and
// reporting per-item outcomes would rebuild exactly the existence oracle that rule forbids.
//
// The enum is validated here (bad status/reason/scope → 400), as is the item count; the
// table CHECK is the backstop. Success is 200 with the updated groups + recomputed triage,
// which is what lets the page update its rows and its badge without a follow-up GET.
func (h *Handler) BulkSetDispositions(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// The body type is shared with the CLI (apitypes), not declared locally: the client
	// encodes the same struct this decodes, and DecodeJSON runs with DisallowUnknownFields,
	// so a key that matched on only one side would be a 400 rather than a silent no-op.
	var req apitypes.JudgeBulkDispositionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Items) == 0 {
		httpx.Error(w, http.StatusBadRequest, "items required")
		return
	}
	// The SAME validator the single-coordinate #94 route uses: done carries no reason;
	// dismissed requires wont_do | not_an_issue. One enum, one gate, no drift.
	if !validDisposition(req.Status, req.Reason) {
		httpx.Error(w, http.StatusBadRequest, "invalid status or reason")
		return
	}
	scope := req.Scope
	if scope == "" {
		// The default: settle what is open, never re-assert a settled member. Referenced,
		// not spelled — if ScopeOpen's wire value ever changed, a literal here would
		// silently become an invalid scope and every default-scope request would 400.
		scope = workersvc.ScopeOpen
	}
	if !workersvc.ValidJudgeDispositionScope(scope) {
		httpx.Error(w, http.StatusBadRequest, "invalid scope")
		return
	}

	res, err := h.wsvc.BulkSetDispositions(r.Context(), user.ID, req.Items, req.Status, req.Reason, scope)
	if err != nil {
		if errors.Is(err, workersvc.ErrTooManyItems) {
			httpx.Error(w, http.StatusBadRequest, "too many items")
			return
		}
		// Unreachable while the scope check above stands — the service refuses an unknown
		// scope rather than defaulting to the destructive `all`, and this maps that refusal
		// to the same 400 the handler would have produced. Two layers, neither load-bearing
		// alone.
		if errors.Is(err, workersvc.ErrInvalidScope) {
			httpx.Error(w, http.StatusBadRequest, "invalid scope")
			return
		}
		slog.Error("bulk set dispositions", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}
