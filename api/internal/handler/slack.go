package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/slacksvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// slackMemberIDPattern is a light shape check on a manual override id (Slack
// member ids are short alphanumerics like U0123ABCD). It rejects whitespace and
// oversized/garbage input before a write or a DM; the real id is verified when the
// confirmation DM is sent.
var slackMemberIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// slackLinkDTO is the current user's own Slack linking state (PRD #25 M3),
// content-minimized to the linking facts the Notifications settings need. state
// is derived: unlinked (no resolved id) | pending (resolved, not confirmed) |
// confirmed. member_id is the manual override (null = rely on email auto-match).
type slackLinkDTO struct {
	MemberID   *string `json:"member_id"`
	Notify     bool    `json:"notify"`
	ResolvedID *string `json:"resolved_id"`
	Confirmed  bool    `json:"confirmed"`
	State      string  `json:"state"`
	// workspace is the collapsed, non-secret Slack workspace connection state
	// (PRD #56 M1): unconfigured | connecting | connected | error.
	Workspace string `json:"workspace"`
}

// publicSlackState collapses the five admin-only manager states (slacksvc.State*)
// to the four public values the Notifications settings surface. The two error
// classes fold to a single "error" so the auth-vs-connection distinction — an
// admin diagnostic — never leaks to a non-admin (PRD #56 Decision 2). Any
// unknown or empty input fails safe to "unconfigured".
func publicSlackState(s string) string {
	switch s {
	case slacksvc.StateConnecting:
		return "connecting"
	case slacksvc.StateConnected:
		return "connected"
	case slacksvc.StateErrorAuth, slacksvc.StateErrorConnection:
		return "error"
	default: // StateDisabled, empty, or any unexpected value
		return "unconfigured"
	}
}

func slackLinkStateOf(resolvedValid, confirmedValid bool) string {
	switch {
	case !resolvedValid:
		return "unlinked"
	case confirmedValid:
		return "confirmed"
	default:
		return "pending"
	}
}

// writeSlackLink renders the linking DTO from the four columns every Slack link
// query returns, so the GET and the PUTs never drift.
func writeSlackLink(w http.ResponseWriter, member, resolved pgtype.Text, notify bool, confirmed pgtype.Timestamptz, workspace string) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"slack": slackLinkDTO{
			MemberID:   textPtrValue(member.Valid, member.String),
			Notify:     notify,
			ResolvedID: textPtrValue(resolved.Valid, resolved.String),
			Confirmed:  confirmed.Valid,
			State:      slackLinkStateOf(resolved.Valid, confirmed.Valid),
			Workspace:  workspace,
		},
	})
}

// GetMySlack returns the current user's own Slack linking state. Session-
// authenticated and own-user only — a user's Slack mapping is theirs.
func (h *Handler) GetMySlack(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	link, err := h.q.GetUserSlackLink(r.Context(), user.ID)
	if err != nil {
		slog.Error("get slack link", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeSlackLink(w, link.SlackMemberID, link.SlackResolvedID, link.SlackNotify, link.SlackLinkConfirmedAt, publicSlackState(h.slackState()))
}

// PutMySlackNotify flips the current user's per-user notification kill switch
// (default on). Own-user only.
func (h *Handler) PutMySlackNotify(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Notify *bool `json:"notify"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil || req.Notify == nil {
		httpx.Error(w, http.StatusBadRequest, "notify (boolean) is required")
		return
	}
	row, err := h.q.SetUserSlackNotify(r.Context(), store.SetUserSlackNotifyParams{
		ID:          user.ID,
		SlackNotify: *req.Notify,
	})
	if err != nil {
		slog.Error("set slack notify", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeSlackLink(w, row.SlackMemberID, row.SlackResolvedID, row.SlackNotify, row.SlackLinkConfirmedAt, publicSlackState(h.slackState()))
}

// PutMySlackOverride sets or clears the manual Slack member-ID override (own-user
// only). Setting an id that collides with another user's effective Slack id is
// rejected with 409 (the unique partial index is the backstop). Because the write
// resets the confirmation, a set also (best-effort) DMs the new target a fresh
// Confirm / Not-me card — content flows only after they confirm, which is what
// keeps a mistyped or squatted id from routing anything to the wrong person.
func (h *Handler) PutMySlackOverride(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// *string over the raw field: a present value sets, null or "" clears. (Absent
	// unmarshals to nil too, which we also treat as a clear — an idempotent no-arg
	// clear is harmless.)
	var req struct {
		MemberID *string `json:"member_id"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	member := ""
	if req.MemberID != nil {
		member = strings.TrimSpace(*req.MemberID)
	}

	var memberParam, resolvedParam pgtype.Text
	if member != "" {
		if !slackMemberIDPattern.MatchString(member) {
			httpx.Error(w, http.StatusBadRequest, "invalid Slack member ID")
			return
		}
		// The override IS the effective resolved id — one write sets both.
		memberParam = pgtype.Text{String: member, Valid: true}
		resolvedParam = memberParam
	}

	row, err := h.q.SetUserSlackOverride(r.Context(), store.SetUserSlackOverrideParams{
		ID:              user.ID,
		SlackMemberID:   memberParam,
		SlackResolvedID: resolvedParam,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "that Slack member ID is already linked to another account")
			return
		}
		slog.Error("set slack override", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// New target must confirm before any content flows; re-send the link DM. The
	// mapping is already stored, so a send failure is logged, not surfaced.
	if member != "" && h.slackLinker != nil {
		h.slackLinker.SendLinkConfirmation(r.Context(), member, user.Email)
	}
	writeSlackLink(w, row.SlackMemberID, row.SlackResolvedID, row.SlackNotify, row.SlackLinkConfirmedAt, publicSlackState(h.slackState()))
}

// PostMySlackTestDM sends a user-initiated test DM to the caller's resolved Slack
// id, so they can verify the link end to end. Own-user only; requires a resolved
// id and a configured Slack.
func (h *Handler) PostMySlackTestDM(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	link, err := h.q.GetUserSlackLink(r.Context(), user.ID)
	if err != nil {
		slog.Error("get slack link for test dm", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !link.SlackResolvedID.Valid || link.SlackResolvedID.String == "" {
		httpx.Error(w, http.StatusBadRequest, "no linked Slack account to send a test DM to")
		return
	}
	if h.slackLinker == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "Slack is not configured")
		return
	}
	if err := h.slackLinker.SendTestDM(r.Context(), link.SlackResolvedID.String); err != nil {
		if errors.Is(err, slacksvc.ErrDMCooldown) {
			w.Header().Set("Retry-After", strconv.Itoa(int(slacksvc.DMTargetCooldown.Seconds())))
			httpx.Error(w, http.StatusTooManyRequests, "a test DM was just sent — please wait a moment before retrying")
			return
		}
		slog.Warn("slack test dm failed", "error", slacksvc.ScrubTokens(err.Error()))
		httpx.Error(w, http.StatusBadGateway, "couldn't send the test DM — check the Slack connection")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// GetAdminSlackStatus returns just the live Slack socket connection state (admin
// only), so the admin webui chip can poll it cheaply instead of re-fetching the
// whole settings blob every few seconds.
func (h *Handler) GetAdminSlackStatus(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"slack_status": h.slackState()})
}
