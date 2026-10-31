package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type prerequisitePageCostStore struct{ *prerequisiteMemory }

func (q *prerequisitePageCostStore) ListPrerequisitePendingDue(ctx context.Context, a store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.now = q.now.Add(499 * time.Millisecond)
	if q.row.State != "pending" || q.row.NextAttemptAt.Time.After(a.DueAt.Time) || a.AfterID == q.row.ID {
		return nil, nil
	}
	return []store.RecoveryPrerequisiteClosure{q.row}, nil
}
func (q *prerequisitePageCostStore) ListPrerequisiteStaleInvokedDue(ctx context.Context, _ store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.now = q.now.Add(499 * time.Millisecond)
	return nil, nil
}
func (q *prerequisitePageCostStore) ListPrerequisiteReconcileDue(ctx context.Context, _ store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.now = q.now.Add(100 * time.Millisecond)
	return nil, nil
}
func (q *prerequisitePageCostStore) ListPrerequisiteAuditDue(ctx context.Context, _ store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.now = q.now.Add(100 * time.Millisecond)
	return nil, nil
}
func (q *prerequisitePageCostStore) ListPrerequisiteCleanupDue(ctx context.Context, _ store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.now = q.now.Add(499 * time.Millisecond)
	return nil, nil
}

// Synthetic elapsed time, not a latency measurement. Each selection operation
// stays below its 500ms bound (the listing pair uses only 200ms). Rotation must
// give preparation an eight-second turn rather than prefetch away every turn.
func TestPrerequisitePreparationGetsFullTurnWithBoundedPageCosts(t *testing.T) {
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	q := &prerequisitePageCostStore{m}
	s.SetPrerequisiteStore(q, m.transaction)
	for pass := 0; pass < 5 && m.row.State == "pending"; pass++ {
		if _, err := s.SweepPrerequisiteClosures(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if m.row.State != "pinned" || m.row.Attempts != 1 || h.sends != 1 {
		t.Fatalf("preparation starved behind bounded selection reads: state=%s attempts=%d sends=%d", m.row.State, m.row.Attempts, h.sends)
	}
}
