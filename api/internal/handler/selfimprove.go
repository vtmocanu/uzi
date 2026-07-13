package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Self-improvement admin config (PRD #46 M5, Decision 9). A dedicated admin
// endpoint — NOT the generic settings PUT — because two of the keys need handling
// the generic path can't give: selfimprove_user_id must come from the SESSION admin
// (audit H3, never a body), and selfimprove_repo must be validated as a repo the
// session admin owns. Those two are engine-managed (absent from settings.Defaults,
// so the generic PUT rejects them); this endpoint writes them directly, alongside
// enabled/interval, then invalidates the cache so the engine reads fresh.

// selfimproveConfigDTO is the admin-facing view of the self-improvement config.
type selfimproveConfigDTO struct {
	Enabled   bool    `json:"enabled"`
	Interval  string  `json:"interval"`
	RepoID    *string `json:"repo_id"`
	RepoPath  *string `json:"repo_path"`
	UserID    *string `json:"user_id"`
	UserEmail *string `json:"user_email"`
	LastRunAt *string `json:"last_run_at"`
	// Active reports whether a self_improve run is currently in flight (the engine
	// skips a due cycle while one is).
	Active bool `json:"active"`
}

// GetSelfimproveConfig returns the current self-improvement config for the admin
// settings surface. Admin-only (mounted under RequireAdmin).
func (h *Handler) GetSelfimproveConfig(w http.ResponseWriter, r *http.Request) {
	dto, err := h.selfimproveConfig(r.Context())
	if err != nil {
		slog.Error("get selfimprove config", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"selfimprove": dto})
}

type putSelfimproveRequest struct {
	Enabled bool `json:"enabled"`
	// Interval is optional; when present it is validated as a Go duration and
	// written. Absent leaves the stored value (or default) untouched.
	Interval *string `json:"interval"`
	// RepoID is the connected uzi repo the job runs against. Required when enabling;
	// must be a repo the SESSION admin owns.
	RepoID *string `json:"repo_id"`
}

// PutSelfimproveConfig enables/disables and configures the self-improvement job.
// The enabling admin becomes the run owner: selfimprove_user_id is set from the
// SESSION admin, never the body (audit H3), so admin A can never schedule autonomous
// spend on admin B's token. Enabling requires a repo_id the session admin owns.
func (h *Handler) PutSelfimproveConfig(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req putSelfimproveRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Ordered writes: repo/user_id/interval FIRST, the enabled flag LAST. There is no
	// cross-key invariant here (unlike the generic settings PUT), so no transaction is
	// needed — and a partial write is safe by construction: if it stops before the
	// enabled flip, the flag never turns on without an owner+repo, and if it stops
	// after, the engine reads an incomplete config and simply skips (not-configured).
	// Ordering enabled last makes "enabled ⇒ configured" hold even under a crash.
	if req.Interval != nil {
		v := strings.TrimSpace(*req.Interval)
		if err := settings.Validate(settings.KeySelfimproveInterval, v); err != nil {
			httpx.Error(w, http.StatusBadRequest, "interval: "+err.Error())
			return
		}
		if !h.putSelfimproveKey(w, r.Context(), actor.ID, settings.KeySelfimproveInterval, v) {
			return
		}
	}

	if req.Enabled {
		// Enabling: a repo the session admin owns, and the session admin as the run
		// owner — taken from the session, NEVER the body (audit H3). The request struct
		// carries no user id at all, so there is nothing to spoof.
		if req.RepoID == nil || strings.TrimSpace(*req.RepoID) == "" {
			httpx.Error(w, http.StatusBadRequest, "repo_id is required to enable self-improvement")
			return
		}
		repoID, err := uuid.Parse(strings.TrimSpace(*req.RepoID))
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid repo_id")
			return
		}
		if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: repoID, UserID: actor.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusBadRequest, "repo_id must be a repo you have connected")
				return
			}
			slog.Error("selfimprove: validate repo", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !h.putSelfimproveKey(w, r.Context(), actor.ID, settings.KeySelfimproveRepo, repoID.String()) {
			return
		}
		if !h.putSelfimproveKey(w, r.Context(), actor.ID, settings.KeySelfimproveUserID, actor.ID.String()) {
			return
		}
	}

	if !h.putSelfimproveKey(w, r.Context(), actor.ID, settings.KeySelfimproveEnabled, boolToSetting(req.Enabled)) {
		return
	}

	// Drop the cache so the engine's next tick reads the new config (M1 carry-forward).
	h.settings.Invalidate()

	dto, err := h.selfimproveConfig(r.Context())
	if err != nil {
		slog.Error("selfimprove: read back config", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"selfimprove": dto})
}

// putSelfimproveKey upserts one setting, writing an error response and returning
// false on failure so the caller can abort the sequence.
func (h *Handler) putSelfimproveKey(w http.ResponseWriter, ctx context.Context, actorID uuid.UUID, key, value string) bool {
	if _, err := h.q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
		Key: key, Value: value, UpdatedBy: pgUUID(actorID),
	}); err != nil {
		slog.Error("selfimprove: upsert setting", "key", key, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

// selfimproveConfig reads the current config into the DTO, resolving the enabling
// admin's email and the repo path for display (both best-effort — a deleted user or
// disconnected repo leaves the id without its label rather than failing the read).
func (h *Handler) selfimproveConfig(ctx context.Context) (selfimproveConfigDTO, error) {
	enabled, err := h.settings.SelfimproveEnabled(ctx)
	if err != nil {
		return selfimproveConfigDTO{}, err
	}
	interval, err := h.settings.SelfimproveInterval(ctx)
	if err != nil {
		return selfimproveConfigDTO{}, err
	}
	repoStr, _ := h.settings.SelfimproveRepo(ctx)
	userStr, _ := h.settings.SelfimproveUserID(ctx)
	lastRun, hasLast, _ := h.settings.SelfimproveLastRunAt(ctx)

	dto := selfimproveConfigDTO{Enabled: enabled, Interval: interval.String()}
	if hasLast {
		s := lastRun.UTC().Format("2006-01-02T15:04:05Z07:00")
		dto.LastRunAt = &s
	}

	var ownerID uuid.UUID
	if userStr = strings.TrimSpace(userStr); userStr != "" {
		dto.UserID = &userStr
		if uid, err := uuid.Parse(userStr); err == nil {
			ownerID = uid
			if u, err := h.q.GetUserByID(ctx, uid); err == nil {
				email := u.Email
				dto.UserEmail = &email
			}
		}
	}
	if repoStr = strings.TrimSpace(repoStr); repoStr != "" {
		dto.RepoID = &repoStr
		if rid, err := uuid.Parse(repoStr); err == nil && ownerID != (uuid.UUID{}) {
			if repo, err := h.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: rid, UserID: ownerID}); err == nil {
				path := repo.PathWithNamespace
				dto.RepoPath = &path
			}
		}
	}

	if active, err := h.q.CountActiveSelfImproveRuns(ctx); err == nil {
		dto.Active = active > 0
	}
	return dto, nil
}

// boolToSetting renders a bool as the strict "true"/"false" the settings layer
// stores and validates.
func boolToSetting(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
