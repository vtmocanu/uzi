package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The usage-limit park opt-in has TWO surfaces (PRD #35 Decision 7), and the split
// is the user's own ruling of 2026-07-27 rather than an implementation convenience:
//
//	PUT /api/me/wait-on-limit        the per-user DEFAULT a new run inherits
//	PUT /api/runs/{id}/wait-on-limit ONE existing run's flag, while it is non-terminal
//
// The per-run half replaced a start-run modal that was the PRD's original design.
// The coverage argument is what carried it and is worth restating: a modal reaches
// only human-started runs, while autopilot, ci_fix and self_improve runs have NO
// start affordance at all — and two of those three park. The toggle is a strictly
// LARGER surface than the modal it replaced, not a reduced one, and it also matches
// when a user actually forms the opinion: looking at a run, not before it exists.
//
// Both clone the autopilot plumbing (handler/autopilot.go) exactly: `{"enabled":
// bool}` in, the updated entity out. Deliberately not a PATCH of a larger settings
// object — one switch, one route, one predicate to reason about.

type setWaitOnLimitRequest struct {
	Enabled bool `json:"enabled"`
}

// SetUserWaitOnLimit flips the caller's per-user default: whether a NEW run parks
// rather than fails when their Anthropic usage window is exhausted.
//
// Scoped strictly to the authenticated user, like autopilot and judge — there is no
// admin path to toggle it for someone else. It is consent to a run holding an
// issue's one-active lock and a worker's disk for up to RUN_LIMIT_MAX_PARK, which is
// the caller's resource to spend and nobody else's to spend for them.
//
// It changes NOTHING about runs that already exist: every run carries its own
// runs.wait_on_limit, stamped at creation. That is why this is a default rather than
// a policy, and why the per-run route below exists at all.
func (h *Handler) SetUserWaitOnLimit(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setWaitOnLimitRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserWaitOnLimit(r.Context(), store.SetUserWaitOnLimitParams{
		ID:          user.ID,
		WaitOnLimit: req.Enabled,
	})
	if err != nil {
		slog.Error("set user wait-on-limit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}

// SetRunWaitOnLimit flips ONE run's opt-in while it is still non-terminal.
//
// 🔴 IT CHANGES FUTURE LIMIT BEHAVIOUR ONLY, NEVER THE RUN'S CURRENT STATUS.
// Flipping it off on a PARKED run does not un-park or cancel it — Decision 11's
// cancel is that control, and quietly failing someone's run because they changed a
// preference would destroy work they never asked to lose. Flipping it on while
// parked is a no-op for the symmetric reason: the run is already parked. The flag is
// re-read at the next limit event and re-delivered on every claim, so a mid-flight
// change takes effect on the next park decision without this route touching the
// state machine.
//
// A terminal run is a no-op that reports success rather than an error: the toggle
// governs future limit behaviour and a finished run has none, so "your change had no
// effect because there is nothing left to affect" is not a failure the caller can
// act on. The response carries the run, so a client that cares can see the flag did
// not move.
//
// Ownership is the SQL predicate, not a pre-read: a foreign run yields 0 rows and
// 404 — never 403, which would confirm the run exists to someone who cannot see it.
func (h *Handler) SetRunWaitOnLimit(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req setWaitOnLimitRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := h.q.SetRunWaitOnLimit(r.Context(), store.SetRunWaitOnLimitParams{
		ID: runID, UserID: user.ID, WaitOnLimit: req.Enabled,
	}); err != nil {
		slog.Error("set run wait-on-limit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Re-read owner-scoped rather than trusting the write's row count to tell the two
	// zero-row causes apart. 0 rows means EITHER "not yours / does not exist" OR "it
	// is terminal", and those are a 404 and a 200 respectively — the count alone
	// cannot distinguish them, so the read is what decides.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}
