package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type openHoldsFakeStore struct {
	recovery.Store
	row      store.ListCustodyHoldsForWorkerRunRow
	countErr error
	calls    []store.CountOpenHoldsForOwnedRunParams
}

func (s *openHoldsFakeStore) ListCustodyHoldsForWorkerRun(context.Context, store.ListCustodyHoldsForWorkerRunParams) ([]store.ListCustodyHoldsForWorkerRunRow, error) {
	return []store.ListCustodyHoldsForWorkerRunRow{s.row}, nil
}

func (s *openHoldsFakeStore) CountOpenHoldsForOwnedRun(_ context.Context, arg store.CountOpenHoldsForOwnedRunParams) (store.CountOpenHoldsForOwnedRunRow, error) {
	s.calls = append(s.calls, arg)
	if s.countErr != nil {
		return store.CountOpenHoldsForOwnedRunRow{}, s.countErr
	}
	return store.CountOpenHoldsForOwnedRunRow{RunID: arg.RunID, OpenHolds: 4}, nil
}

func TestWorkerListRecoveryHoldsOpenHoldCount(t *testing.T) {
	run := uuid.New()
	get := func(st *openHoldsFakeStore, caps []string, query string) (int, map[string]any) {
		t.Helper()
		h := &Handler{}
		h.recoverySvc = recovery.New(st, nil, nil, recovery.Limits{}, nil)
		rec := exactHoldRoute(h, store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: caps}, run, query)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		return rec.Code, body
	}
	newStore := func() *openHoldsFakeStore {
		return &openHoldsFakeStore{row: store.ListCustodyHoldsForWorkerRunRow{ID: uuid.New(), Generation: 1}}
	}

	t.Run("capability off", func(t *testing.T) {
		st := newStore()
		code, body := get(st, []string{capability.RecoveryInventoryV1}, "")
		if _, present := body["open_holds_all_workers"]; code != 200 || present || len(st.calls) != 0 {
			t.Fatalf("code=%d body=%v count calls=%d", code, body, len(st.calls))
		}
	})
	t.Run("capability on", func(t *testing.T) {
		st := newStore()
		code, body := get(st, []string{capability.RecoveryRunOpenHoldsV1}, "")
		got, _ := body["open_holds_all_workers"].(map[string]any)
		if code != 200 || got["run_id"] != run.String() || got["count"] != float64(4) || len(body["holds"].([]any)) != 1 {
			t.Fatalf("code=%d body=%v", code, body)
		}
		if len(st.calls) != 1 || st.calls[0].RunID != run {
			t.Fatalf("count calls = %+v", st.calls)
		}
	})
	for name, err := range map[string]error{"no row": pgx.ErrNoRows, "read error": errors.New("db down")} {
		t.Run(name, func(t *testing.T) {
			st := newStore()
			st.countErr = err
			code, body := get(st, []string{capability.RecoveryRunOpenHoldsV1}, "")
			if _, present := body["open_holds_all_workers"]; code != 200 || present || len(body["holds"].([]any)) != 1 {
				t.Fatalf("code=%d body=%v", code, body)
			}
		})
	}
	t.Run("exact generation branch never counts", func(t *testing.T) {
		st := newStore()
		code, body := get(st, []string{capability.RecoveryRunOpenHoldsV1, capability.RecoveryBindinglessClosedV1}, "?generation=0")
		if _, present := body["open_holds_all_workers"]; code != 200 || present || len(st.calls) != 0 {
			t.Fatalf("code=%d body=%v count calls=%d", code, body, len(st.calls))
		}
	})
}
