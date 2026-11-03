package workersvc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// countingAcquirer is the retention lock's connection source: it counts each Acquire (one per
// detached supersession operation) and fails it, so no lock is taken.
type countingAcquirer struct{ n atomic.Int32 }

func (a *countingAcquirer) Acquire(context.Context) (*pgxpool.Conn, error) {
	a.n.Add(1)
	return nil, errors.New("countingAcquirer: no connection")
}

// slotStore returns one other-run record for the branch from both slot listings.
type slotStore struct {
	*fakeStore
	row store.CheckpointRetention
}

func (s *slotStore) ListActiveCheckpointRetentionsForBranch(context.Context, store.ListActiveCheckpointRetentionsForBranchParams) ([]store.CheckpointRetention, error) {
	return []store.CheckpointRetention{s.row}, nil
}

func (s *slotStore) ListCheckpointRetentionsForBranch(context.Context, store.ListCheckpointRetentionsForBranchParams) ([]store.CheckpointRetention, error) {
	return []store.CheckpointRetention{s.row}, nil
}

func newSlotService(t *testing.T) (*Service, *countingAcquirer) {
	t.Helper()
	st := &slotStore{fakeStore: &fakeStore{}, row: store.CheckpointRetention{RunID: uuid.New()}}
	svc := New(st, newBox(t), testParams())
	acq := &countingAcquirer{}
	svc.SetRetentionLockPool(acq)
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	svc.SetBackground(func(fn func()) { fn() })
	unreached := errors.New("forge seam reached without a lock")
	svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return unreached })
	svc.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { return unreached })
	svc.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
		return nil, unreached
	})
	if !svc.supersessionWired() {
		t.Fatal("the supersession seams are not all wired")
	}
	return svc, acq
}

func slotRun() store.Run {
	return store.Run{ID: uuid.New(), Kind: runkind.Issue, Status: "running", RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
}

// A cancelled context stops the claim before it starts a detached supersession; a live one
// (the control) starts exactly one, which is refused as not_descendant because the lock is busy.
func TestClaimCheckpointSlotCancelledContextStartsNoOperation(t *testing.T) {
	svc, acq := newSlotService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := svc.claimCheckpointSlot(ctx, slotRun(), "agent/issue-1"); got != "not_descendant" {
		t.Fatalf("cancelled claim = %q, want not_descendant", got)
	}
	if n := acq.n.Load(); n != 0 {
		t.Fatalf("a cancelled claim acquired %d connections, want 0", n)
	}
	if got := svc.claimCheckpointSlot(context.Background(), slotRun(), "agent/issue-1"); got != "not_descendant" {
		t.Fatalf("live claim = %q, want not_descendant", got)
	}
	if n := acq.n.Load(); n != 1 {
		t.Fatalf("a live claim acquired %d connections, want 1", n)
	}
}

func TestFreeCheckpointSlotCancelledContextStartsNoOperation(t *testing.T) {
	svc, acq := newSlotService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if svc.freeCheckpointSlot(ctx, slotRun(), "agent/issue-1") {
		t.Fatal("cancelled free reported true")
	}
	if n := acq.n.Load(); n != 0 {
		t.Fatalf("a cancelled free acquired %d connections, want 0", n)
	}
	if svc.freeCheckpointSlot(context.Background(), slotRun(), "agent/issue-1") {
		t.Fatal("live free with a failing lock reported true")
	}
	if n := acq.n.Load(); n != 1 {
		t.Fatalf("a live free acquired %d connections, want 1", n)
	}
}
