package handler

import (
	"errors"

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
