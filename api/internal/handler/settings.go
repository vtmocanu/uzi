package handler

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// updateSettingsRequest is a partial map of setting key → value. Any subset of
// the known keys may be sent; unknown keys are rejected. Secret keys (the Slack
// tokens) carry the plaintext token to store; non-secret keys carry the value
// verbatim. The web Settings page sends the label keys together and the Slack
// card separately, but the API tolerates any single-key update.
type updateSettingsRequest struct {
	Settings map[string]string `json:"settings"`
}

// settingsResponse is the admin GET/PUT body (PRD #25). `settings` carries only
// the non-secret effective values; `secrets` reports whether each secret key is
// configured (never its value); `sources` reports, per key, whether the effective
// value comes from an env var, the DB, or the compiled default. The split of
// values from secrets is what makes a token leak structurally impossible — the
// value map cannot hold a secret's bytes.
type settingsResponse struct {
	Settings map[string]string `json:"settings"`
	Secrets  map[string]bool   `json:"secrets"`
	Sources  map[string]string `json:"sources"`
	// SlackStatus is the live Slack socket connection state (PRD #25 M2):
	// "disabled" | "connecting" | "connected" | "error:<class>". The webui chip
	// renders it.
	SlackStatus string `json:"slack_status"`
	// OIDCStatus is the OIDC SSO health for the admin status line (PRD #45, Nit6):
	// "disabled" (unconfigured), "ok" (discovery succeeded), or "degraded"
	// (configured but discovery has not yet succeeded — the IdP was down at boot; a
	// login retry clears it). OIDCProviderName is the operator-set button label.
	OIDCStatus       string `json:"oidc_status"`
	OIDCProviderName string `json:"oidc_provider_name"`
}

func newSettingsResponse(v settings.AdminView, slackStatus string) settingsResponse {
	return settingsResponse{Settings: v.Values, Secrets: v.Secrets, Sources: v.Sources, SlackStatus: slackStatus}
}

