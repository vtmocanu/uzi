package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type openHoldsStore struct {
	Store
	list     []store.ListCustodyHoldsForWorkerRunRow
	count    int64
	countErr error
	calls    []store.CountOpenHoldsForOwnedRunParams
}

func (s *openHoldsStore) ListCustodyHoldsForWorkerRun(context.Context, store.ListCustodyHoldsForWorkerRunParams) ([]store.ListCustodyHoldsForWorkerRunRow, error) {
	return s.list, nil
}

func (s *openHoldsStore) CountOpenHoldsForOwnedRun(_ context.Context, arg store.CountOpenHoldsForOwnedRunParams) (store.CountOpenHoldsForOwnedRunRow, error) {
	s.calls = append(s.calls, arg)
	if s.countErr != nil {
		return store.CountOpenHoldsForOwnedRunRow{}, s.countErr
	}
	return store.CountOpenHoldsForOwnedRunRow{RunID: arg.RunID, OpenHolds: s.count}, nil
}

func TestListHoldsForWorkerRunOpenHoldCount(t *testing.T) {
	run := uuid.New()
	hold := store.ListCustodyHoldsForWorkerRunRow{ID: uuid.New(), Generation: 2}
	wkr := func(caps ...string) store.Worker {
		return store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: caps}
	}
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	baseline := func() string {
		st := &openHoldsStore{list: []store.ListCustodyHoldsForWorkerRunRow{hold}}
		res, err := New(st, nil, nil, Limits{}, nil).ListHoldsForWorkerRun(context.Background(), wkr(), run)
		if err != nil {
			t.Fatal(err)
		}
		return marshal(res)
	}()

	t.Run("capability off omits the field and never queries", func(t *testing.T) {
		st := &openHoldsStore{list: []store.ListCustodyHoldsForWorkerRunRow{hold}, count: 9}
		res, err := New(st, nil, nil, Limits{}, nil).ListHoldsForWorkerRun(context.Background(), wkr(capability.RecoveryBindinglessClosedV1), run)
		if err != nil || res.RunOpenHolds != nil || len(st.calls) != 0 {
			t.Fatalf("res=%+v err=%v count calls=%d", res, err, len(st.calls))
		}
		if got := marshal(res); got != baseline {
			t.Fatalf("worker-scoped body changed: %s vs %s", got, baseline)
		}
	})

	t.Run("capability on carries a run-bound count and the list is unchanged", func(t *testing.T) {
		st := &openHoldsStore{list: []store.ListCustodyHoldsForWorkerRunRow{hold}, count: 5}
		w := wkr(capability.RecoveryRunOpenHoldsV1)
		res, err := New(st, nil, nil, Limits{}, nil).ListHoldsForWorkerRun(context.Background(), w, run)
		if err != nil {
			t.Fatal(err)
		}
		if res.RunOpenHolds == nil || res.RunOpenHolds.RunID != run.String() || res.RunOpenHolds.Count != 5 {
			t.Fatalf("count = %+v, want run %s count 5", res.RunOpenHolds, run)
		}
		if len(res.Holds) != 1 || res.Holds[0].HoldID != hold.ID.String() {
			t.Fatalf("holds = %+v", res.Holds)
		}
		if len(st.calls) != 1 || st.calls[0].RunID != run || st.calls[0].UserID != w.UserID {
			t.Fatalf("count params = %+v, want run %s user %s", st.calls, run, w.UserID)
		}
		want := `"run_open_holds":{"run_id":"` + run.String() + `","count":5}`
		if got := marshal(res); got != baseline[:len(baseline)-1]+","+want+"}" {
			t.Fatalf("body = %s", got)
		}
	})

	t.Run("a real zero is emitted", func(t *testing.T) {
		st := &openHoldsStore{count: 0}
		res, err := New(st, nil, nil, Limits{}, nil).ListHoldsForWorkerRun(context.Background(), wkr(capability.RecoveryRunOpenHoldsV1), run)
		if err != nil || res.RunOpenHolds == nil || res.RunOpenHolds.Count != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	for name, countErr := range map[string]error{"no row": pgx.ErrNoRows, "read error": errors.New("db down")} {
		t.Run(name+" omits the field and keeps the list", func(t *testing.T) {
			st := &openHoldsStore{list: []store.ListCustodyHoldsForWorkerRunRow{hold}, countErr: countErr}
			res, err := New(st, nil, nil, Limits{}, nil).ListHoldsForWorkerRun(context.Background(), wkr(capability.RecoveryRunOpenHoldsV1), run)
			if err != nil || res.RunOpenHolds != nil || len(res.Holds) != 1 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if got := marshal(res); got != baseline {
				t.Fatalf("body = %s, want %s", got, baseline)
			}
		})
	}
}
