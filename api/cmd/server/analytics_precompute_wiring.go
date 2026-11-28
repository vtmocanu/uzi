package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analyticsprecompute"
	"github.com/vtmocanu/uzi/api/internal/handler"
	"github.com/vtmocanu/uzi/api/internal/sweeper"
)

// wireAnalyticsPrecompute returns passes with the analytics_precompute pass inserted immediately
// before the last one (the usage fold pass must stay last), and the scheduler that pass drives.
// With interval 0 (the setting is off) it returns passes unchanged and a nil scheduler: nothing is
// constructed and nothing is registered. ctx is the process context: cancelling it cancels a
// running build. The caller must call the scheduler's Wait at shutdown, after the sweeper has
// stopped and before the pool closes.
func wireAnalyticsPrecompute(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, passes []sweeper.Pass) ([]sweeper.Pass, *analyticsprecompute.Scheduler) {
	if interval <= 0 {
		return passes, nil
	}
	sched := analyticsprecompute.New(pool, interval, []analyticsprecompute.Section{{
		Name: handler.AllTimeSpendSection, Keys: handler.AllTimeSpendKeys(), Build: handler.BuildAllTimeSpendDocs,
	}})
	sched.Start(ctx)
	pass := sweeper.Pass{Name: "analytics_precompute", Run: sched.Pass}
	if len(passes) == 0 {
		return []sweeper.Pass{pass}, sched
	}
	out := make([]sweeper.Pass, 0, len(passes)+1)
	out = append(out, passes[:len(passes)-1]...)
	out = append(out, pass, passes[len(passes)-1])
	return out, sched
}
