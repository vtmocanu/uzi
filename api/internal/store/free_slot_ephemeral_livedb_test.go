package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Issue #1624 against a REAL Postgres: an ephemeral (run-bound) worker can claim only its
// ephemeral_run_id, so CountOnlineWorkersWithFreeSlotForUser must never count it as having
// a free slot for another run, whatever its cap or load. Each case uses a fresh fleetFixture
// (a new user), so the user's ONLY online worker is the one under test. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).

// markOnline puts a worker online with the given max_concurrent_runs (nil = NULL cap).
func markOnline(fx *fleetFixture, workerID uuid.UUID, cap *int32) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE workers SET status = 'online', last_heartbeat_at = now(), max_concurrent_runs = $2 WHERE id = $1`,
		workerID, cap)
}

func freeSlotCount(fx *fleetFixture) int64 {
	fx.t.Helper()
	n, err := fx.q.CountOnlineWorkersWithFreeSlotForUser(fx.ctx, fx.userID)
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersWithFreeSlotForUser: %v", err)
	}
	return n
}

func TestCountOnlineWorkersWithFreeSlotExcludesEphemeralLiveDB(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }

	cases := []struct {
		name string
		cap  *int32
		busy bool // the bound run is active on the worker
	}{
		{"holding its bound run, advertised cap 2", i32(2), true},
		{"idle, NULL cap", nil, false},
		{"idle, oversized cap 5", i32(5), false},
	}
	for _, tc := range cases {
		t.Run("ephemeral "+tc.name+" has no free slot", func(t *testing.T) {
			fx := newFleetFixture(t)
			bound := queuedRunWithCaps(fx, []string{"docker"})
			wID := seedEphemeralWorkerBound(fx, bound)
			markOnline(fx, wID, tc.cap)
			if tc.busy {
				mustExec(fx.ctx, fx.t, fx.pool,
					`UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, bound, wID)
			}
			if n := freeSlotCount(fx); n != 0 {
				t.Fatalf("free-slot count = %d, want 0: a run-bound ephemeral worker never has room for another run", n)
			}
		})

		// Non-vacuity control: a persistent worker in the equivalent state IS counted, so
		// the zero above is the ephemeral exclusion and not a fixture that counts nothing.
		t.Run("persistent "+tc.name+" has a free slot", func(t *testing.T) {
			fx := newFleetFixture(t)
			wID := fx.worker("persistent", tc.cap, true)
			if tc.busy {
				fx.holdActive(wID, 1)
			}
			if n := freeSlotCount(fx); n != 1 {
				t.Fatalf("free-slot count = %d, want 1 for a persistent worker below its cap", n)
			}
		})
	}
}
