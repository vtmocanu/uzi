package recovery

import (
	"context"
	"errors"
	"reflect"
	"testing"

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
	for _, input := range []holdAttentionInput{
		{State: "open", RunStatus: "running"},
		{State: "open", RunStatus: "queued"},
		{State: "open", RunStatus: "paused"},
		{State: "open", CaptureState: "needs_action", RunStatus: "running"},
		{State: "open", CaptureState: "needs_action", RunStatus: "failed"},
		{State: "open", RunStatus: "failed"},
		{State: "open"},
		{State: "open", HasAvailableCapture: true, CaptureState: "needs_action"},
		{State: "open", CaptureState: "preparing", RunStatus: "failed"},
		{State: "open", CaptureState: "uploading", RunStatus: "running"},
		{State: "released", CaptureState: "needs_action"},
		{State: "discarded"},
	} {
		st.rows = append(st.rows, store.ListOpenCustodyHoldsForWorkersRow{WorkerID: a, State: input.State, HasAvailableCapture: input.HasAvailableCapture, CaptureState: input.CaptureState, RunStatus: input.RunStatus})
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
