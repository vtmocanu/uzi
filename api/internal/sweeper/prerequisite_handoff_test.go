package sweeper

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Unused persistence methods panic through the nil embedded interfaces. The two
// stores are separate so their shared method names cannot mask unexpected calls.
type prerequisiteHandoffLiveness struct {
	workersvc.Store
	t         *testing.T
	parent    context.Context
	stale     time.Duration
	timeout   time.Duration
	handoff   time.Time
	markCalls int
	wallCalls int
	stopErr   error
}

func (q *prerequisiteHandoffLiveness) MarkStaleWorkersOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	q.markCalls++
	q.handoff = time.Now()
	if ctx != q.parent {
		q.t.Error("run-liveness sweep did not receive the original engine context")
	}
	if !cutoff.Valid || !cutoff.Time.Equal(q.handoff.Add(-q.stale)) {
		q.t.Errorf("stale cutoff = %v, handoff = %v, stale = %v", cutoff, q.handoff, q.stale)
	}
	// The canceled-parent case observes invocation only. A real database would
	// reject this context; these empty results let us also inspect wall arguments.
	return 0, nil
}

func (q *prerequisiteHandoffLiveness) SweepClaimedNeverStarted(context.Context, pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error) {
	return nil, nil
}

func (q *prerequisiteHandoffLiveness) SweepTaskNeverDispatched(context.Context, store.SweepTaskNeverDispatchedParams) ([]store.SweepTaskNeverDispatchedRow, error) {
	return nil, nil
}

func (q *prerequisiteHandoffLiveness) RequestWallParks(ctx context.Context, arg store.RequestWallParksParams) ([]store.RequestWallParksRow, error) {
	q.wallCalls++
	if ctx != q.parent {
		q.t.Error("wall request did not receive the original engine context")
	}
	if !arg.Now.Valid || !arg.Now.Time.Equal(q.handoff) ||
		!arg.WorkerStaleCutoff.Valid || !arg.WorkerStaleCutoff.Time.Equal(q.handoff.Add(-q.stale)) ||
		arg.GlobalTimeoutSeconds != int32(q.timeout.Seconds()) {
		q.t.Errorf("wall arguments = %+v, handoff = %v, stale = %v, timeout = %v",
			arg, q.handoff, q.stale, q.timeout)
	}
	return nil, q.stopErr // Deliberately stop the real Sweep before unrelated work.
}

type prerequisiteHandoffStore struct {
	workersvc.PrerequisiteStore
	t              *testing.T
	rows           []store.RecoveryPrerequisiteClosure
	selected       []uuid.UUID
	captureLocks   []uuid.UUID
	closureLocks   []uuid.UUID
	completed      []uuid.UUID
	finished       []time.Time
	outcomeErr     error
	returnedErrors int
	cancelThird    context.CancelFunc
	inTransaction  bool
}

func (q *prerequisiteHandoffStore) ListPrerequisitePendingDue(context.Context, store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}

func (q *prerequisiteHandoffStore) ListPrerequisiteStaleInvokedDue(context.Context, store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}

func (q *prerequisiteHandoffStore) ListPrerequisiteReconcileDue(ctx context.Context, arg store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(arg.Lim) < int64(len(q.rows)) {
		q.t.Fatalf("selector limit %d cannot supply four discriminating rows", arg.Lim)
	}
	return q.rows, nil
}

func (q *prerequisiteHandoffStore) ListPrerequisiteAuditDue(context.Context, store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}

func (q *prerequisiteHandoffStore) ListPrerequisiteCleanupDue(context.Context, store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	return nil, nil
}

func (q *prerequisiteHandoffStore) GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if err := ctx.Err(); err != nil {
		return store.GetRepoForUserRow{}, err
	}
	index := len(q.selected)
	if index >= len(q.rows) || arg.ID != q.rows[index].RepoID || arg.UserID != q.rows[index].UserID {
		q.t.Fatalf("unexpected binding read: %+v at index %d", arg, index)
	}
	q.selected = append(q.selected, q.rows[index].ID)
	return store.GetRepoForUserRow{}, errors.New("controlled binding unavailable; no network")
}

