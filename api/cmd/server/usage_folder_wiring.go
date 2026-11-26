package main

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/sweeper"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// lifecycleSetter is the slice of *workersvc.Service the wiring needs.
type lifecycleSetter interface {
	SetLifecycle(workersvc.RunLifecycle)
}

// usageFoldRunner is the slice of *store.UsageFolder the wiring needs: it is a lifecycle
// hook (the fast path) and has a sweeper pass (the safety net).
type usageFoldRunner interface {
	workersvc.RunLifecycle
	Pass(ctx context.Context) (int64, error)
}

// wireUsageFolder installs a lifecycle that fans every status notification out to the
// existing lifecycle (board column automation; may be nil) and to the usage folder, and
// returns the sweeper pass main.go must register so terminal writes that never kicked the
// folder are still folded. Dropping the returned pass from sweeper.New silently disables the
// safety net, which is why it is the return value rather than registered here.
func wireUsageFolder(svc lifecycleSetter, existing workersvc.RunLifecycle, folder usageFoldRunner) sweeper.Pass {
	svc.SetLifecycle(workersvc.TeeLifecycle(existing, folder))
	return sweeper.Pass{Name: "usage_fold_materialize", Run: folder.Pass}
}
