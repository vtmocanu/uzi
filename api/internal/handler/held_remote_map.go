package handler

import (
	"errors"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// heldRemoteForRecovery maps workersvc's "run, repo or forge connection is gone" sentinel onto
// recovery.ErrHeldRemoteGone (the recovery package cannot import workersvc); every other error,
// and nil, passes through unchanged.
func heldRemoteForRecovery(err error) error {
	if errors.Is(err, workersvc.ErrHeldRemoteGone) {
		return recovery.ErrHeldRemoteGone
	}
	return err
}

// heldListAllowFor is step B's per-worker forge-listing gate: one limiter bucket per worker,
// keyed apart from every route-pattern bucket the same limiter serves.
func heldListAllowFor(lim *mw.Limiter) func(workerID uuid.UUID) bool {
	return func(workerID uuid.UUID) bool { return lim.Allow("held-final-list|" + workerID.String()) }
}
