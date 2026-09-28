package workersvc

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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

// timedArmStore answers the one arm under test with a page of rows (every other arm lists an empty
// page) and accepts the attempts arm's defer of a row it could not resolve.
type timedArmStore struct {
	*fakeStore
	arm  string
	rows int
}

func (s *timedArmStore) page(arm string) int {
	if arm == s.arm {
		return s.rows
	}
	return 0
}

func (s *timedArmStore) retentionPage(arm, state string) []store.CheckpointRetention {
	out := make([]store.CheckpointRetention, s.page(arm))
	for i := range out {
		out[i] = store.CheckpointRetention{RunID: uuid.New(), State: state}
	}
	return out
}

func (s *timedArmStore) GetCheckpointRetentionBackfillNow(context.Context) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}, nil
}

func (s *timedArmStore) ListCheckpointRetentionBackfill(context.Context, store.ListCheckpointRetentionBackfillParams) ([]store.ListCheckpointRetentionBackfillRow, error) {
	return nil, nil
}

func (s *timedArmStore) AdvanceCheckpointRetentionBackfillWatermark(context.Context, pgtype.Timestamptz) (int64, error) {
	return 0, nil
}

func (s *timedArmStore) ListDueCheckpointPublishAttempts(context.Context, store.ListDueCheckpointPublishAttemptsParams) ([]store.CheckpointPublishAttempt, error) {
	out := make([]store.CheckpointPublishAttempt, s.page("attempts"))
	for i := range out {
		out[i] = store.CheckpointPublishAttempt{ID: uuid.New(), RunID: uuid.New()}
	}
	return out, nil
}

func (s *timedArmStore) DeferCheckpointPublishAttempt(context.Context, store.DeferCheckpointPublishAttemptParams) (int64, error) {
	return 1, nil
}

// ListCheckpointRetentionWork alternates settling and superseding rows, so the two operations the
// pass starts before its budget is spent cover both of the work arm's locked operations (the
// settle and reconcileSuperseding).
func (s *timedArmStore) ListCheckpointRetentionWork(context.Context, store.ListCheckpointRetentionWorkParams) ([]store.CheckpointRetention, error) {
	out := s.retentionPage("work", retentionSettling)
	for i := 1; i < len(out); i += 2 {
		out[i].State = retentionSuperseding
	}
	return out, nil
}

func (s *timedArmStore) ListUnheldCheckpointRetentions(context.Context, store.ListUnheldCheckpointRetentionsParams) ([]store.CheckpointRetention, error) {
	return s.retentionPage("unheld", retentionRetained), nil
}

func (s *timedArmStore) ListCheckpointRetentionAudit(context.Context, store.ListCheckpointRetentionAuditParams) ([]store.CheckpointRetention, error) {
	return s.retentionPage("audit", "deleted"), nil
}

// slowAcquirer is the retention lock's connection source for a pure unit test: every Acquire (one
// per locked operation the pass starts) records how long its context had left, spends per (or
// until that context ends), and fails, so the operation ends with no lock taken.
type slowAcquirer struct {
	per  time.Duration
	mu   sync.Mutex
	left []time.Duration // the remaining time of each Acquire's context; -1 when it had no deadline
}

func (a *slowAcquirer) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	a.mu.Lock()
	a.left = append(a.left, left)
	a.mu.Unlock()
	select {
	case <-time.After(a.per):
	case <-ctx.Done():
	}
	return nil, errors.New("slowAcquirer: no connection")
}

func (a *slowAcquirer) acquired() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Duration(nil), a.left...)
}

// TestRetentionPassArmsCheckBudgetPerRowAndUseSweepTimeout: for each forge-calling arm alone, with
// a full page of due rows whose every locked operation takes per, the pass (1) stops starting that
// arm's rows once its budget is spent, so it starts fewer than the page (the per-row pass.spent
// check in the arm), and (2) runs every operation it starts under the sweeper's per-record timeout,
// not the publish path's retentionOpTimeout (pass.timeout()). Removing either from any one arm
// fails that arm's subtest (in the work arm, either of its two operations).
func TestRetentionPassArmsCheckBudgetPerRowAndUseSweepTimeout(t *testing.T) {
	const (
		rows      = 6
		per       = 60 * time.Millisecond
		budget    = 150 * time.Millisecond // three operations fit: rows 0-2 start, row 3 does not
		opTimeout = 7 * time.Second        // far below retentionOpTimeout, and above per
	)
	for _, arm := range []string{"attempts", "work", "unheld", "audit"} {
		t.Run(arm, func(t *testing.T) {
			st := &timedArmStore{fakeStore: &fakeStore{}, arm: arm, rows: rows}
			svc := New(st, newBox(t), testParams())
			acq := &slowAcquirer{per: per}
			svc.SetRetentionLockPool(acq)
			svc.SetForgeBaseURLAllowed(func(string) bool { return true })
			svc.SetBackground(func(fn func()) { fn() })
			unreached := errors.New("forge seam reached without a lock")
			svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return unreached })
			svc.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { return unreached })
			svc.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
				return nil, unreached
			})
			svc.retentionPassBudget = budget
			svc.retentionSweepOpTimeout = opTimeout

			if _, err := svc.ReconcileCheckpointRetentions(context.Background()); err != nil {
				t.Fatalf("ReconcileCheckpointRetentions: %v", err)
			}
			left := acq.acquired()
			if len(left) == 0 || len(left) >= rows {
				t.Fatalf("the %s arm started %d of its %d rows, want at least 1 and fewer than all "+
					"(the spent budget must stop it between rows)", arm, len(left), rows)
			}
			for i, l := range left {
				if l <= 0 || l > opTimeout {
					t.Fatalf("the %s arm's operation %d ran with %s left on its context, want at most the sweeper's "+
						"per-record timeout %s (not retentionOpTimeout %s)", arm, i, l, opTimeout, retentionOpTimeout)
				}
			}
		})
	}
}

