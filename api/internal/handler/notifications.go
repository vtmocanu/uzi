package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Notifications inbox (PRD #46 Decision 6, M2). Scoping mirrors the owner-or-admin
// GetRunForViewer rule: the list + unread count are session-user-scoped; the admin
// all-view (?all=1) is gated by IsAdmin and carries each row's owner; mark-read
// matches on (id, user_id) so a non-owner's id is a 404 exactly like an unknown id.

const (
	// defaultNotifLimit is the inbox page size when none is given; maxNotifLimit
	// caps a caller-supplied one so a single page can't pull the whole table.
	defaultNotifLimit = 30
	maxNotifLimit     = 100
)

// notificationOwner is the owning user, included only in the admin all-view so the
// admin sees whose inbox each row belongs to.
type notificationOwner struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name"`
}

// notificationDTO is the JSON view of a notification row. payload is the generic
// jsonb render blob, forwarded verbatim as raw JSON (the kind tells the SPA how to
// read it). run_id / review_id are optional deep-link anchors.
type notificationDTO struct {
	ID        string             `json:"id"`
	Kind      string             `json:"kind"`
	Payload   json.RawMessage    `json:"payload"`
	RunID     *string            `json:"run_id"`
	ReviewID  *string            `json:"review_id"`
	ReadAt    *time.Time         `json:"read_at"`
	CreatedAt time.Time          `json:"created_at"`
	Owner     *notificationOwner `json:"owner,omitempty"`
}

func toNotificationDTO(n store.Notification) notificationDTO {
	dto := notificationDTO{
		ID:        n.ID.String(),
		Kind:      n.Kind,
		Payload:   rawPayload(n.Payload),
		RunID:     uuidPtrString(n.RunID),
		ReviewID:  uuidPtrString(n.ReviewID),
		CreatedAt: n.CreatedAt.Time,
	}
	if n.ReadAt.Valid {
		t := n.ReadAt.Time
		dto.ReadAt = &t
	}
	return dto
}

func toAllNotificationDTO(n store.ListAllNotificationsRow) notificationDTO {
	dto := notificationDTO{
		ID:        n.ID.String(),
		Kind:      n.Kind,
		Payload:   rawPayload(n.Payload),
		RunID:     uuidPtrString(n.RunID),
		ReviewID:  uuidPtrString(n.ReviewID),
		CreatedAt: n.CreatedAt.Time,
		Owner: &notificationOwner{
			ID:          n.UserID.String(),
			Email:       n.OwnerEmail,
			DisplayName: textPtrValue(n.OwnerDisplayName.Valid, n.OwnerDisplayName.String),
		},
	}
	if n.ReadAt.Valid {
		t := n.ReadAt.Time
		dto.ReadAt = &t
	}
	return dto
}

// rawPayload forwards the stored jsonb as raw JSON, defaulting an empty column to
// an object so the SPA always receives a valid JSON value.
func rawPayload(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}

// uuidPtrString renders an optional pgtype.UUID anchor as *string (nil = null).
func uuidPtrString(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// ListNotifications serves the caller's own inbox newest-first, paginated. With
// ?all=1 an admin gets every user's notifications (each carrying its owner); a
// non-admin asking for ?all=1 is forbidden. The envelope also carries the caller's
// own unread count so the page's badge needs no second call, plus the scope total
// for paging.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	lim, off, err := parseNotifPage(r)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	all := r.URL.Query().Get("all") == "1"
	if all && !user.IsAdmin {
		httpx.Error(w, http.StatusForbidden, "admin only")
		return
	}

	// The unread badge is always the caller's OWN count, in both scopes.
	unread, err := h.q.CountUnreadNotificationsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("count unread notifications", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	if all {
		rows, err := h.q.ListAllNotifications(r.Context(), store.ListAllNotificationsParams{Lim: lim, Off: off})
		if err != nil {
			slog.Error("list all notifications", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		total, err := h.q.CountAllNotifications(r.Context())
		if err != nil {
			slog.Error("count all notifications", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := make([]notificationDTO, 0, len(rows))
		for _, n := range rows {
			out = append(out, toAllNotificationDTO(n))
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"notifications": out, "unread": unread, "total": total})
		return
	}

	rows, err := h.q.ListNotificationsForUser(r.Context(), store.ListNotificationsForUserParams{
		UserID: user.ID, Lim: lim, Off: off,
	})
	if err != nil {
		slog.Error("list notifications", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	total, err := h.q.CountNotificationsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("count notifications", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]notificationDTO, 0, len(rows))
	for _, n := range rows {
		out = append(out, toNotificationDTO(n))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"notifications": out, "unread": unread, "total": total})
}

// UnreadNotificationCount is the bell's lightweight poll: just the caller's own
// unread number, no rows. Always personal — even for an admin the badge counts the
// admin's own unread, never the all-view.
func (h *Handler) UnreadNotificationCount(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	unread, err := h.q.CountUnreadNotificationsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("count unread notifications", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"unread": unread})
}

// MarkNotificationRead marks one of the caller's own notifications read. Ownership
// is the (id, user_id) match in the query (audit M2): another user's id — or an
// unknown one — matches zero rows and is a 404. Idempotent for an already-read row.
func (h *Handler) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "notification")
	if !ok {
		return
	}
	row, err := h.q.MarkNotificationRead(r.Context(), store.MarkNotificationReadParams{ID: id, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "notification not found")
		return
	}
	if err != nil {
		slog.Error("mark notification read", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"notification": toNotificationDTO(row)})
}

// parseNotifPage reads and validates ?limit=&offset=. limit defaults to
// defaultNotifLimit and is clamped to maxNotifLimit; offset defaults to 0. A
// malformed or out-of-range value is a 400.
func parseNotifPage(r *http.Request) (lim, off int32, err error) {
	lim = defaultNotifLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		v, e := strconv.Atoi(s)
		if e != nil || v < 1 {
			return 0, 0, errors.New("invalid limit")
		}
		if v > maxNotifLimit {
			v = maxNotifLimit
		}
		lim = int32(v) //nolint:gosec // G109: v is clamped to [1, maxNotifLimit] just above
	}
	if s := r.URL.Query().Get("offset"); s != "" {
		v, e := strconv.Atoi(s)
		if e != nil || v < 0 {
			return 0, 0, errors.New("invalid offset")
		}
		off = int32(v) //nolint:gosec // G109: pagination offset, validated non-negative; a pathological value causes a query error, not unsafe behavior
	}
	return lim, off, nil
}
