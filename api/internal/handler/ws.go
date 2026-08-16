package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// wsWriteTimeout bounds a single frame write; a browser that stalls this long is
// dropped rather than blocking the run's fan-out.
const wsWriteTimeout = 10 * time.Second

// wsPingInterval keeps the connection (and any intermediary) alive and detects a
// silently dead peer.
const wsPingInterval = 30 * time.Second

// ServeWS upgrades a caller to a WebSocket subscribed to one run's live events.
//
// It runs inside a RequireUser group (PRD #112 M1), so the credential is EITHER a
// validated session JWT cookie OR a user-scoped CLI token (uzc_/uza_) — the same
// dual guard the run READ routes use. RequireUser dispatches on credential presence
// at parse time and populates the same context key with the same store.User type
// either way, which is why nothing below branches on which one arrived: an auth-type
// branch here would be a second copy of that dispatch predicate, free to drift.
//
// Same context, same authz call — but NOT the same reach, and the asymmetry is
// deliberate. RequireUser clears IsAdmin on any token whose scope is not admin_ro
// (middleware/cli_auth.go:85-87), so an admin holding a cookie subscribes to any run
// through GetRunForViewer's admin branch while the SAME admin's default-scope uzc_ is
// owner-only and gets the 404 below. Admitting Bearer therefore NARROWS what a given
// person can reach, never widens it, which is why no handler change was needed.
//
// Two authorization rules the PRD makes mandatory are enforced here:
//
//   - Origin validation on the upgrade. coder/websocket's default Accept checks
//     Origin == Host (same-origin). We rely on it explicitly by NOT setting
//     InsecureSkipVerify and NOT widening OriginPatterns; behind nginx the browser
//     origin and the API host are the same. This one unchanged rule covers both
//     credential paths — see below.
//   - Per-run authorization on subscribe. The ?run=<id> must be a run the user
//     owns, or the user must be an admin — identical to what the REST endpoints
//     enforce (GetRunForViewer). A denied subscribe fails the handshake before any
//     upgrade, so a non-owner never opens the socket.
//
// Why the same-origin rule still holds now that Bearer is admitted, in two
// independent halves:
//
//   - A browser-less client sends NO Origin header, and coder/websocket's
//     authenticateOrigin returns nil for an empty Origin (v1.8.14 accept.go:228-232,
//     reached from accept.go:116-117 whenever InsecureSkipVerify is false). Its own
//     Dial never sets one. So a Bearer upgrade passes the default check unmodified:
//     nothing has to be skipped to let the CLI in. Note ABSENT is the only value that
//     passes for free — "Origin: null", what a sandboxed iframe or a data: page sends,
//     parses to a URL with no host and is rejected at accept.go:256-258. A browser
//     cannot reach the exemption by stripping its own origin.
//   - A cross-site browser page CANNOT attach an Authorization header (the browser
//     WebSocket API forbids custom headers), so it can only present the ambient
//     cookie. It therefore stays on the cookie path, sends its own foreign Origin,
//     and is still rejected — the cross-site WebSocket hijacking defense the cookie
//     needs is byte-for-byte what it was before the route moved.
//
// The socket is a live channel only: every frame it carries was already persisted
// to run_messages (messages) or applied to runs (state) before the hub was poked,
// so a dropped or missed frame is recovered by the client's REST replay — the WS
// is never the source of truth.
func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, err := uuid.Parse(r.URL.Query().Get("run"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "run query parameter must be a run id")
		return
	}
	// Per-run authz BEFORE the upgrade: owner or admin, else the same 404 the REST
	// endpoints return for a run the viewer may not see.
	if _, err := h.wsvc.GetRunForViewer(r.Context(), user.ID, user.IsAdmin, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("ws authorize run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Clear the http.Server's Read/Write deadlines before hijacking: a long-lived
	// socket must not be killed by the server's per-request WriteTimeout. Best
	// effort — coder/websocket manages its own per-op deadlines regardless.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		// Accept already wrote the failure (e.g. 403 on an Origin mismatch).
		slog.Debug("ws accept", "error", err)
		return
	}
	defer conn.CloseNow()

	sub := h.hub.Subscribe(runID)
	defer sub.Close()

	// CloseRead drains and discards anything the client sends (this is a
	// server-push channel) and cancels connCtx when the socket closes, which is
	// how we detect the browser going away.
	connCtx := conn.CloseRead(r.Context())

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-connCtx.Done():
			return
		case frame, ok := <-sub.Events():
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(connCtx, wsWriteTimeout)
			err := conn.Write(wctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(connCtx, wsWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