// TestRetentionPassWorstCaseBelowSweeperDanger pins the per-tick worst case the retentionPass doc
// states (#1810 rework review N1): the pass budget, one full per-record timeout, one shared
// bookkeeping window past that deadline, the unlock and the connection teardown must stay clearly
// below the sweeper health beat's danger line (ten 15s default intervals, internal/healthsvc and
// internal/sweeper's defaultInterval). Raising retentionSweepOpTimeout back to the 5 x 30s
// derivation (160s) fails it.
func TestRetentionPassWorstCaseBelowSweeperDanger(t *testing.T) {
	const (
		sweeperDefaultInterval = 15 * time.Second
		dangerLine             = 10 * sweeperDefaultInterval
		margin                 = 20 * time.Second
	)
	worst := retentionPassBudget + retentionSweepOpTimeout + retentionRecordTimeout + 2*retentionUnlockTimeout
	if worst != 120*time.Second {
		t.Errorf("worst case per tick = %s, want the documented 120s (update the retentionPass doc, the PRD and the ADR with it)", worst)
	}
	if worst > dangerLine-margin {
		t.Fatalf("worst case per tick = %s, want at most %s (the danger line %s less %s)", worst, dangerLine-margin, dangerLine, margin)
	}
	svc := New(nil, nil, testParams())
	if svc.retentionSweepOpTimeout != retentionSweepOpTimeout || svc.retentionPassBudget != retentionPassBudget {
		t.Fatalf("New wired op timeout %s / budget %s, want the defaults %s / %s", svc.retentionSweepOpTimeout,
			svc.retentionPassBudget, retentionSweepOpTimeout, retentionPassBudget)
	}
}

// TestRetentionBookkeepingCtxSharesOneWindow (#1810 rework review N2): a bookkeeping context
// derived from another shares its deadline (deferExpiredRetentionStep hands its context to
// recordRetentionFailure), and every bookkeeping context derived from a locked operation ends
// within one retentionRecordTimeout past the operation's deadline, however late it is created. A
// context unrelated to either gets a fresh window.
func TestRetentionBookkeepingCtxSharesOneWindow(t *testing.T) {
	const slack = time.Second

	outer, cancelOuter := retentionBookkeepingCtx(context.Background())
	defer cancelOuter()
	d1, _ := outer.Deadline()
	time.Sleep(20 * time.Millisecond)
	nested, cancelNested := retentionBookkeepingCtx(outer)
	defer cancelNested()
	if d2, _ := nested.Deadline(); !d2.Equal(d1) {
		t.Fatalf("nested bookkeeping deadline = %v, want its caller's %v (one shared window)", d2, d1)
	}

	// A locked operation whose deadline passed 4s ago: its bookkeeping ends 6s from now, not 10s.
	opDeadline := time.Now().Add(-4 * time.Second)
	opCtx, cancelOp := context.WithDeadline(context.Background(), opDeadline)
	defer cancelOp()
	opCtx = context.WithValue(context.WithValue(opCtx, retentionOpDeadlineKey{}, opDeadline), retentionBookkeepingKey{}, nil)
	late, cancelLate := retentionBookkeepingCtx(opCtx)
	defer cancelLate()
	if d, _ := late.Deadline(); !d.Equal(opDeadline.Add(retentionRecordTimeout)) {
		t.Fatalf("late bookkeeping deadline = %v, want the operation's deadline + %s = %v", d, retentionRecordTimeout,
			opDeadline.Add(retentionRecordTimeout))
	}
	if late.Err() != nil {
		t.Fatalf("late bookkeeping context is done (%v); want it detached from the expired operation", late.Err())
	}

	// A context with no operation or bookkeeping mark: a fresh window.
	fresh, cancelFresh := retentionBookkeepingCtx(opCtxWithoutMarks(t))
	defer cancelFresh()
	if d, _ := fresh.Deadline(); time.Until(d) < retentionRecordTimeout-slack {
		t.Fatalf("fresh bookkeeping deadline in %s, want about %s", time.Until(d), retentionRecordTimeout)
	}
}

// opCtxWithoutMarks is an already-expired context that carries no retention marks.
func opCtxWithoutMarks(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	t.Cleanup(cancel)
	return ctx
}