// GetSettings returns every known setting (admin only): non-secret effective
// values, per-secret `configured` flags (never the token itself), and per-key
// source. The shape is stable — one entry per known key — so a missing row reads
// as its compiled-in default rather than an absent field.
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	view, err := h.settings.AdminView(r.Context())
	if err != nil {
		slog.Error("get settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := newSettingsResponse(view, h.slackState())
	resp.OIDCStatus = h.oidcStatus()
	resp.OIDCProviderName = h.cfg.OIDCProviderName
	httpx.JSON(w, http.StatusOK, resp)
}

// oidcStatus reports OIDC SSO health for the admin settings line (PRD #45, Nit6),
// non-blocking: "disabled" when unconfigured, "ok" once discovery has succeeded,
// "degraded" when configured but discovery has not yet succeeded (IdP down at boot;
// a login retry clears it). Never networks on the admin page load.
func (h *Handler) oidcStatus() string {
	switch {
	case h.oidc == nil:
		return "disabled"
	case h.oidc.Discovered():
		return "ok"
	default:
		return "degraded"
	}
}

// VaultMigration reports how many stored user secrets still use the legacy
// master-key sealing (PRD #32): an admin-visible migration-progress signal so the
// operator can see who has not unlocked since the vault rolled out. New saves are
// born 'dek' and lazy rewrap flips old rows on unlock, so a healthy instance
// trends to zero. Admin-only (mounted under the admin group). It never exposes any
// secret value or per-user identity — only the count.
func (h *Handler) VaultMigration(w http.ResponseWriter, r *http.Request) {
	n, err := h.q.CountMasterSealedSecrets(r.Context())
	if err != nil {
		slog.Error("count master-sealed secrets", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"master_sealed": n})
}

// UpdateSettings writes one or more settings (admin only). It validates per key
// (label rules, the theme registry, the strict bool, the URL rule, or the token
// format), rejects unknown keys, rejects a write to an env-sourced key with 409
// (the value is fixed by the environment), live-validates any submitted Slack
// token against Slack before storing it, seals secret keys with secretbox+base64,
// and enforces the cross-key label rule against the effective post-update state.
// The writes commit in a single transaction so a two-key swap can never leave the
// two labels transiently equal.
func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req updateSettingsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Settings) == 0 {
		httpx.Error(w, http.StatusBadRequest, "no settings provided")
		return
	}

	// Per-key gate: known key, not env-fixed, and per-value valid. The env check
	// runs before validation so an env-sourced key is refused as policy (409)
	// rather than incidentally passing/failing a value rule.
	for key, value := range req.Settings {
		if !settings.Known(key) {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("unknown setting: %s", key))
			return
		}
		if h.settings.IsEnvSourced(key) {
			httpx.Error(w, http.StatusConflict, fmt.Sprintf("%s is set from the environment and cannot be changed here", key))
			return
		}
		if err := settings.Validate(key, value); err != nil {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("%s: %s", key, err))
			return
		}
	}

	ctx := r.Context()

	// Cheap pre-transaction cross-key check against the cache: rejects the obvious
	// equal-label case without opening a transaction, and BEFORE the live Slack
	// call below — so a label-collision PUT never wastes a network round-trip. Only
	// non-secret keys enter the merged map (a secret token's plaintext never
	// participates in a label check). Best-effort only (the cache can be stale);
	// the authoritative check runs below inside the write tx against the FOR UPDATE
	// rows.
	current, err := h.settings.All(ctx)
	if err != nil {
		slog.Error("update settings: read current", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	precheck := make(map[string]string, len(current))
	for k, v := range current {
		precheck[k] = v
	}
	for k, v := range req.Settings {
		if settings.IsSecret(k) {
			continue
		}
		precheck[k] = v
	}
	if err := settings.ValidateMerged(precheck); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	// PRD #602 M2: the agent-source repo URL is constrained by the SEPARATE SSRF
	// allowlist (AGENT_SOURCE_ALLOWED_BASE_URLS). It is enforced HERE, in the generic
	// PUT handler that holds h.cfg — NOT in settings.Validate, which is a pure
	// (key,value) function with no Config (importing config there is an import cycle).
	// Because UpdateSettings IS the generic PUT, enforcing here is not bypassable by a
	// separate write route (memory: a gate in a separate dedicated handler is
	// bypassable). An empty value (clearing/disabling the source) is exempt; a
	// non-empty value must be allowlisted. The clone seam re-checks in M3 (TOCTOU).
	if srcURL, ok := req.Settings[settings.KeyAgentSourceRepoURL]; ok && strings.TrimSpace(srcURL) != "" {
		if !h.cfg.AgentSourceBaseURLAllowed(srcURL) {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("%s: URL is not in the agent-source allowlist (AGENT_SOURCE_ALLOWED_BASE_URLS)", settings.KeyAgentSourceRepoURL))
			return
		}
	}

	// Live-validate submitted Slack tokens against Slack after the cheap label
	// precheck and BEFORE opening a transaction (a network call must not run inside
	// the write tx). The error is scrubbed of any token bytes and never echoes the
	// submitted value.
	if token, ok := req.Settings[settings.KeySlackBotToken]; ok {
		if err := h.slackVal().ValidateBotToken(ctx, token); err != nil {
			slog.Warn("settings: slack bot token validation failed", "error", slacksvc.ScrubTokens(err.Error()))
			httpx.Error(w, http.StatusBadRequest, "slack_bot_token: Slack rejected the token ("+slacksvc.ScrubTokens(err.Error())+")")
			return
		}
	}
	if token, ok := req.Settings[settings.KeySlackAppToken]; ok {
		if err := h.slackVal().ValidateAppToken(ctx, token); err != nil {
			slog.Warn("settings: slack app token validation failed", "error", slacksvc.ScrubTokens(err.Error()))
			httpx.Error(w, http.StatusBadRequest, "slack_app_token: Slack rejected the token ("+slacksvc.ScrubTokens(err.Error())+")")
			return
		}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("update settings: begin tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	// Serialize all settings writes globally (issue #831). This MUST be the first
	// statement of the transaction: it is a mutex that holds regardless of which
	// rows exist, which the ListAppSettingsForUpdate FOR UPDATE below is not — a
	// label key at its compiled-in default has no row for FOR UPDATE to lock, so two
	// concurrent PUTs could each insert a NEW row invisible to the other's READ
	// COMMITTED snapshot and both pass the cross-key check, committing equal labels.
	// See store.SettingsMutationLockKey for the full reasoning.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.SettingsMutationLockKey); err != nil {
		slog.Error("update settings: acquire lock", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Authoritative cross-key check (Decision 8) against committed state: read the
	// settings rows FOR UPDATE. With the advisory lock held this no longer races (a
	// concurrent PUT is blocked at the lock above), and reading the committed rows
	// here still avoids a possibly-stale cache snapshot. Only non-secret updates
	// merge into the label map (Effective already excludes secrets).
	locked, err := qtx.ListAppSettingsForUpdate(ctx)
	if err != nil {
		slog.Error("update settings: lock rows", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	committed := settings.Effective(locked)
	merged := make(map[string]string, len(committed))
	for k, v := range committed {
		merged[k] = v
	}
	nonSecretUpdates := make(map[string]string, len(req.Settings))
	for k, v := range req.Settings {
		if settings.IsSecret(k) {
			continue
		}
		merged[k] = v
		nonSecretUpdates[k] = v
	}
	// changed decides whether to force a full repo resync after commit. Only a
	// LABEL change re-filters boards; every other key (theme, slack, judge) is a
	// no-op here. Computed against the committed (FOR UPDATE-locked) rows.
	changed := settings.LabelChanged(committed, nonSecretUpdates)
	if err := settings.ValidateMerged(merged); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	for key, value := range req.Settings {
		// Secret keys are sealed here (never stored in the clear); non-secret keys
		// pass through verbatim. Single write-side seam — see settings.ValueForStorage.
		toStore, sealErr := settings.ValueForStorage(h.box, key, value)
		if sealErr != nil {
			slog.Error("update settings: seal secret failed", "key", key, "error", sealErr)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, err := qtx.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
			Key:       key,
			Value:     toStore,
			UpdatedBy: pgconv.UUID(actor.ID),
		}); err != nil {
			// Deliberately omit the key/value from the log: a secret value must never
			// be logged, and nothing in app_settings should be assumed loggable.
			slog.Error("update settings: upsert failed", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("update settings: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Drop the read cache so the next read (poller or this handler's response)
	// reflects the write immediately rather than lagging by the TTL.
	h.settings.Invalidate()

	// A changed label re-filters every board, so ask the poller to full-sync all
	// repos next cycle. Non-blocking: the resync needs each connection's decrypted
	// PAT and belongs in the poller; the PUT returns after signalling.
	if changed && h.reconciler != nil {
		h.reconciler.ForceReconcile()
	}

	view, err := h.settings.AdminView(ctx)
	if err != nil {
		slog.Error("update settings: read back", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, newSettingsResponse(view, h.slackState()))
}
