package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// setGuardrailOverrideRequest is the POST body: the admin's reason for allowing this
// one repo through the #66 guardrail. Required and non-empty (D8) — the override is a
// recorded accept-risk decision, so it must carry a reason.
type setGuardrailOverrideRequest struct {
	Reason string `json:"reason"`
}

// maxGuardrailOverrideReasonBytes caps the audit note so it cannot be used to bloat
// the repos row or a rendered surface. A sentence or two is the intent.
const maxGuardrailOverrideReasonBytes = 1000

// SetRepoGuardrailOverride is the admin per-repo guardrail override write (PRD #66 D8,
// M8). It is ADMIN-ONLY with NO member path: mounted under the admin WRITE group
// (RequireAuth + RequireAdmin) and backed by the UNSCOPED-by-id SetRepoGuardrailOverride
// query — there is deliberately no `...ForUser` variant, because a member self-allowing
// is exactly the R6 route-around D8 forbids (owner included, even for a repo they own).
//
// The actor id and timestamp are taken from the session and now(), never the request
// body (audit). reason is required non-empty (400 otherwise). An unknown id → 404
// (pgx.ErrNoRows from the :one RETURNING). This stores the override; the shared
// evaluator's post-evaluation downgrade (M2) and the three gates (M4/M5/M6, threaded in
// M8) do the enforcement — this never itself waives protection_unreadable (D8/D3).
func (h *Handler) SetRepoGuardrailOverride(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	var req setGuardrailOverrideRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The reason validation is the SHARED validateGuardrailReason (PRD #1432): it
	// trims, rejects an empty (400), over-long (422), or control-character (400)
	// reason, and returns the clean value. Both this admin write and the member
	// request (RequestGuardrailOverride) call the same helper so they cannot drift
	// on what a legal audit note is (the M8 write-side backstop rationale lives on
	// the helper's doc comment).
	reason, status, msg := validateGuardrailReason(req.Reason)
	if status != 0 {
		httpx.Error(w, status, msg)
		return
	}

	repo, err := h.q.SetRepoGuardrailOverride(r.Context(), store.SetRepoGuardrailOverrideParams{
		ID:                      id,
		GuardrailOverrideReason: pgtype.Text{String: reason, Valid: true},
		GuardrailOverrideBy:     pgconv.UUID(user.ID),
		GuardrailOverrideAt:     pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("admin set repo guardrail override", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repo": repoToDTO(repo)})
}

// ClearRepoGuardrailOverride is the admin revoke of a per-repo guardrail override (PRD
// #66 D8, M8): it NULLs all three columns, re-arming the guardrail immediately at the
// next gate call. Same admin-only, unscoped-by-id shape and 404-on-unknown-id contract
// as SetRepoGuardrailOverride.
func (h *Handler) ClearRepoGuardrailOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return
	}
	repo, err := h.q.ClearRepoGuardrailOverride(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("admin clear repo guardrail override", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"repo": repoToDTO(repo)})
}
