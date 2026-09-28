package handler

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// TestRecoveryAccessorWiresCustodySettleLiveDB pins PRD #1810 D3's wiring: the recovery service
// the handler builds (the lazy recovery() accessor, the only production construction site) calls
// the worker service's SettleRetainedCheckpoint after an owner discard commits. The settle is
// observed at its dispatch (the worker service's background seam); the delete itself is proven in
// workersvc (checkpoint_settlement_livedb_test.go). A handler without a worker service builds the
// recovery service with no hook and the discard still succeeds.
func TestRecoveryAccessorWiresCustodySettleLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h := e.handler()
	var dispatched atomic.Int32
	h.wsvc.SetRetentionLockPool(e.pool)
	h.wsvc.SetForgeBaseURLAllowed(func(string) bool { return true })
	h.wsvc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return nil })
	h.wsvc.SetBackground(func(func()) { dispatched.Add(1) })

	ok, err := h.recovery().DiscardHold(e.ctx, e.user, e.run, e.holdID)
	if err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v; want discarded", ok, err)
	}
	if n := dispatched.Load(); n != 1 {
		t.Fatalf("settle dispatches after the discard = %d, want 1 (the hook is not wired to SettleRetainedCheckpoint)", n)
	}

	e2 := newRecoveryEnv(t)
	bare := e2.handler()
	bare.wsvc = nil
	if ok, err := bare.recovery().DiscardHold(e2.ctx, e2.user, e2.run, e2.holdID); err != nil || !ok {
		t.Fatalf("DiscardHold without a worker service = %v, %v; want discarded", ok, err)
	}
}
