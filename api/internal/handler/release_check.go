package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/releasecheck"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// PostReleaseCheckSnooze snoozes the admin escalation banner (PRD #836 M6) for the
// current upstream release. It reads the persisted latest_tag and, when non-empty,
// upserts KeyReleaseBannerSnoozeTag = latest_tag through the existing generic
// UpsertAppSetting (NO new SQL query), invalidates the settings cache, and returns the
// refreshed DTO — banner_snoozed is then true. Keying the snooze to the release TAG is
// what makes it auto-expire: a newer upstream release changes latest_tag, so the stored
// snooze no longer matches and the banner returns with no admin action. When no
// latest_tag has been fetched yet there is nothing to snooze, so it is a no-op that
// returns the current DTO (banner_snoozed false). Cookie-only admin write; nil-safe.
func (h *Handler) PostReleaseCheckSnooze(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := h.settings.ReleaseStatus(ctx)
	if err != nil {
		slog.Error("release-check: read status for snooze", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if st.LatestTag != "" {
		if h.q == nil {
			httpx.Error(w, http.StatusInternalServerError, "release check not configured")
			return
		}
		// Attribute the snooze to the acting admin when present; UpdatedBy is nullable, so
		// a missing actor stores NULL rather than failing.
		var updatedBy pgtype.UUID
		if actor, ok := mw.UserFromContext(ctx); ok {
			updatedBy = pgconv.UUID(actor.ID)
		}
		if _, err := h.q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
			Key:       settings.KeyReleaseBannerSnoozeTag,
			Value:     st.LatestTag,
			UpdatedBy: updatedBy,
		}); err != nil {
			slog.Error("release-check: persist banner snooze", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		// Drop the read cache so the refreshed DTO below reflects the new snooze tag.
		h.settings.Invalidate()
	}
	dto, derr := h.releaseCheckDTO(ctx)
	if derr != nil {
		slog.Error("release-check: build dto after snooze", "error", derr)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
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
		// Snoozed iff a snooze tag is set AND still matches the current latest_tag; a
		// newer release changes latest_tag, so the snooze auto-expires (PRD #836 M6).
		BannerSnoozed: st.BannerSnoozeTag != "" && st.BannerSnoozeTag == st.LatestTag,
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
