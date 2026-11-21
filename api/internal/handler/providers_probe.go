package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// providerHaikuModel is the mapped haiku model of a stored model map ("" when absent).
func providerHaikuModel(raw []byte) string {
	var m providers.ModelMap
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m[providers.HaikuAlias].Model
}

// reasonProviderProbeRateLimited is the 429 code of the provider Test route (PRD #4320 section 8):
// one probe in flight per owner, four instance-wide, 30 per owner per hour.
const reasonProviderProbeRateLimited = "provider_probe_rate_limited"

// providerProbeResponseGrace is added to the two probe steps when extending the response write
// deadline: the listener's own WriteTimeout (15 s from the request start) is shorter than a probe.
const providerProbeResponseGrace = 10 * time.Second

// providerProbe returns the probe client and the admission limiter, built on first use from the
// config. The allowlist is read live from h.cfg on every probe.
func (h *Handler) providerProbe() (*providers.Prober, *providers.ProbeLimiter) {
	h.providerProbeOnce.Do(func() {
		if h.providerProber == nil {
			pool, err := providers.RootCAs(h.cfg.ProviderExtraCAPEM)
			if err != nil {
				// Boot already refused an unparseable bundle; fall back to system roots rather than panic.
				slog.Error("provider probe: extra CA bundle unusable; using system roots", "error", err)
				pool = nil
			}
			h.providerProber = providers.NewProber(providers.ProbeConfig{
				Allowed:       func(u string) (string, bool) { return h.cfg.ProviderBaseURLAllowed(u) },
				AllowLoopback: h.cfg.ProviderAllowLoopback,
				RootCAs:       pool,
			})
		}
		if h.providerProbeLimiter == nil {
			h.providerProbeLimiter = providers.NewProbeLimiter()
		}
	})
	return h.providerProber, h.providerProbeLimiter
}

// TestMyProvider probes one of the caller's providers (POST /api/me/providers/{id}/test): a bounded
// request in the profile's protocol and, if it succeeds, a forced single-tool request at its base URL
// with its key. The key is opened for the call and dropped after it; the response carries the two
// outcomes and nothing from the gateway's bodies. The result is stored for the current revision.
func (h *Handler) TestMyProvider(w http.ResponseWriter, r *http.Request) {
	user, ok := userForProviderRoute(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	row, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		internalProviderError(w, "get model provider for test", err)
		return
	}
	if row.RetiredAt.Valid {
		writeProviderError(w, errProviderRetired)
		return
	}
	if !row.KeySecretID.Valid || row.KeyDisabledAt.Valid {
		writeProviderError(w, errProviderNoKey)
		return
	}
	// The feature being off, or the stored URL having left the allowlist, is the same typed refusal
	// as everywhere else; the probe re-checks it again before it dials.
	entry, allowed := h.cfg.ProviderBaseURLAllowed(row.BaseUrl)
	if !allowed {
		writeBaseURLRefusal(w)
		return
	}
	var models providers.ModelMap
	if json.Unmarshal(row.ModelMap, &models) != nil || providers.ValidateProfile(row.Engine, row.Protocol, row.AuthStyle, models) != nil {
		httpx.Error(w, http.StatusConflict, "the provider profile is invalid")
		return
	}
	model := providerHaikuModel(row.ModelMap)
	expectedKind := store.KindAnthropicCompatibleKey
	if row.Engine == providers.EngineCodex {
		model = models["default"].Model
		expectedKind = store.KindOpenAICompatibleKey
	}
	if model == "" {
		httpx.Error(w, http.StatusConflict, "the provider has no probe model")
		return
	}

	prober, limiter := h.providerProbe()
	release, _, retryAfter, admitted := limiter.Acquire(user.ID.String())
	if !admitted {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		httpx.ErrorReason(w, http.StatusTooManyRequests, "too many provider tests; try again later", reasonProviderProbeRateLimited)
		return
	}
	defer release()

	key, err := secretopen.OpenByIDOfKind(r.Context(), h.q, h.vault, h.box, user.ID, row.KeySecretID.Bytes, expectedKind)
	if err != nil {
		switch {
		case errors.Is(err, secretopen.ErrVaultLocked) || errors.Is(err, vault.ErrLocked):
			h.writeVaultLocked(w, vault.ErrLocked)
		case errors.Is(err, secretopen.ErrNoSecret):
			writeProviderError(w, errProviderNoKey)
		default:
			internalProviderError(w, "open provider key for test", err)
		}
		return
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	// The ID alone does not bind the opened material: rotation keeps the secret
	// ID and bumps enablement_rev without changing the provider revision. Re-read
	// both revisions in one snapshot after opening and before any network request,
	// so a new destination's key can never be sent to the old destination. Edits
	// after this check can only leave us sending the old key to its old endpoint.
	current, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (current.CurrentRevision != row.CurrentRevision ||
		current.KeySecretID != row.KeySecretID || !row.KeyEnablementRev.Valid ||
		current.KeyEnablementRev != row.KeyEnablementRev || current.KeyDisabledAt.Valid || current.RetiredAt.Valid) {
		httpx.Error(w, http.StatusConflict, "the provider was edited or removed while it was being tested; test it again")
		return
	}
	if err != nil {
		internalProviderError(w, "recheck provider key for test", err)
		return
	}

	// The listener's 15 s WriteTimeout would cut a probe (two steps of up to 20 s) short.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2*providers.DefaultProbeStepTimeout + providerProbeResponseGrace))

	res, err := prober.Probe(r.Context(), providers.ProbeTarget{Protocol: row.Protocol, BaseURL: entry, AuthStyle: row.AuthStyle, Model: model, Key: key})
	if errors.Is(err, providers.ErrBaseURLNotAllowed) {
		writeBaseURLRefusal(w)
		return
	}
	if err != nil {
		internalProviderError(w, "provider probe", err)
		return
	}

	n, err := h.q.UpsertModelProviderProbeResult(r.Context(), store.UpsertModelProviderProbeResultParams{
		AuthResult:  res.Auth,
		ToolResult:  pgconv.Text(res.Tool),
		ProviderID:  id,
		OwnerUserID: user.ID,
		Revision:    row.CurrentRevision,
	})
	if err != nil {
		internalProviderError(w, "store provider probe result", err)
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusConflict, "the provider was edited or removed while it was being tested; test it again")
		return
	}
	fresh, err := h.q.GetModelProviderForOwner(r.Context(), store.GetModelProviderForOwnerParams{ID: id, OwnerUserID: user.ID})
	if err != nil || !fresh.ProbeAuthResult.Valid {
		internalProviderError(w, "read provider probe result", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"probe": providerDTO(providerRow(fresh)).Probe})
}