func (q *prerequisiteHandoffStore) transaction(ctx context.Context, fn func(workersvc.PrerequisiteStore) error) error {
	if q.inTransaction {
		q.t.Fatal("nested prerequisite transaction")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	q.inTransaction = true
	defer func() { q.inTransaction = false }()
	return fn(q)
}

func (q *prerequisiteHandoffStore) LockPrerequisiteCapture(ctx context.Context, id uuid.UUID) (store.RecoveryCapture, error) {
	if err := ctx.Err(); err != nil {
		return store.RecoveryCapture{}, err
	}
	index := len(q.captureLocks)
	if !q.inTransaction || index >= len(q.rows) || id != q.rows[index].CaptureID ||
		len(q.closureLocks) != index {
		q.t.Fatalf("unexpected capture lock %v at index %d", id, index)
	}
	q.captureLocks = append(q.captureLocks, id)
	return store.RecoveryCapture{ID: id}, nil
}

func (q *prerequisiteHandoffStore) LockPrerequisiteClosure(ctx context.Context, id uuid.UUID) (store.RecoveryPrerequisiteClosure, error) {
	if err := ctx.Err(); err != nil {
		return store.RecoveryPrerequisiteClosure{}, err
	}
	index := len(q.closureLocks)
	if !q.inTransaction || index >= len(q.rows) || id != q.rows[index].ID ||
		len(q.captureLocks) != index+1 {
		q.t.Fatalf("unexpected closure lock %v at index %d", id, index)
	}
	q.closureLocks = append(q.closureLocks, id)
	return q.rows[index], nil
}

func (q *prerequisiteHandoffStore) DeferPrerequisiteReconciliation(ctx context.Context, arg store.DeferPrerequisiteReconciliationParams) (int64, error) {
	index := len(q.completed)
	if !q.inTransaction || index >= len(q.rows) || arg.ID != q.rows[index].ID ||
		arg.Generation != q.rows[index].Attempts || arg.ReconciliationGeneration != q.rows[index].ReconciliationAttempts ||
		len(q.closureLocks) != index+1 {
		q.t.Fatalf("unexpected reconciliation: %+v at index %d", arg, index)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
		q.t.Errorf("outcome deadline = %v, present = %v", deadline, ok)
	}
	if index == 2 && q.cancelThird != nil {
		q.cancelThird()
	}
	if err := ctx.Err(); err != nil {
		q.t.Errorf("detached outcome already canceled: %v", err)
		return 0, err
	}
	// Four candidates make a widened admission budget observable. The first two
	// outcomes cost 3.2s each, and the third costs 4.999s, honoring real contexts.
	cost := 4999 * time.Millisecond
	if index < 2 {
		cost = 3200 * time.Millisecond
	}
	timer := time.NewTimer(cost)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		q.t.Errorf("detached outcome interrupted before durable tail: %v", ctx.Err())
		return 0, ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		q.t.Errorf("outcome context no longer live: %v", err)
		return 0, err
	}
	q.completed = append(q.completed, arg.ID)
	q.finished = append(q.finished, time.Now())
	if q.outcomeErr != nil {
		q.returnedErrors++
		return 0, q.outcomeErr
	}
	return 1, nil
}

