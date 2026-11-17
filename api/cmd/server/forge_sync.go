package main

import (
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/poller"
)

// wireForgeSync runs before the poller starts. The health evaluator uses the
// effective cadence from this same engine, including New's interval clamp.
func wireForgeSync(engine *poller.Engine, now func() time.Time) healthsvc.Config {
	registry := healthsvc.NewSyncRegistry(now)
	engine.SetSyncAttempt(registry.Begin)
	itRegistry := healthsvc.NewSyncRegistry(now)
	engine.SetIssueTrackerSyncAttempt(itRegistry.Begin)
	return healthsvc.Config{
		ForgeSyncRegistry:        registry,
		IssueTrackerSyncRegistry: itRegistry,
		ForgeSyncInterval:        engine.Interval(),
	}
}

func wireHandlerForgeSync(h interface{ SetRepoSyncInvalidator(func(uuid.UUID)) }, cfg healthsvc.Config) {
	h.SetRepoSyncInvalidator(func(id uuid.UUID) {
		cfg.ForgeSyncRegistry.Invalidate(id)
		cfg.IssueTrackerSyncRegistry.Invalidate(id)
	})
}
