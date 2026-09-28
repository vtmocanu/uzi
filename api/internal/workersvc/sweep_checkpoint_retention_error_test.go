package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// backfillClockErrStore fails the checkpoint-retention pass's first read, so
// ReconcileCheckpointRetentions returns a candidate-list error.
type backfillClockErrStore struct{ *fakeStore }

func (backfillClockErrStore) GetCheckpointRetentionBackfillNow(context.Context) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{}, errors.New("backfill clock unavailable")
}

// unusedConnAcquirer satisfies ConnAcquirer for a wiring check; no lock is ever taken here.
type unusedConnAcquirer struct{}

func (unusedConnAcquirer) Acquire(context.Context) (*pgxpool.Conn, error) {
	return nil, errors.New("unused")
}

// TestSweepContinuesPastCheckpointRetentionError (#1810 M1 rework): a
// ReconcileCheckpointRetentions error is logged, not returned, so the passes after it still run
// (the upload-retry-window expiry here) and the sweep succeeds, with the retention count left 0.
func TestSweepContinuesPastCheckpointRetentionError(t *testing.T) {
	fs := &fakeStore{stalledUploadsRows: 3}
	p := testParams()
	p.RecoveryUploadRetryWindow = 24 * time.Hour
	svc := New(backfillClockErrStore{fs}, newBox(t), p)
	svc.SetRetentionLockPool(unusedConnAcquirer{})
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	svc.SetBackground(func(fn func()) { fn() })
	svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return nil })
	svc.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { return nil })
	svc.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
		return nil, nil
	})
	if !svc.supersessionWired() {
		t.Fatal("setup: supersession is not wired, so the retention pass would be inert")
	}
	if _, err := svc.ReconcileCheckpointRetentions(context.Background()); err == nil {
		t.Fatal("setup: ReconcileCheckpointRetentions returned no error")
	}

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v, want the checkpoint-retention error logged, not returned", err)
	}
	if len(fs.expireStalledWindows) != 1 || res.RecoveryStalled != 3 {
		t.Fatalf("ExpireStalledUploads calls = %d, RecoveryStalled = %d; want 1 and 3 (the pass after the failed one still ran)",
			len(fs.expireStalledWindows), res.RecoveryStalled)
	}
	if res.CheckpointRetentionsReconciled != 0 {
		t.Fatalf("CheckpointRetentionsReconciled = %d, want 0 for a failed pass", res.CheckpointRetentionsReconciled)
	}
}
