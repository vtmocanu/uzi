package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type slowBlockerReadStore struct {
	*fakeStore
	delay time.Duration
}

func (s *slowBlockerReadStore) ListWorkerUpgradeBlockers(ctx context.Context, _ []uuid.UUID) ([]store.ListWorkerUpgradeBlockersRow, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("diagnostic read unavailable")
	}
}

func TestCapacityBlockerReadPreservesConfirmationBudget(t *testing.T) {
	for _, delay := range []time.Duration{0, fleetCapacityConfirmBudget + 100*time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			owner, worker := uuid.New(), uuid.New()
			since := pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
			reason := pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}
			f := &fakeStore{
				controller:   pgtype.Timestamptz{Time: fixedNow, Valid: true},
				workers:      []store.ListAllWorkersRow{{Worker: store.Worker{ID: worker, UserID: owner, Kind: "hosted", DrainingSince: since}}},
				capacityRows: []store.ListOwnersWaitingNoCapacityRow{{RunID: uuid.New(), UserID: owner, HealthReason: reason, HealthSince: since}},
				// A distinct run requires a fresh queue confirmation, rather than a memoized capacity result.
				waitingRows: []store.ListWaitingWorkerRunsRow{{RunID: uuid.New(), UserID: owner, HealthReason: reason, HealthSince: since}},
			}
			s := newSvc(f, &fakeSettings{healthEnabled: true})
			s.cfg.Store = &slowBlockerReadStore{fakeStore: f, delay: delay}
			calls := 0
			s.cfg.WorkerEligibilityForHealth = func(context.Context, time.Time, uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
				calls++
				return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: since}, nil
			}
			doc, err := s.Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"fleet.capacity", "queue.waiting"} {
				severity, summary := check(t, doc, id)
				if severity != sevOK {
					t.Fatalf("diagnostic delay %s changed %s severity to %s: %s", delay, id, severity, summary)
				}
			}
			if calls != 2 {
				t.Fatalf("diagnostic delay consumed confirmation budget: eligibility calls=%d, want 2", calls)
			}
			for _, c := range doc.Checks {
				if c.ID == "fleet.capacity" && (len(c.Evidence) == 0 || !strings.Contains(c.Evidence[0].Value, "unavailable")) {
					t.Fatalf("diagnostic failure was not reported: %+v", c)
				}
			}
		})
	}
}
