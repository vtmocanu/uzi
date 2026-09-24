package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// The closed set of per-account Codex rate-limit statuses (PRD #1209 M3), mirroring the
// apitypes.CodexAccountRateLimitDTO doc-comment union. Derived server-side per account by
// codexRateLimitStatus; a client never re-derives them (the same D21 reasoning as
// autoStatus on the Anthropic side).
//
// no_subscription is DELIBERATELY absent here: it is not a per-account status but the shape
// a user with no linked subscription account produces — an EMPTY accounts array, which both
// store reads already yield (their EXISTS(linked) filter drops unlinked accounts). So there
// is no per-account value to represent it, and a lone unused const would trip the unused
// linter; the empty array carries the meaning.
const (
	codexRateLimitStatusPending                  = "pending"
	codexRateLimitStatusNoReading                = "no_reading"
	codexRateLimitStatusFresh                    = "fresh"
	codexRateLimitStatusStale                    = "stale"
	codexRateLimitStatusVaultLocked              = "vault_locked"
	codexRateLimitStatusCredentialActionRequired = "credential_action_required" //nolint:gosec // G101: a rate-limit status discriminator string, not a credential
	codexRateLimitStatusPollingDisabled          = "polling_disabled"
)

// codexRateLimitReasonProviderRejected is the only CodexAccountRateLimitDTO.Reason value
// (issue #1594): the account's reauth_reason as stored by the provider-rejection refresh
// transaction. Any other stored reason maps to "" (no reason shown).
const codexRateLimitReasonProviderRejected = "provider_rejected"

