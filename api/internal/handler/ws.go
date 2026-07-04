package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// wsWriteTimeout bounds a single frame write; a browser that stalls this long is
// dropped rather than blocking the run's fan-out.
const wsWriteTimeout = 10 * time.Second

// wsPingInterval keeps the connection (and any intermediary) alive and detects a
// silently dead peer.
const wsPingInterval = 30 * time.Second

// ServeWS upgrades a browser to a WebSocket subscribed to one run's live events.
// It runs inside the session-authenticated group, so the JWT cookie is already
// validated. Two authorization rules the PRD makes mandatory are enforced here:
//
//   - Origin validation on the upgrade. coder/websocket's default Accept checks
//     Origin == Host (same-origin), which is what defends a cookie-authenticated
//     socket against cross-site WebSocket hijacking. We rely on it explicitly by
//     NOT setting InsecureSkipVerify and NOT widening OriginPatterns; behind nginx
//     the browser origin and the API host are the same.
//   - Per-run authorization on subscribe. The ?run=<id> must be a run the user
//     owns, or the user must be an admin — identical to what the REST endpoints
//     enforce (GetRunForViewer). A denied subscribe fails the handshake before any
//     upgrade, so a non-owner never opens the socket.
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
