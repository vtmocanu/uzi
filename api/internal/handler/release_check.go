package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/releasecheck"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Admin upstream-release-check surface (PRD #836 M3), mirroring the agent-source
// read/write split:
//   - GET  /admin/release-check   (RequireUser + RequireAdminRO) full facts + status
//   - POST /admin/release-check   (cookie-only RequireAuth + RequireAdmin) "Check now"
//
// Both return apitypes.ReleaseCheckStatusDTO. Unlike the world-readable /api/version,
// this DTO carries the RAW release Body (the markdown notes the admin card previews) —
// it is admin-only by route, so that is the correct home for it. No token is ever
// serialized. All reads go through h.settings (the read-through cache), so the GET is
// zero-egress; only the POST triggers a network fetch.

// GetReleaseCheck returns the release-check config + persisted facts + derived signals
// for the admin Updates card (PRD #836 M3). Admin read (RequireAdminRO). Read-only: it
// never triggers a check.
func (h *Handler) GetReleaseCheck(w http.ResponseWriter, r *http.Request) {
	dto, err := h.releaseCheckDTO(r.Context())
	if err != nil {
		slog.Error("release-check: build dto", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"release_check": dto})
}

// PostReleaseCheck runs "Check now" (PRD #836 M3): the SAME CheckForUpdate the interval
// Runner calls (one fn, two triggers). Cookie-only admin write. It mirrors
// PostAgentSourceUpdateCheck — run the check, then always rebuild and return the
// refreshed DTO so the card's facts/checked-at update. On an error status nothing was
// persisted (last-good facts stay), and the token-scrubbed message rides in the status.
func (h *Handler) PostReleaseCheck(w http.ResponseWriter, r *http.Request) {
	if h.releaseCheck == nil {
		httpx.Error(w, http.StatusInternalServerError, "release check not configured")
		return
	}
	res, err := h.releaseCheck.CheckForUpdate(r.Context())
	if err != nil {
		// CheckForUpdate records every failure in its result and returns nil today; a
		// non-nil error is unexpected, so surface it as a 500 rather than swallow it.
		slog.Error("release-check: check now", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "check failed")
		return
	}
	dto, derr := h.releaseCheckDTO(r.Context())
	if derr != nil {
		slog.Error("release-check: build dto after check", "error", derr)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// On an error status the check persisted nothing, so releaseCheckDTO derived Status
	// from the PRIOR facts ("ok"/"never"). Override it so the web's error branch fires,
	// and surface the (token-scrubbed) reason — SanitizeTTY defensively, as the
	// agent-source update-check error is. "error" is the same status-string literal
	// the release-check Result and PostAgentSourceUpdateCheck use.
	if res.Status == "error" {
		dto.Status = "error"
		if res.Message != "" {
			dto.Message = termsafe.SanitizeTTY(res.Message)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"release_check": dto})
}

// releaseCheckDTO assembles the admin release-check view from h.settings: the two
// toggles + interval, the running version, the persisted remote facts (Body included —
// admin-only), and the read-time derivations. Status distinguishes disabled / never
// checked / ok from the enable flag + a stamped CheckedAt. PURE read: no egress.
func (h *Handler) releaseCheckDTO(ctx context.Context) (apitypes.ReleaseCheckStatusDTO, error) {
	enabled, err := h.settings.ReleaseCheckEnabled(ctx)
	if err != nil {
		return apitypes.ReleaseCheckStatusDTO{}, err
	}
	banner, err := h.settings.ReleaseCheckBannerEnabled(ctx)
	if err != nil {
		return apitypes.ReleaseCheckStatusDTO{}, err
	}
	interval, err := h.settings.ReleaseCheckInterval(ctx)
	if err != nil {
		return apitypes.ReleaseCheckStatusDTO{}, err
	}
	st, err := h.settings.ReleaseStatus(ctx)
	if err != nil {
		return apitypes.ReleaseCheckStatusDTO{}, err
	}

	dto := apitypes.ReleaseCheckStatusDTO{
		Enabled:         enabled,
		BannerEnabled:   banner,
		Interval:        interval.String(),
		RunningVersion:  h.version,
		LatestTag:       st.LatestTag,
		LatestName:      st.LatestName,
		Body:            st.Body,
		NotesURL:        st.NotesURL,
		PublishedAt:     st.PublishedAt,
		CheckedAt:       st.CheckedAt,
		UpdateAvailable: releasecheck.UpdateAvailable(h.version, st.LatestTag),
		FarBehind:       releasecheck.FarBehind(h.version, st.LatestTag, st.PublishedAt, h.clock()),
		Security:        releasecheck.Security(st.Body),
	}
	switch {
	case !enabled:
		dto.Status = "disabled"
	case st.CheckedAt == "":
		dto.Status = "never"
	default:
		dto.Status = "ok"
	}
	return dto, nil
}