// SelfCodexRateLimits returns the caller's Codex account rate-limit meters, ONE PER
// LINKED SUBSCRIPTION ACCOUNT (PRD #1209 M3) — the Codex sibling of SelfRateLimits.
// RequireUser (session OR a user-scoped CLI token), scoped to the caller by the query's
// user_id filter. A user with no linked codex account gets an empty accounts array (the
// no_subscription shape), never null.
//
// Each element names WHICH account it describes by its linked-alias labels (a name, never
// a token/login/raw-provider-id), carries the account's opaque uzi id as the client key,
// its default flag, the derived status, the last-success time and the stored buckets. No
// synchronous provider call and no credential refresh happen on read: the stored snapshot
// is served as-is, so a meter is never re-derived or locally cleared at read time.
func (h *Handler) SelfCodexRateLimits(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.GetCodexAccountRateLimitsForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("codex rate limits: get for user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The caller's live vault-lock state, looked up ONCE (not per account): a locked vault
	// means the poller cannot read the login, so every one of this user's accounts is
	// vault_locked.
	vaultLocked := !h.vaultUnlocked(user.ID)
	accounts := make([]apitypes.CodexAccountRateLimitDTO, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, h.codexAccountDTO(
			row.ProviderAccountID, row.Aliases, row.IsDefault, row.ReauthRequired, row.ReauthReason,
			row.Buckets, row.LastSuccessAt, row.LastAttemptAt, vaultLocked,
		))
	}
	// A meter reading is per-user private state; never let a shared cache serve one
	// caller's readings to another (mirrors the sensitivity of the labels beside them).
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// AdminCodexRateLimits returns every user's Codex account meters + live vault-lock state
// (PRD #1209 M3), grouped BY USER then BY ACCOUNT — the Codex sibling of AdminRateLimits.
// Admin-only (mounted under RequireAdminRO), so a non-admin never reaches here.
//
// The query returns one row per (user, account), ordered by email then account id and
// driven from codex_provider_account with an EXISTS(linked) filter, so a user with no
// linked account simply does not appear (there is no token-less row to skip, unlike the
// Anthropic admin fold) and consecutive rows for one user collapse into a single
// CodexAdminRateLimitRowDTO without a map — the ORDER BY is what makes the fold correct.
// vault_locked is folded in-memory per user (looked up once, on the user's first row),
// exactly as AdminRateLimits does.
func (h *Handler) AdminCodexRateLimits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListCodexAccountRateLimits(r.Context())
	if err != nil {
		slog.Error("codex rate limits: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	users := make([]apitypes.CodexAdminRateLimitRowDTO, 0)
	for _, row := range rows {
		// A new user starts a new group; vault-lock is resolved once per user, on the
		// first row, not once per account.
		if len(users) == 0 || users[len(users)-1].ID != row.UserID.String() {
			users = append(users, apitypes.CodexAdminRateLimitRowDTO{
				ID:          row.UserID.String(),
				Email:       row.Email,
				Name:        row.DisplayName.String, // "" when the user has no display name
				VaultLocked: !h.vaultUnlocked(row.UserID),
				Accounts:    []apitypes.CodexAccountRateLimitDTO{},
			})
		}
		grp := &users[len(users)-1]
		grp.Accounts = append(grp.Accounts, h.codexAccountDTO(
			row.ProviderAccountID, row.Aliases, row.IsDefault, row.ReauthRequired, row.ReauthReason,
			row.Buckets, row.LastSuccessAt, row.LastAttemptAt, grp.VaultLocked,
		))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
}

// codexAccountDTO maps one store row's shared fields onto a CodexAccountRateLimitDTO,
// deriving the status and normalizing the two collection fields to non-nil slices (the
// contract keeps aliases and buckets PRESENT, never null). Both store reads carry these
// same fields, so the two handlers share this one builder. reauthReason becomes the DTO's
// reason only under credential_action_required and only for the closed-set value.
func (h *Handler) codexAccountDTO(accountID uuid.UUID, aliases []string, isDefault, reauthRequired bool, reauthReason pgtype.Text, buckets []byte, lastSuccessAt, lastAttemptAt pgtype.Timestamptz, vaultLocked bool) apitypes.CodexAccountRateLimitDTO {
	status := codexRateLimitStatus(h.cfg.CodexUsagePollInterval, vaultLocked, reauthRequired, lastSuccessAt, lastAttemptAt, time.Now())

	// Aliases and buckets are contract fields with no omitempty, so a nil slice would
	// serialize as null; force an empty (non-nil) slice instead.
	if aliases == nil {
		aliases = []string{}
	}
	bucketsDTO := []apitypes.CodexRateLimitBucketDTO{}
	if len(buckets) > 0 {
		if err := json.Unmarshal(buckets, &bucketsDTO); err != nil {
			// A stored snapshot our own poller wrote; a decode failure is effectively
			// unreachable. Serve an empty bucket set rather than failing the whole read.
			slog.Error("codex rate limits: unmarshal buckets", "account", accountID.String(), "error", err)
			bucketsDTO = []apitypes.CodexRateLimitBucketDTO{}
		}
	}
	// A JSONB `null` literal (json.Marshal of a nil bucket slice) unmarshals back to a nil
	// slice; re-force empty so the DTO field is never null.
	if bucketsDTO == nil {
		bucketsDTO = []apitypes.CodexRateLimitBucketDTO{}
	}

	dto := apitypes.CodexAccountRateLimitDTO{
		AccountID: accountID.String(),
		Aliases:   aliases,
		IsDefault: isDefault,
		Status:    status,
		Buckets:   bucketsDTO,
	}
	if status == codexRateLimitStatusCredentialActionRequired && reauthReason.Valid && reauthReason.String == codexRateLimitReasonProviderRejected {
		dto.Reason = codexRateLimitReasonProviderRejected
	}
	if lastSuccessAt.Valid {
		dto.LastSuccessAt = lastSuccessAt.Time.UTC().Format(time.RFC3339)
	}
	// stale is set (true) only for the two statuses that mean "what you see may be behind":
	// an aged reading, and a disabled poller that will never refresh the stored snapshot.
	// Every other status leaves it nil (omitted).
	if status == codexRateLimitStatusStale || status == codexRateLimitStatusPollingDisabled {
		stale := true
		dto.Stale = &stale
	}
	return dto
}

// codexRateLimitStatus derives one account's closed-set status (PRD #1209 M3) from the
// poll interval, the user's live vault-lock state, the account's reauth flag, and the two
// snapshot timestamps. Precedence, highest first:
//
//  1. polling_disabled — interval <= 0: the poller is off, so nothing will ever refresh
//     the stored snapshot (served as-is, but flagged stale).
//  2. vault_locked — the user's vault DEK is not cached, so the poller cannot read the
//     login to poll at all.
//  3. credential_action_required — the account is flagged reauth_required: its login needs
//     re-authentication before any further reading is possible.
//  4. a reading exists (last_success_at valid): fresh when its age <= 3× the interval
//     (the same staleness window as the Anthropic side), else stale.
//  5. no reading: no_reading when a poll was attempted (last_attempt_at valid, i.e. tried
//     and failed), else pending (never attempted yet).
//
// A per-account status is NEVER no_subscription — that is the empty-accounts-array shape
// both store reads already produce for a user with no linked account.
func codexRateLimitStatus(interval time.Duration, vaultLocked, reauthRequired bool, lastSuccessAt, lastAttemptAt pgtype.Timestamptz, now time.Time) string {
	switch {
	case interval <= 0:
		return codexRateLimitStatusPollingDisabled
	case vaultLocked:
		return codexRateLimitStatusVaultLocked
	case reauthRequired:
		return codexRateLimitStatusCredentialActionRequired
	case lastSuccessAt.Valid:
		if now.Sub(lastSuccessAt.Time) <= 3*interval {
			return codexRateLimitStatusFresh
		}
		return codexRateLimitStatusStale
	case lastAttemptAt.Valid:
		return codexRateLimitStatusNoReading
	default:
		return codexRateLimitStatusPending
	}
}
