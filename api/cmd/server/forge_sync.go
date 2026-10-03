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
	return healthsvc.Config{ForgeSyncRegistry: registry, ForgeSyncInterval: engine.Interval()}
}

func wireHandlerForgeSync(h interface{ SetRepoSyncInvalidator(func(uuid.UUID)) }, cfg healthsvc.Config) {
	h.SetRepoSyncInvalidator(cfg.ForgeSyncRegistry.Invalidate)
}
