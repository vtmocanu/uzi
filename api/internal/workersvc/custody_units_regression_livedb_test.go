package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4162 regression: this file is copied UNCHANGED into a scratch export of the base tree
// e7c7d897 for the exact-base red (issue #4162 plan section 2), so it must keep compiling there. It
// therefore uses only identifiers that exist at that base (setupCodexLiveDB, codexTestEnv,
// newCustodyContinuationFixture and its seedRun/seedUnrelatedOpenHolds/openHolds/ownOpenHolds/claim
// helpers, fakeCustodyLimit, testCustodyLimit) and raw SQL for every fixture row the units tests
// would otherwise build with their own helpers. Do not switch it to those helpers.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// TestCustodyUnitsRegressionUnrelatedRunClaimableLiveDB: one failed run holds five open generation
// holds on one worker (nothing is a healthy live claim, so each one counts per hold) and three other
// runs hold one each. That is eight counted holds, exactly the limit, but only four admission units
// (the run's five holds on its one worker are a single unit). An unrelated queued run must claim.
func TestCustodyUnitsRegressionUnrelatedRunClaimableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)

	worker := uuid.New()
	env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at) VALUES ($1,$2,'regression',$3,'offline',$4)`,
		worker, f.userID, worker[:], time.Now().Add(-24*time.Hour))
	run := f.seedRun("failed", 5)
	env.exec(`UPDATE runs SET worker_id=$2, finished_at=now() WHERE id=$1`, run, worker)
	for gen := int64(1); gen <= 5; gen++ {
		env.exec(`INSERT INTO recovery_custody_holds
		  (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,created_at)
		  VALUES ($1,$2,$3,$4,$5,'open',$6,'fixture',$6,$4,$7)`,
			uuid.New(), f.userID, f.repoID, run, gen, worker, time.Now().Add(-time.Hour+time.Duration(gen)*time.Second))
	}
	f.seedUnrelatedOpenHolds(3)
	if got := f.openHolds(t); got != testCustodyLimit {
		t.Fatalf("precondition: open holds = %d, want %d (the limit)", got, testCustodyLimit)
	}

	unrelated := f.seedRun("queued", 0)
	f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: testCustodyLimit}
	got, gen := f.claim(t)
	if got != unrelated {
		t.Fatalf("unrelated run claimed = false: claim returned %s gen %d, want the unrelated queued run %s (the owner holds %d open holds but only 4 admission units)", got, gen, unrelated, testCustodyLimit)
	}
	if gen != 1 {
		t.Fatalf("unrelated run claimed at generation %d, want 1", gen)
	}
	if n := f.ownOpenHolds(t, unrelated); n != 1 {
		t.Fatalf("claim opened %d holds for the unrelated run, want 1", n)
	}
}
