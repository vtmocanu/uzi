package recovery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type workerDecisionsStore struct {
	Store
	rows  []store.ListOpenCustodyHoldsForWorkersRow
	err   error
	calls int
	ids   []uuid.UUID
}

func (s *workerDecisionsStore) ListOpenCustodyHoldsForWorkers(_ context.Context, ids []uuid.UUID) ([]store.ListOpenCustodyHoldsForWorkersRow, error) {
	s.calls++
	s.ids = ids
	return s.rows, s.err
}
func TestCustodyDecisionsByWorker(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	st := &workerDecisionsStore{}
	svc := New(st, nil, nil, Limits{}, nil)
	counts, err := svc.CustodyDecisionsByWorker(context.Background(), nil)
	if err != nil || len(counts) != 0 || st.calls != 0 {
		t.Fatalf("empty batch = %v, %v; reads %d", counts, err, st.calls)
	}
	// DecisionNeeded is authoritative even when attention carries a different label.
	for _, decision := range []bool{false, false, false, true, true, true, true, false, false, false, false, false} {
		st.rows = append(st.rows, store.ListOpenCustodyHoldsForWorkersRow{WorkerID: a, State: "open", RunStatus: "recovery_wait", RecoveryWaitCause: "worker_requeue_exhausted", CaptureState: "available", HasAvailableCapture: true, Attention: "active", DecisionNeeded: decision})
	}
	counts, err = svc.CustodyDecisionsByWorker(context.Background(), []uuid.UUID{a, b})
	if err != nil || !reflect.DeepEqual(counts, map[uuid.UUID]int{a: 4, b: 0}) || st.calls != 1 || !reflect.DeepEqual(st.ids, []uuid.UUID{a, b}) {
		t.Fatalf("batch = %v, %v; calls=%d ids=%v", counts, err, st.calls, st.ids)
	}
	st.err = errors.New("partial read failed")
	counts, err = svc.CustodyDecisionsByWorker(context.Background(), []uuid.UUID{a, b})
	if !errors.Is(err, st.err) || counts != nil {
		t.Fatalf("partial result leaked: %v, %v", counts, err)
	}
}

type ownerCustodyStore struct {
	Store
	rows                  []store.ListCustodyHoldsForOwnerRow
	listParams            store.ListCustodyHoldsForOwnerParams
	aggregateParams       store.GetCustodyAggregateForOwnerParams
	listErr, aggregateErr error
}

func (s *ownerCustodyStore) ListCustodyHoldsForOwner(_ context.Context, arg store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
	s.listParams = arg
	return s.rows, s.listErr
}
func (s *ownerCustodyStore) GetCustodyAggregateForOwner(_ context.Context, arg store.GetCustodyAggregateForOwnerParams) (store.GetCustodyAggregateForOwnerRow, error) {
	s.aggregateParams = arg
	return store.GetCustodyAggregateForOwnerRow{OpenHolds: 11, AdmissionCountedHolds: 8, BlockedRuns: 2}, s.aggregateErr
}

func TestOwnerCustodyConfiguredCutoffAndSQLFields(t *testing.T) {
	owner := uuid.New()
	for _, openOnly := range []bool{false, true} {
		st := &ownerCustodyStore{rows: []store.ListCustodyHoldsForOwnerRow{
			{Attention: "active", DecisionNeeded: true},
			{Attention: "needs_action", DecisionNeeded: false},
		}}
		calls := 0
		svc := New(st, nil, nil, Limits{CustodyHoldLimit: 8, WorkerHeartbeatStale: 73 * time.Second}, func() time.Time {
			calls++
			return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(time.Duration(calls-1) * time.Hour)
		})
		got, err := svc.ListHoldsForOwner(context.Background(), owner, openOnly)
		want := apitypes.RecoveryCustodyAggregateDTO{OpenHolds: 11, AdmissionCountedHolds: 8, CustodyHoldLimit: 8, DecisionNeeded: 1, BlockedRuns: 2}
		if err != nil || got.Aggregate != want || calls != 1 {
			t.Fatalf("got=%+v err=%v clock calls=%d", got, err, calls)
		}
		arg := st.aggregateParams
		if arg.UserID != owner || arg.CustodyHoldLimit != 8 || !arg.HeartbeatCutoff.Valid || !arg.HeartbeatCutoff.Time.Equal(time.Date(2026, 10, 7, 11, 58, 47, 0, time.UTC)) {
			t.Fatalf("aggregate params=%+v", arg)
		}
		if st.listParams.UserID != owner || st.listParams.State.Valid != openOnly || (openOnly && st.listParams.State.String != "open") {
			t.Fatalf("list params=%+v", st.listParams)
		}
		if len(got.Holds) != 2 || got.Holds[0].Attention != "active" || got.Holds[1].Attention != "needs_action" {
			t.Fatalf("SQL attention lost: %+v", got.Holds)
		}
		st.aggregateErr = errors.New("aggregate failed")
		got, err = svc.ListHoldsForOwner(context.Background(), owner, openOnly)
		if !errors.Is(err, st.aggregateErr) || got.Holds != nil || got.Aggregate != (apitypes.RecoveryCustodyAggregateDTO{}) {
			t.Fatalf("partial aggregate leaked: %+v %v", got, err)
		}
		st.listErr = errors.New("list failed")
		_, err = svc.ListHoldsForOwner(context.Background(), owner, openOnly)
		if !errors.Is(err, st.listErr) {
			t.Fatalf("list error=%v", err)
		}
	}
}
