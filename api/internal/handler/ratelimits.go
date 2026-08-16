package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// rateLimitWindow (apitypes.RateLimitWindow), rateLimitDTO (apitypes.RateLimitDTO)
// and adminRateLimitRowDTO (apitypes.AdminRateLimitRowDTO) moved to the stdlib-only
// apitypes leaf (PRD #64 M1); the builders below stay here.

const (
	rateLimitStatusOK          = "ok"
	rateLimitStatusUnavailable = "unavailable"
	// rateLimitStatusNoToken is no longer produced by these handlers since #104 M5:
	// "the user has no token" is now an EMPTY tokens array, not a status on a single
	// reading, because there is no per-token reading to attach a status to when there
	// is no token. The constant is retained because the CLI and the web client still
	// render this string, and the frozen union (apitypes.RateLimitDTO) still admits it.
	rateLimitStatusNoToken = "no_token"
)

var _ = rateLimitStatusNoToken // retained for the wire contract; see the const comment

// SelfRateLimits returns the caller's Claude rate-limit meters, ONE PER TOKEN since
// PRD #104 M5 (D4): a `{"tokens": [...]}` array replacing PRD #53's single reading —
// a breaking response-shape change the web client, the CLI, and the e2e assertions
// all move for in the same MR. Session-authed, scoped to the caller.
//
// Each element carries its credential's label + default flag (a name, never the
// value) and the same status union as before: a token with a reading is `ok`, a
// token with no reading yet (fresh save, probe disabled, refused credential) is
// `unavailable`. A user with no tokens gets an empty array, which the client
// renders as the old no_token state — there is no per-token status to report when
// there is no token.
func (h *Handler) SelfRateLimits(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListRateLimitsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("rate limits: list for user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	tokens := make([]apitypes.TokenRateLimitDTO, 0, len(rows))
	for _, row := range rows {
		var limits apitypes.RateLimitDTO
		if !row.SyncedAt.Valid {
			// LEFT JOIN miss: the token exists but has no reading yet.
			limits = apitypes.RateLimitDTO{Status: rateLimitStatusUnavailable}
		} else {
			limits = h.okRateLimitDTO(
				row.FiveHourPct, row.FiveHourResetsAt,
				row.SevenDayPct, row.SevenDayResetsAt,
				row.Source, row.SyncedAt,
			)
		}
		tokens = append(tokens, apitypes.TokenRateLimitDTO{
			SecretID:     row.UserSecretID.String(),
			Label:        row.Label,
			IsDefault:    row.IsDefault,
			AutoEligible: row.AutoEligible,
			AutoStatus:   string(h.autoStatus(autoselectrow.FromRateLimitRow(row))),
			Limits:       limits,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// autoStatus classifies ONE candidate's live auto-selection eligibility (PRD #111 M2).
//
// It exists so the ANSWER is computed server-side and shipped as a string (D21). The
// alternative — sending the raw pcts and letting the web decide — is what guarantees
// drift: the ranker's gate would be in Go and the UI's in TypeScript, they would
// diverge on some edge, and the divergence would surface as a settings page promising
// a token is eligible while the selector quietly skips it, with no test failing.
func (h *Handler) autoStatus(c autoselect.Candidate) autoselect.Status {
	return autoselect.Classify(c, h.cfg.AutoselectPolicy(), time.Now()).Status
}

// AdminRateLimits returns every user's meters + staleness (PRD #53), grouped BY USER
// THEN BY TOKEN since #104 M5 (D4). Admin-only — the route is under the RequireAdmin
// group (mirroring AdminUsage), so a non-admin never reaches here (403). Each user
// carries their live vault lock state and one meter per token; a token-less user is
// present with an empty tokens array.
//
// The query returns one row per (user, token) — and one row per token-less user with
// a NULL user_secret_id — ordered by email, so consecutive rows for one user
// collapse into a single AdminRateLimitRowDTO without a map (the ORDER BY is what
// makes the fold correct rather than incidental).
func (h *Handler) AdminRateLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListRateLimits(r.Context())
	if err != nil {
		slog.Error("rate limits: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	users := make([]apitypes.AdminRateLimitRowDTO, 0)
	for _, u := range rows {
		// A new user starts a new group. vaultUnlocked is looked up once per user, on
		// the first row, not once per token.
		if len(users) == 0 || users[len(users)-1].ID != u.UserID.String() {
			users = append(users, apitypes.AdminRateLimitRowDTO{
				ID:          u.UserID.String(),
				Email:       u.Email,
				Name:        u.DisplayName.String, // "" when the user has no display name
				VaultLocked: !h.vaultUnlocked(u.UserID),
				Tokens:      []apitypes.TokenRateLimitDTO{},
			})
		}
		// A token-less user is a single row with a NULL secret id: no token to append,
		// the empty Tokens array is the no_token signal.
		if !u.UserSecretID.Valid {
			continue
		}
		var limits apitypes.RateLimitDTO
		if !u.SyncedAt.Valid {
			limits = apitypes.RateLimitDTO{Status: rateLimitStatusUnavailable}
		} else {
			limits = h.okRateLimitDTO(
				u.FiveHourPct, u.FiveHourResetsAt,
				u.SevenDayPct, u.SevenDayResetsAt,
				u.Source, u.SyncedAt,
			)
		}
		grp := &users[len(users)-1]
		// The pool flag + live status ride the admin view too (PRD #111 M2). NOTHING
		// RENDERS THEM TODAY — the admin page reads neither field — so this is an API
		// contract choice, not a rendering requirement, and an earlier version of this
		// comment overstated it. The reason it is still right: this is the SAME DTO the
		// self view uses, so leaving them zero would put `auto_eligible: false` and an
		// empty status on the wire for every token in the factory. A field that is
		// absent is honest; a field that is present and uniformly wrong is what a
		// future admin surface would build on. Widening that surface is PRD #104 M5's
		// job, not this PRD's.
		//
		// AutoEligible is pgtype.Bool here because the admin query LEFT JOINs
		// user_secrets; the token-less row is skipped above, so .Bool is the real value
		// by the time it is read.
		grp.Tokens = append(grp.Tokens, apitypes.TokenRateLimitDTO{
			SecretID:     uuid.UUID(u.UserSecretID.Bytes).String(),
			Label:        u.Label.String,
			IsDefault:    u.IsDefault.Bool,
			AutoEligible: u.AutoEligible.Bool,
			AutoStatus:   string(h.autoStatus(autoselectrow.FromAdminRateLimitRow(u))),
			Limits:       limits,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
}

// okRateLimitDTO builds the "ok" union branch from the stored gauge columns. stale
// is computed server-side (D3); resets are rendered as epoch seconds (D7).
func (h *Handler) okRateLimitDTO(fivePct pgtype.Int2, fiveReset pgtype.Timestamptz, sevenPct pgtype.Int2, sevenReset pgtype.Timestamptz, source pgtype.Text, syncedAt pgtype.Timestamptz) apitypes.RateLimitDTO {
	stale := h.rateLimitStale(syncedAt)
	return apitypes.RateLimitDTO{
		Status:   rateLimitStatusOK,
		FiveHour: &apitypes.RateLimitWindow{Pct: int(fivePct.Int16), ResetsAt: epochPtr(fiveReset)},
		SevenDay: &apitypes.RateLimitWindow{Pct: int(sevenPct.Int16), ResetsAt: epochPtr(sevenReset)},
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
