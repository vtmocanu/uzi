package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// healthCacheTTL is how long GetAdminHealth serves a cached evaluation (PRD #1484):
// admin tabs poll every 10 s and the M2 evaluator runs once a minute, so a short cache
// collapses a fleet of open tabs to one evaluation per window while staying well under
// the poll cadence.
const healthCacheTTL = 5 * time.Second

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
	doc, err := h.cachedHealthDoc(r.Context())
	if err != nil {
		slog.Error("admin health: evaluate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, doc)
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
