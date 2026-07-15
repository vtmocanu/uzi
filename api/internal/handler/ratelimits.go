package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
)

// rateLimitWindow is one window of the frozen DTO (PRD #53): a 0..100 percent plus
// the reset as epoch seconds, or null when Anthropic reported none. resets_at has
// no omitempty — the contract keeps it PRESENT as `null` inside every window.
type rateLimitWindow struct {
	Pct      int    `json:"pct"`
	ResetsAt *int64 `json:"resets_at"`
}

// rateLimitDTO is the frozen union discriminated on status. Only the "ok" branch
// carries the windows/source/synced_at/stale; "no_token" and "unavailable" are
// status-only — omitempty drops the ok-only fields so they serialize to exactly
// {"status": "..."}. Exposes ONLY pct/resets_at/status/source/synced_at/stale, per
// the frozen contract (no is_admin, no internal fields).
type rateLimitDTO struct {
	Status   string           `json:"status"` // ok | no_token | unavailable
	FiveHour *rateLimitWindow `json:"five_hour,omitempty"`
	SevenDay *rateLimitWindow `json:"seven_day,omitempty"`
	Source   string           `json:"source,omitempty"`
	SyncedAt string           `json:"synced_at,omitempty"`
	Stale    *bool            `json:"stale,omitempty"`
}

// adminRateLimitRowDTO is one user's row on the admin view: identity + the live
// vault lock state + the same union as /me. Every user appears, including
// no_token ones.
type adminRateLimitRowDTO struct {
	ID          string       `json:"id"`
	Email       string       `json:"email"`
	Name        string       `json:"name"`
	VaultLocked bool         `json:"vault_locked"`
	Limits      rateLimitDTO `json:"limits"`
}

const (
	rateLimitStatusOK          = "ok"
	rateLimitStatusNoToken     = "no_token"
	rateLimitStatusUnavailable = "unavailable"
)

// SelfRateLimits returns the caller's own two Claude rate-limit meters (PRD #53).
// Session-authed, scoped to the caller. no_token is derived from secret-existence
// (not the gauge row being absent), so a stale/failed row cleanup still reads as
// no_token; a token with no reading yet (fresh save, probe disabled, refused
// credential) reads as unavailable.
func (h *Handler) SelfRateLimits(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	hasToken, err := h.q.UserHasAnthropicToken(r.Context(), user.ID)
	if err != nil {
		slog.Error("rate limits: token existence", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !hasToken {
		httpx.JSON(w, http.StatusOK, rateLimitDTO{Status: rateLimitStatusNoToken})
		return
	}
	row, err := h.q.GetRateLimits(r.Context(), user.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.JSON(w, http.StatusOK, rateLimitDTO{Status: rateLimitStatusUnavailable})
		return
	}
	if err != nil {
		slog.Error("rate limits: get", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, h.okRateLimitDTO(
		row.FiveHourPct, row.FiveHourResetsAt,
		row.SevenDayPct, row.SevenDayResetsAt,
		row.Source, row.SyncedAt,
	))
}

// AdminRateLimits returns every user's meters + staleness (PRD #53). Admin-only —
// the route is under the RequireAdmin group (mirroring AdminUsage), so a non-admin
// never reaches here (403). Each user carries their live vault lock state and the
// same status union as /me; no_token users are included.
func (h *Handler) AdminRateLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListRateLimits(r.Context())
	if err != nil {
		slog.Error("rate limits: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	users := make([]adminRateLimitRowDTO, 0, len(rows))
	for _, u := range rows {
		var limits rateLimitDTO
		switch {
		case !u.HasToken:
			limits = rateLimitDTO{Status: rateLimitStatusNoToken}
		case !u.SyncedAt.Valid:
			// Token held but no gauge row yet (LEFT JOIN miss): no reading.
			limits = rateLimitDTO{Status: rateLimitStatusUnavailable}
		default:
			limits = h.okRateLimitDTO(
				u.FiveHourPct, u.FiveHourResetsAt,
				u.SevenDayPct, u.SevenDayResetsAt,
				u.Source, u.SyncedAt,
			)
		}
		users = append(users, adminRateLimitRowDTO{
			ID:          u.UserID.String(),
			Email:       u.Email,
			Name:        u.DisplayName.String, // "" when the user has no display name
			VaultLocked: !h.vaultUnlocked(u.UserID),
			Limits:      limits,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
}

// okRateLimitDTO builds the "ok" union branch from the stored gauge columns. stale
// is computed server-side (D3); resets are rendered as epoch seconds (D7).
func (h *Handler) okRateLimitDTO(fivePct pgtype.Int2, fiveReset pgtype.Timestamptz, sevenPct pgtype.Int2, sevenReset pgtype.Timestamptz, source pgtype.Text, syncedAt pgtype.Timestamptz) rateLimitDTO {
	stale := h.rateLimitStale(syncedAt)
	return rateLimitDTO{
		Status:   rateLimitStatusOK,
		FiveHour: &rateLimitWindow{Pct: int(fivePct.Int16), ResetsAt: epochPtr(fiveReset)},
		SevenDay: &rateLimitWindow{Pct: int(sevenPct.Int16), ResetsAt: epochPtr(sevenReset)},
		Source:   source.String,
		SyncedAt: syncedAt.Time.UTC().Format(time.RFC3339),
		Stale:    &stale,
	}
}

// rateLimitStale reports whether a reading is stale (D3): synced_at older than 3×
// the effective poll interval. With the poller disabled (UZI_USAGE_POLL_INTERVAL=0)
// existing rows are still served but always marked stale, since nothing refreshes
// them.
func (h *Handler) rateLimitStale(syncedAt pgtype.Timestamptz) bool {
	interval := h.cfg.UsagePollInterval
	if interval <= 0 {
		return true
	}
	if !syncedAt.Valid {
		return true
	}
	return time.Since(syncedAt.Time) > 3*interval
}

// epochPtr renders a stored reset timestamp as epoch seconds, or nil (→ JSON null)
// when Anthropic reported no reset.
func epochPtr(t pgtype.Timestamptz) *int64 {
	if !t.Valid {
		return nil
	}
	secs := t.Time.Unix()
	return &secs
}
