package handler

import (
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
)

// ControllerPoll serves the hosted-worker controller's desired-state poll (PRD
// #58 Decision 2). Authenticated by mw.RequireController on the route group; the
// credential is whole-fleet scoped, so there is no per-row authz here — this
// endpoint exists only when hosting is enabled, and only the controller can reach
// it.
//
// A GET with no request body. It was a POST when the body carried the controller's
// delivery acks; those are gone (delivery is now proved by the worker's own
// registration — see hostedsvc/protocol.go), leaving a pure read, which is what a
// GET is for. No CSRF step: the route group is bearer-authenticated, not
// cookie-authenticated, exactly like /api/worker/*.
func (h *Handler) ControllerPoll(w http.ResponseWriter, r *http.Request) {
	resp, err := h.hsvc.Poll(r.Context())
	if err != nil {
		slog.Error("controller poll", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// This response body carries join-token PLAINTEXT. A POST was never cacheable;
	// a GET is, so say so explicitly. Nothing sits between the controller and the
	// api today (Decision 4: workers and the controller dial it directly, no nginx
	// in the path), so this is prophylaxis against a future mesh or sidecar rather
	// than a live bug — which is exactly when it is cheap to get right.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, resp)
}
