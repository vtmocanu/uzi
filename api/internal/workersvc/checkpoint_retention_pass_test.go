package workersvc

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_retention_pass_test.go pins the checkpoint-retention pass's time budget and arm
// rotation (PRD #1810, CodeRabbit on PR #1819) without a database: a store that answers every
// candidate list with an empty page and records which arm listed, in what order.

// armOrderStore records the order the retention pass's arms list their candidates in. Every
// list returns an empty page; the backfill clock and watermark writes succeed.
type armOrderStore struct {
	*fakeStore
	mu     sync.Mutex
	listed []string
}

func (s *armOrderStore) note(arm string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = append(s.listed, arm)
}

func (s *armOrderStore) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.listed
	s.listed = nil
	return out
}

func (s *armOrderStore) GetCheckpointRetentionBackfillNow(context.Context) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}, nil
}

func (s *armOrderStore) ListCheckpointRetentionBackfill(context.Context, store.ListCheckpointRetentionBackfillParams) ([]store.ListCheckpointRetentionBackfillRow, error) {
	s.note("backfill")
	return nil, nil
}

func (s *armOrderStore) AdvanceCheckpointRetentionBackfillWatermark(context.Context, pgtype.Timestamptz) (int64, error) {
	return 0, nil
}

func (s *armOrderStore) ListDueCheckpointPublishAttempts(context.Context, store.ListDueCheckpointPublishAttemptsParams) ([]store.CheckpointPublishAttempt, error) {
	s.note("attempts")
	return nil, nil
}

func (s *armOrderStore) ListCheckpointRetentionWork(context.Context, store.ListCheckpointRetentionWorkParams) ([]store.CheckpointRetention, error) {
	s.note("work")
	return nil, nil
}

func (s *armOrderStore) ListUnheldCheckpointRetentions(context.Context, store.ListUnheldCheckpointRetentionsParams) ([]store.CheckpointRetention, error) {
	s.note("unheld")
	return nil, nil
}

func (s *armOrderStore) ListCheckpointRetentionAudit(context.Context, store.ListCheckpointRetentionAuditParams) ([]store.CheckpointRetention, error) {
	s.note("audit")
	return nil, nil
}

// newArmOrderService is a Service whose retention and supersession seams are all wired (so the
// pass is not inert) on an armOrderStore. No forge seam is ever reached: every page is empty.
func newArmOrderService(t *testing.T) (*Service, *armOrderStore) {
	t.Helper()
	st := &armOrderStore{fakeStore: &fakeStore{}}
	svc := New(st, newBox(t), testParams())
	svc.SetRetentionLockPool(unusedConnAcquirer{})
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	svc.SetBackground(func(fn func()) { fn() })
	unreached := errors.New("forge seam reached with an empty page")
	svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return unreached })
	svc.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { return unreached })
	svc.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
		return nil, unreached
	})
	if !svc.supersessionWired() {
		t.Fatal("setup: supersession is not wired, so the retention pass would be inert")
	}
	return svc, st
}

// TestRetentionArmOrderRotates: the pure rotation. Each cursor value starts at arm cursor mod n and
// keeps the canonical cyclic order, and n consecutive cursors start at every arm exactly once.
func TestRetentionArmOrderRotates(t *testing.T) {
	const n = 4
	for base := uint32(0); base < 3*n; base += n {
		firsts := map[int]bool{}
		for c := base; c < base+n; c++ {
			order := retentionArmOrder(c, n)
			if len(order) != n {
				t.Fatalf("retentionArmOrder(%d, %d) = %v, want %d arms", c, n, order, n)
			}
			for i, arm := range order {
				if want := (int(c%n) + i) % n; arm != want {
					t.Fatalf("retentionArmOrder(%d, %d) = %v, want a rotation of the canonical order starting at %d", c, n, order, c%n)
				}
			}
			firsts[order[0]] = true
		}
		if len(firsts) != n {
			t.Fatalf("cursors %d..%d start at arms %v, want every one of the %d arms first once", base, base+n-1, firsts, n)
		}
	}
	// The cursor wraps at the top of uint32 without skipping or panicking.
	if got := retentionArmOrder(^uint32(0), n); got[0] != int(^uint32(0)%n) {
		t.Fatalf("retentionArmOrder(max) = %v, want it to start at %d", got, ^uint32(0)%n)
	}
	if got := retentionArmOrder(7, 0); len(got) != 0 {
		t.Fatalf("retentionArmOrder(7, 0) = %v, want empty", got)
	}
}

// TestReconcileCheckpointRetentionsRotatesArms: through the real pass, backfill always lists first
// and the four forge-calling arms start from the next arm on every pass, in canonical cyclic order.
func TestReconcileCheckpointRetentionsRotatesArms(t *testing.T) {
	svc, st := newArmOrderService(t)
	canonical := []string{"attempts", "work", "unheld", "audit"}
	for pass := 0; pass < 2*len(canonical); pass++ {
		if _, err := svc.ReconcileCheckpointRetentions(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		want := []string{"backfill"}
		for i := range canonical {
			want = append(want, canonical[(pass+i)%len(canonical)])
		}
		if got := st.take(); !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d listed %v, want %v", pass, got, want)
		}
	}
}

// TestReconcileCheckpointRetentionsSpentBudgetStartsNothing: a pass whose budget is already spent
// lists no forge-calling arm (only backfill's page is read, and it would start none of its
// records) and returns nil, so Sweep's later passes run.
func TestReconcileCheckpointRetentionsSpentBudgetStartsNothing(t *testing.T) {
	svc, st := newArmOrderService(t)
	svc.retentionPassBudget = 0
	n, err := svc.ReconcileCheckpointRetentions(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("ReconcileCheckpointRetentions = %d, %v; want 0, nil for a spent budget", n, err)
	}
	if got := st.take(); !reflect.DeepEqual(got, []string{"backfill"}) {
		t.Fatalf("listed %v, want only the backfill page (every forge arm left for the next tick)", got)
	}
}