// All elapsed times here are virtual synctest time through production dispatch
// and deadline code, not measurements of database or production pin latency.
func TestPrerequisiteClosureRealServiceHandoff(t *testing.T) {
	for _, config := range []struct {
		name    string
		stale   time.Duration
		timeout time.Duration
	}{
		{"normal", 45 * time.Second, 6 * time.Hour},
		{"short_stale", time.Second, 6 * time.Hour},
		{"short_timeout", 45 * time.Second, time.Second},
		{"both_short", time.Second, time.Second},
	} {
		for _, mode := range []string{"success", "outcome_error", "canceled_parent"} {
			t.Run(config.name+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					parent, cancel := context.WithCancel(context.Background())
					defer cancel()
					liveness := &prerequisiteHandoffLiveness{
						t: t, parent: parent, stale: config.stale, timeout: config.timeout,
						stopErr: errors.New("controlled stop after wall request"),
					}
					q := &prerequisiteHandoffStore{t: t}
					for i := 0; i < 4; i++ {
						q.rows = append(q.rows, store.RecoveryPrerequisiteClosure{
							ID: uuid.New(), CaptureID: uuid.New(), RepoID: uuid.New(), UserID: uuid.New(),
							State: "create_unknown", Attempts: 1, ReconciliationAttempts: 2,
							NextAttemptAt: pgtype.Timestamptz{Time: start.Add(time.Duration(i-4) * time.Second), Valid: true},
						})
					}
					if mode == "outcome_error" {
						q.outcomeErr = errors.New("controlled durable outcome failure")
					}
					if mode == "canceled_parent" {
						q.cancelThird = cancel
					}
					svc := workersvc.New(liveness, nil, workersvc.Params{
						WorkerHeartbeatStale: config.stale, RunTimeout: config.timeout,
						ClaimGrace: 30 * time.Second, DispatchGrace: time.Minute,
						SweeperBootGrace: 0,
					})
					svc.SetClock(time.Now)
					svc.SetPrerequisiteStore(q, q.transaction)
					engine := New(svc, time.Second,
						Pass{Name: "prior_cost", Run: func(context.Context) (int64, error) {
							timer := time.NewTimer(2 * time.Second)
							<-timer.C
							return 0, nil
						}},
						Pass{Name: "prerequisite_closure", Run: svc.SweepPrerequisiteClosures},
					)
					engine.Boot(parent)
					wantIDs := []uuid.UUID{q.rows[0].ID, q.rows[1].ID, q.rows[2].ID}
					if !slices.Equal(q.selected, wantIDs) || !slices.Equal(q.completed, wantIDs) ||
						!slices.Equal(q.closureLocks, wantIDs) {
						t.Fatalf("selected = %v, completed = %v, closure locks = %v; want first three %v",
							q.selected, q.completed, q.closureLocks, wantIDs)
					}
					wantCaptures := []uuid.UUID{q.rows[0].CaptureID, q.rows[1].CaptureID, q.rows[2].CaptureID}
					if !slices.Equal(q.captureLocks, wantCaptures) {
						t.Fatalf("capture locks = %v, want %v", q.captureLocks, wantCaptures)
					}
					for i, want := range []time.Duration{3200 * time.Millisecond, 6400 * time.Millisecond, 11399 * time.Millisecond} {
						if got := q.finished[i].Sub(start.Add(2 * time.Second)); got != want {
							t.Errorf("outcome %d finished at %v after prior pass, want %v", i, got, want)
						}
					}
					if liveness.markCalls != 1 || liveness.wallCalls != 1 {
						t.Fatalf("liveness calls: mark = %d, wall = %d", liveness.markCalls, liveness.wallCalls)
					}
					afterPrior := liveness.handoff.Sub(start.Add(2 * time.Second))
					if afterPrior != 11399*time.Millisecond || afterPrior > 15*time.Second {
						t.Errorf("run-liveness handoff after prior pass = %v, want 11.399s and <=15s", afterPrior)
					}
					if mode == "outcome_error" && q.returnedErrors != 3 {
						t.Errorf("controlled outcome errors returned = %d, want 3", q.returnedErrors)
					}
					if (parent.Err() != nil) != (mode == "canceled_parent") {
						t.Errorf("original parent error = %v in mode %s", parent.Err(), mode)
					}
				})
			})
		}
	}
}
