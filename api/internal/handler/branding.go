package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Instance branding surface (PRD #685 M1). Four endpoints:
//
//   - GET /api/branding                 — public, allowlisted config + presence flags.
//   - GET /api/branding/logo/{slot}     — public, the cacheable logo bytes.
//   - PUT /api/admin/branding/logo/{slot}    — admin upload (type + size validated).
//   - DELETE /api/admin/branding/logo/{slot} — admin clear.
//
// Config (modes/flags/company text) lives in app_settings; logo BYTES live in the
// branding_assets table (Decision D7). The two are served through different routes so
// the JSON stays small and cacheable and the settings cache never loads a blob.

// maxBrandingLogoBytes caps an uploaded logo at 256 KiB (PRD #685 M1). Small enough
// that an unauthenticated GET of the bytes is not a DoS-amplification lever (Risk R2)
// and that the write stays well under httpx.maxBodyBytes (1 MiB) — which is why the
// logo goes through this dedicated raw-body route rather than PUT /admin/settings
// (Risk R4). At-cap is accepted; cap+1 is rejected.
const maxBrandingLogoBytes = 262144

// brandingContentTypes is the upload allowlist (Decision D5): PNG, WebP, SVG. An
// uploaded SVG is only ever rendered via <img> (passive; scripts do not run in that
// context) with CSP object-src 'none', so the stored-XSS surface is closed without a
// content scan here.
var brandingContentTypes = map[string]struct{}{
	"image/png":     {},
	"image/webp":    {},
	"image/svg+xml": {},
}

// brandingLogoMaxAge is the Cache-Control window for a served logo. Short enough that
// a re-upload propagates within minutes, while the strong ETag makes the common repeat
// load a cheap 304 regardless.
const brandingLogoMaxAge = 300

// brandingResponse is the EXPLICIT, allowlisted shape of GET /api/branding: exactly the
// six branding config keys (typed) plus the two derived presence flags. It is built
// field-by-field from settings.Cache.Branding, NEVER from Cache.All / AdminView, so an
// anonymous caller can read only these fields and never the rest of the non-secret
// settings surface (Risk R1). TestBrandingPublicReadIsAllowlisted pins that closed.
type brandingResponse struct {
	AppLogoMode      string `json:"app_logo_mode"`
	AppLogoPresent   bool   `json:"app_logo_present"`
	AppLogoKeepName  bool   `json:"app_logo_keep_name"`
	BrandMode        string `json:"brand_mode"`
	BrandCompany     string `json:"brand_company"`
	BrandPlacement   string `json:"brand_placement"`
	BrandPlaque      bool   `json:"brand_plaque"`
	BrandLogoPresent bool   `json:"brand_logo_present"`
}

// validBrandingSlot reports whether slot is one of the two logo slots.
func validBrandingSlot(slot string) bool {
	return slot == "app" || slot == "brand"
}

// GetBranding serves the public, allowlisted branding config (PRD #685 M1). Modeled on
// Version: mounted OUTSIDE every RequireAuth group, unauthenticated by design so the
// signed-out shell can brand itself. It returns only the branding fields — see
// brandingResponse and Risk R1.
func (h *Handler) GetBranding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, _ := h.settings.Branding(ctx)

	resp := brandingResponse{
		AppLogoMode:     cfg.AppLogoMode,
		AppLogoKeepName: cfg.AppLogoKeepName,
		BrandMode:       cfg.BrandMode,
		BrandCompany:    cfg.BrandCompany,
		BrandPlacement:  cfg.BrandPlacement,
		BrandPlaque:     cfg.BrandPlaque,
	}
	// Presence is derived from row existence via a slot-only query so this
	// AppShell-polled public endpoint never loads the logo bytes.
	if slots, err := h.q.ListBrandingSlots(ctx); err == nil {
		for _, s := range slots {
			switch s {
			case "app":
				resp.AppLogoPresent = true
			case "brand":
				resp.BrandLogoPresent = true
			}
		}
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// GetBrandingLogo serves the raw logo bytes for a slot (PRD #685 M1), unauthenticated.
// It returns the stored Content-Type, a strong ETag (sha256 of the bytes),
// Cache-Control, and honors If-None-Match with a 304 so repeat loads are cheap. A slot
// with no uploaded asset is a 404 (the chrome then falls back to the preset/none per
// mode).
func (h *Handler) GetBrandingLogo(w http.ResponseWriter, r *http.Request) {
	slot := chi.URLParam(r, "slot")
	if !validBrandingSlot(slot) {
		httpx.Error(w, http.StatusNotFound, "unknown logo slot")
		return
	}
	asset, err := h.q.GetBrandingAsset(r.Context(), slot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no logo uploaded for this slot")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "failed to read logo")
		return
	}

	sum := sha256.Sum256(asset.Bytes)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", brandingLogoMaxAge))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", asset.ContentType)
	// Defense-in-depth for admin-uploaded bytes (svg+xml is an allowed type): stop
	// content-type sniffing and force a passive rendering context, so the route
	// self-defends even if the nginx-inherited CSP/nosniff ever stops covering /api/.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.Bytes)
}

// PutBrandingLogo is the admin logo upload (PRD #685 M1): admin-gated, raw image body
// with the Content-Type header naming the type. It validates the type against the
// allowlist and caps the size at maxBrandingLogoBytes (256 KiB) via MaxBytesReader —
// at-cap passes, cap+1 is rejected 413 — then upserts branding_assets, recording the
// admin as updated_by.
func (h *Handler) PutBrandingLogo(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slot := chi.URLParam(r, "slot")
	if !validBrandingSlot(slot) {
		httpx.Error(w, http.StatusNotFound, "unknown logo slot")
		return
	}

	ct := r.Header.Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mediaType
	}
	if _, allowed := brandingContentTypes[ct]; !allowed {
		httpx.Error(w, http.StatusUnsupportedMediaType,
			"logo must be image/png, image/webp, or image/svg+xml")
		return
	}

	// MaxBytesReader lets the body read up to and including the cap; the (cap+1)th byte
	// makes Read return *http.MaxBytesError, which we answer with a clear 413.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBrandingLogoBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("logo must be at most %d bytes", maxBrandingLogoBytes))
			return
		}
		httpx.Error(w, http.StatusBadRequest, "failed to read logo body")
		return
	}
	if len(body) == 0 {
		httpx.Error(w, http.StatusBadRequest, "logo body must not be empty")
		return
	}

	if _, err := h.q.UpsertBrandingAsset(r.Context(), store.UpsertBrandingAssetParams{
		Slot:        slot,
		ContentType: ct,
		Bytes:       body,
		UpdatedBy:   pgUUID(actor.ID),
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to store logo")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"slot": slot, "present": true})
}

// DeleteBrandingLogo clears a slot's uploaded logo (PRD #685 M1), reverting the chrome
// to that mode's fallback (preset or none). Admin-gated. Idempotent: deleting an
// absent slot still returns 200.
func (h *Handler) DeleteBrandingLogo(w http.ResponseWriter, r *http.Request) {
	slot := chi.URLParam(r, "slot")
	if !validBrandingSlot(slot) {
		httpx.Error(w, http.StatusNotFound, "unknown logo slot")
		return
	}
	if err := h.q.DeleteBrandingAsset(r.Context(), slot); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to delete logo")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"slot": slot, "present": false})
}
