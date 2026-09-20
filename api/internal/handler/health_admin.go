package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// healthCacheTTL is how long GetAdminHealth serves a cached evaluation (PRD #1484):
// admin tabs poll every 10 s and the M2 evaluator runs once a minute, so a short cache
// collapses a fleet of open tabs to one evaluation per window while staying well under
// the poll cadence. Only the CHECK-evaluation portion (status/checks/counts) is cached;
// episode_id and the PER-CALLER snoozed_until are resolved per request (M2).
const healthCacheTTL = 5 * time.Second

// healthSnoozeDuration is how long POST /api/admin/health/snooze silences the caller's
// Danger banner for the current episode (PRD #1484 D2).
const healthSnoozeDuration = time.Hour

// healthService lazily constructs the admin-health evaluator from the collaborators the
// handler already holds. Constructed once under healthSvcOnce so a struct-literal test
// handler (which does not call New) is race-safe, mirroring memo()/recovery(). *store.Queries
// satisfies healthsvc.Store structurally and *settings.Cache satisfies healthsvc.Settings.
func (h *Handler) healthService() *healthsvc.Service {
	h.healthSvcOnce.Do(func() {
		if h.healthSvc == nil {
			h.healthSvc = healthsvc.New(healthsvc.Config{
				Store:               h.q,
				Pool:                h.pool,
				Settings:            h.settings,
				SlackState:          h.slackState,
				Now:                 h.now,
				HostedWorkerVersion: h.cfg.HostedWorkerVersion,
				RunningVersion:      h.version,
				HeartbeatStale:      h.cfg.WorkerHeartbeatStale,
				// The custody admission ceiling ClaimRun/recovery gate on, so custody.holds
				// never disagrees with the claim path. int32() is a compile-time conversion.
				CustodyHoldLimit: int32(workersvc.CustodyHoldLimit),
				// controller.report's boot grace anchor (M2). A struct-literal test handler
				// with a zero startedAt makes the grace not apply, the safe direction.
				BootTime:         h.startedAt,
				CIWatchMaxRefs:   h.cfg.CIWatchMaxRefs,
				CIWatchRunWindow: h.cfg.CIWatchRunWindow,
				// NO Registry in the lazy fallback: a struct-literal test handler wired no
				// loops, so the loops check degrades to na rather than inventing a warn. The
				// production path injects the SHARED registry via SetHealthService.
			})
		}
	})
	return h.healthSvc
}

// GetAdminHealth returns the admin-health document (PRD #1484 M1): the closed registry of
// checks with the overall verdict, per-severity tally and each check's evidence. Admin
// read (RequireUser + RequireAdminRO); a uzc_ token or a non-admin cookie is 403 before
// this runs, so the owner/worker names it carries never reach a non-admin. The result is
// cached for healthCacheTTL.
func (h *Handler) GetAdminHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	doc, err := h.cachedHealthDoc(ctx)
	if err != nil {
		slog.Error("admin health: evaluate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// episode_id and the PER-CALLER snoozed_until are resolved per request, NOT served from
	// the 5 s shared check cache: two admins must each get their OWN snoozed_until, and the
	// banner must appear/clear on the current episode. Both are cheap indexed reads. doc is a
	// value copy of the cached evaluation, so mutating these fields never poisons the cache.
	var callerID uuid.UUID
	if u, ok := mw.UserFromContext(ctx); ok {
		callerID = u.ID
	}
	doc.EpisodeID, doc.SnoozedUntil = h.resolveEpisodeAndSnooze(ctx, callerID)
	httpx.JSON(w, http.StatusOK, doc)
}

// resolveEpisodeAndSnooze reads the open danger episode's id and, when one is open, the
// caller's own snooze against it (PRD #1484 M2). Both nil when no episode is open. A read
// error degrades to nil (the banner simply does not offer a snooze) rather than failing the
// whole health document — the checks are what the page is for. Guarded on h.q so a
// struct-literal test handler with no store returns nil/nil.
func (h *Handler) resolveEpisodeAndSnooze(ctx context.Context, callerID uuid.UUID) (episodeID, snoozedUntil *string) {
	if h.q == nil {
		return nil, nil
	}
	ep, err := h.q.GetOpenHealthEpisode(ctx)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("admin health: read open episode", "error", err)
		}
		return nil, nil
	}
	id := ep.ID.String()
	episodeID = &id

	if callerID == uuid.Nil {
		return episodeID, nil
	}
	snooze, err := h.q.GetHealthBannerSnooze(ctx, store.GetHealthBannerSnoozeParams{
		EpisodeID: ep.ID,
		UserID:    callerID,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("admin health: read banner snooze", "error", err)
		}
		return episodeID, nil
	}
	if snooze.Valid {
		s := snooze.Time.UTC().Format(time.RFC3339)
		snoozedUntil = &s
	}
	return episodeID, snoozedUntil
}

// PostAdminHealthSnooze snoozes the caller's Danger banner for the current episode for
// healthSnoozeDuration (PRD #1484 D2). Cookie-only admin write (RequireAuth + RequireAdmin
// — a uzc_/uza_ CLI token 401s/403s before this runs). 409 when no episode is open (there
// is nothing to snooze). On success it upserts the per-(episode, caller) snooze and returns
// 200 with the new snoozed_until.
func (h *Handler) PostAdminHealthSnooze(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.q == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ep, err := h.q.GetOpenHealthEpisode(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusConflict, "no open health episode to snooze")
			return
		}
		slog.Error("admin health snooze: read open episode", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	u, ok := mw.UserFromContext(ctx)
	if !ok {
		// RequireAuth guarantees a user; this is a defensive guard.
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	until := h.clock().Add(healthSnoozeDuration)
	if err := h.q.UpsertHealthBannerSnooze(ctx, store.UpsertHealthBannerSnoozeParams{
		EpisodeID:    ep.ID,
		UserID:       u.ID,
		SnoozedUntil: pgconv.Time(until),
	}); err != nil {
		slog.Error("admin health snooze: upsert", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"episode_id":    ep.ID.String(),
		"snoozed_until": until.UTC().Format(time.RFC3339),
	})
}

// cachedHealthDoc returns the last evaluation when it is younger than healthCacheTTL, else
// re-evaluates and caches. The evaluation runs OUTSIDE the lock (it does DB I/O), so two
// concurrent misses may both evaluate — a harmless last-write-wins, not a correctness bug:
// the cache is a throttle, not a singleflight.
func (h *Handler) cachedHealthDoc(ctx context.Context) (apitypes.HealthDocDTO, error) {
	now := h.clock()

	h.healthCacheMu.Lock()
	if h.healthCached != nil && now.Sub(h.healthCachedAt) < healthCacheTTL {
		doc := *h.healthCached
		h.healthCacheMu.Unlock()
		return doc, nil
	}
	h.healthCacheMu.Unlock()

	doc, err := h.healthService().Evaluate(ctx)
	if err != nil {
		return apitypes.HealthDocDTO{}, err
	}

	h.healthCacheMu.Lock()
	cached := doc
	h.healthCached = &cached
	h.healthCachedAt = h.clock()
	h.healthCacheMu.Unlock()
	return doc, nil
}
