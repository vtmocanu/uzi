package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type pendingOutcomeCancelRaceStore struct {
	Store
	run            store.Run
	worker         store.Worker
	cancelRows     int64
	cancelToStatus string
	ownerReadCount int
}

func (s *pendingOutcomeCancelRaceStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID != s.run.ID || arg.UserID != s.run.UserID {
		return store.Run{}, pgx.ErrNoRows
	}
	s.ownerReadCount++
	return s.run, nil
}

func (s *pendingOutcomeCancelRaceStore) GetWorkerByID(_ context.Context, id uuid.UUID) (store.Worker, error) {
	if id != s.worker.ID {
		return store.Worker{}, pgx.ErrNoRows
	}
	return s.worker, nil
}

func (s *pendingOutcomeCancelRaceStore) RunHasPendingOutcomeLease(_ context.Context, id uuid.UUID) (bool, error) {
	return id == s.run.ID, nil
}

func (s *pendingOutcomeCancelRaceStore) CancelRunServerSideWithPendingOutcome(_ context.Context, arg store.CancelRunServerSideWithPendingOutcomeParams) (int64, error) {
	if arg.ID != s.run.ID || arg.UserID != s.run.UserID {
		return 0, nil
	}
	if s.cancelToStatus != "" {
		s.run.Status = s.cancelToStatus
	}
	return s.cancelRows, nil
}

func newPendingOutcomeCancelRaceService(st *pendingOutcomeCancelRaceStore) *Service {
	return New(st, nil, Params{WorkerHeartbeatStale: time.Hour})
}

func newPendingOutcomeCancelRaceStore() *pendingOutcomeCancelRaceStore {
	ownerID := uuid.New()
	workerID := uuid.New()
	return &pendingOutcomeCancelRaceStore{
		run: store.Run{
			ID:       uuid.New(),
			UserID:   ownerID,
			Kind:     runkind.Issue,
			Status:   "running",
			WorkerID: pgtype.UUID{Bytes: workerID, Valid: true},
		},
		worker: store.Worker{
			ID:              workerID,
			LastHeartbeatAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		},
	}
}

func TestSubmitInputPendingOutcomeCancelZeroRows(t *testing.T) {
	t.Run("active run reports a race conflict", func(t *testing.T) {
		st := newPendingOutcomeCancelRaceStore()
		svc := newPendingOutcomeCancelRaceService(st)

		_, err := svc.SubmitInputWithOptions(context.Background(), st.run.UserID, st.run.ID, "cancel", "discard", nil, SubmitInputOptions{DiscardPendingOutcome: true})
		if !errors.Is(err, ErrOutcomePendingCancelRaced) {
			t.Fatalf("err = %v, want ErrOutcomePendingCancelRaced", err)
		}
		if st.run.Status != "running" {
			t.Fatalf("status = %q, want running", st.run.Status)
		}
		if st.ownerReadCount != 2 {
			t.Fatalf("owner-scoped run reads = %d, want 2 (initial read plus zero-row re-read)", st.ownerReadCount)
		}
	})

	t.Run("terminal winner remains a successful no-op", func(t *testing.T) {
		st := newPendingOutcomeCancelRaceStore()
		st.cancelToStatus = "completed"
		svc := newPendingOutcomeCancelRaceService(st)

		res, err := svc.SubmitInputWithOptions(context.Background(), st.run.UserID, st.run.ID, "cancel", "discard", nil, SubmitInputOptions{DiscardPendingOutcome: true})
		if err != nil {
			t.Fatalf("terminal winner err = %v, want nil", err)
		}
		if !res.ServerSide {
			t.Fatal("terminal winner no-op must report server-side completion")
		}
		if st.run.Status != "completed" {
			t.Fatalf("status = %q, want completed", st.run.Status)
		}
		if st.ownerReadCount != 2 {
			t.Fatalf("owner-scoped run reads = %d, want 2 (initial read plus zero-row re-read)", st.ownerReadCount)
		}
	})
}
