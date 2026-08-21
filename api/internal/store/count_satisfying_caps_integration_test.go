package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CountOnlineWorkersSatisfyingCaps (PRD #84 M3) against a REAL Postgres — the queued-run
// reason resolver's "no online worker can run this" input. It counts the user's ONLINE,
// non-draining workers whose EFFECTIVE caps (capabilities ∪ {docker when docker_enabled})
// are a SUPERSET of a required set, using the SAME fold fn_worker_can_claim (migration
// 00142) applies at claim time — so a run this returns 0 for is exactly a run no online
// worker can claim. These prove, against real fleet state:
//   - 0 when only a base worker is online for a {docker} requirement;
//   - 1 when a docker-capable (self-report) OR docker_enabled worker is online;
//   - a DRAINING capable worker does NOT count;
//   - an empty required set counts all online, non-draining workers.
//
// Reuses fleetFixture (worker/workerWithCaps helpers) from the same package. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).

func countCaps(fx *fleetFixture, req []string) int64 {
	fx.t.Helper()
	n, err := fx.q.CountOnlineWorkersSatisfyingCaps(fx.ctx, store.CountOnlineWorkersSatisfyingCapsParams{
		UserID:               fx.userID,
		RequiredCapabilities: req,
	})
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersSatisfyingCaps(%v): %v", req, err)
	}
	return n
}

// drainWorker stamps draining_since so a worker keeps status='online' but is excluded.
func drainWorker(fx *fleetFixture, workerID uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, workerID)
}

func TestCountOnlineWorkersSatisfyingCapsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	// Only a base worker (no caps, not docker) is online.
	fx.worker("base", capOf(2), false)

	// A {docker} requirement: the base worker cannot satisfy it → 0.
	if n := countCaps(fx, []string{"docker"}); n != 0 {
		t.Fatalf("base-only, {docker} required = %d, want 0", n)
	}
	// An empty required set (non-nil → encoded as the SQL empty array '{}', which <@ any
	// set, NOT nil → NULL): the base worker counts → 1.
	if n := countCaps(fx, []string{}); n != 1 {
		t.Fatalf("base-only, {} required = %d, want 1 (all online non-draining count)", n)
	}

	// Add a self-reported {docker} worker: the {docker} requirement is now satisfiable by 1.
	selfDocker := workerWithCaps(fx, "selfDocker", capOf(2), []string{"docker"})
	if n := countCaps(fx, []string{"docker"}); n != 1 {
		t.Fatalf("with a self-reported docker worker, {docker} = %d, want 1", n)
	}

	// A docker_enabled worker (is_docker fold) ALSO satisfies {docker} without a self-report.
	fx.worker("hostedDocker", capOf(2), true)
	if n := countCaps(fx, []string{"docker"}); n != 2 {
		t.Fatalf("with self-report + docker_enabled, {docker} = %d, want 2 (the fold folds is_docker into effective caps)", n)
	}

	// Draining the self-reported docker worker drops it: it keeps status='online' but claims
	// nothing, so it must not count as an eligible worker.
	drainWorker(fx, selfDocker)
	if n := countCaps(fx, []string{"docker"}); n != 1 {
		t.Fatalf("after draining the self-report worker, {docker} = %d, want 1 (only the docker_enabled one)", n)
	}

	// The empty-required count now sees base + selfDocker(draining) + hostedDocker → 2 online
	// non-draining (base + hostedDocker), confirming the draining exclusion applies there too.
	if n := countCaps(fx, []string{}); n != 2 {
		t.Fatalf("{} required with one worker draining = %d, want 2 (draining excluded)", n)
	}
}
