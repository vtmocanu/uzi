package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestHealthPlanCrossCheckWaitExcludedLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() {
		mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID)
	})
	now := time.Now().UTC().Truncate(time.Second)
	oldestWaiting := func() (pgtype.Timestamptz, error) {
		rows, err := fx.q.ListWaitingWorkerRuns(fx.ctx)
		var oldest pgtype.Timestamptz
		for _, row := range rows {
			if row.HealthSince.Valid && (!oldest.Valid || row.HealthSince.Time.Before(oldest.Time)) {
				oldest = row.HealthSince
			}
		}
		return oldest, err
	}
	baseline, err := oldestWaiting()
	if err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	if baseline.Valid && !since.Before(baseline.Time) {
		since = baseline.Time.Add(-time.Hour)
	}

	// The lead is waiting for its own plan check, not worker admission. Its
	// existing worker may drain while that owned work finishes.
	worker := fx.worker("plan-check-draining", capOf(1), false)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET draining_since=$2 WHERE id=$1", worker, now)
	seed := func(reason any, at time.Time, owned bool) uuid.UUID {
		t.Helper()
		run := queuedRunWithCaps(fx, []string{})
		t.Cleanup(func() {
			mustExec(fx.ctx, t, fx.pool, "DELETE FROM runs WHERE id=$1 AND user_id=$2", run, fx.userID)
		})
		mustExec(fx.ctx, t, fx.pool,
			"UPDATE runs SET health='waiting_worker',health_reason=$2,health_since=$3 WHERE id=$1",
			run, reason, at)
		if owned {
			mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='running',worker_id=$2,plan_cross_check_required=true,auto_approve=true,claim_generation=1,started_at=$3 WHERE id=$1", run, worker, now)
		}
		return run
	}
	expected := seed("waiting for plan cross-check", since, true)
	assertOldest := func(t *testing.T, want pgtype.Timestamptz) {
		t.Helper()
		got, err := oldestWaiting()
		if err != nil {
			t.Fatal(err)
		}
		if got.Valid != want.Valid || (got.Valid && !got.Time.Equal(want.Time)) {
			t.Errorf("ListWaitingWorkerRuns oldest=%+v want %+v", got, want)
		}
	}
	assertCapacity := func(t *testing.T, want map[uuid.UUID]time.Time) {
		t.Helper()
		rows, err := fx.q.ListOwnersWaitingNoCapacity(fx.ctx, store.ListOwnersWaitingNoCapacityParams{
			HeartbeatCutoff: ts(now.Add(-45 * time.Second)),
			RollReason:      workersvc.ReasonWorkersUpgrading,
		})
		if err != nil {
			t.Fatal(err)
		}
		found := map[uuid.UUID]bool{}
		for _, row := range rows {
			if row.UserID != fx.userID {
				continue
			}
			found[row.RunID] = true
			at, ok := want[row.RunID]
			if !ok {
				t.Errorf("ListOwnersWaitingNoCapacity included unexpected owned run %s (plan wait %s)", row.RunID, expected)
				continue
			}
			if !row.HealthSince.Valid || !row.HealthSince.Time.Equal(at) || row.HasRollReason {
				t.Errorf("ordinary waiting row=%+v want since=%v without roll reason", row, at)
			}
		}
		for run := range want {
			if !found[run] {
				t.Errorf("ListOwnersWaitingNoCapacity omitted ordinary waiting run %s", run)
			}
		}
	}
	t.Run("expected plan wait leaves global oldest and owner capacity unchanged", func(t *testing.T) {
		assertOldest(t, baseline)
		assertCapacity(t, nil)
	})

	ordinarySince := since.Add(-time.Hour)
	ordinary := seed("no worker available", ordinarySince, false)
	t.Run("ordinary reason remains counted", func(t *testing.T) {
		assertOldest(t, ts(ordinarySince))
		assertCapacity(t, map[uuid.UUID]time.Time{ordinary: ordinarySince})
	})

	nullSince := ordinarySince.Add(-time.Hour)
	nullReason := seed(nil, nullSince, false)
	t.Run("NULL reason remains counted", func(t *testing.T) {
		assertOldest(t, ts(nullSince))
		assertCapacity(t, map[uuid.UUID]time.Time{ordinary: ordinarySince, nullReason: nullSince})
	})
}
