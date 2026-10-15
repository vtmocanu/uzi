package workersvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func pendingIdentityTimes(t *testing.T, e codexTestEnv, worker, run uuid.UUID) (time.Time, time.Time) {
	t.Helper()
	var since, until time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT first_seen_at, reported_until FROM worker_pending_outcome_identities WHERE worker_id=$1 AND run_id=$2`, worker, run).Scan(&since, &until); err != nil {
		t.Fatal(err)
	}
	return since, until
}

func pendingIdentityCount(t *testing.T, e codexTestEnv, worker uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_pending_outcome_identities WHERE worker_id=$1`, worker).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPendingAllFourApplyRoutesLiveDB(t *testing.T) {
	for _, route := range []string{"compatibility", "heartbeat", "register", "claim"} {
		t.Run(route, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, _, repo := e.seedCodexInfra(t)
			svc := snapshotSvc(e, testParams())
			worker := seedSnapshotWorker(t, e, user, "n1")
			run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
			apply := func(snap *ActiveSnapshot) {
				t.Helper()
				w, err := e.q.GetWorkerByID(e.ctx, worker)
				if err != nil {
					t.Fatal(err)
				}
				snap.RegisterNonce = w.SnapshotRegisterNonce.String
				snap.SnapshotEpoch = w.SnapshotEpoch + 1
				snap.PendingOverflow = true // Preserve existing lifecycle closure in each route.
				switch route {
				case "compatibility":
					if ok, err := applySnapshot(t, svc, e, worker, snap, snapshotModeHeartbeat); err != nil || !ok {
						t.Fatalf("apply: %v %v", ok, err)
					}
				case "heartbeat":
					if _, err := svc.Heartbeat(e.ctx, w, nil, nil, snap); err != nil {
						t.Fatal(err)
					}
				case "register":
					snap.SnapshotEpoch = 0
					if _, _, err := svc.Register(e.ctx, w, "v1", "base", nil, nil, nil, snap); err != nil {
						t.Fatal(err)
					}
				case "claim":
					if payload, err := svc.Claim(e.ctx, w, snap); err != nil || payload != nil {
						t.Fatalf("claim: %v %v", payload, err)
					}
				}
			}
			identity := []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 1}}
			apply(&ActiveSnapshot{PendingAll: identity})
			if n := pendingIdentityCount(t, e, worker); n != 1 {
				t.Fatalf("count=%d, want 1", n)
			}
			if n := countActiveRuns(t, e, worker); n != 0 {
				t.Fatalf("identity entered active list: %d", n)
			}
			e.exec(`UPDATE worker_pending_outcome_identities SET first_seen_at=now()-interval '1 hour', reported_until=now()+interval '30 seconds' WHERE worker_id=$1`, worker)
			old, until := pendingIdentityTimes(t, e, worker, run)
			apply(&ActiveSnapshot{}) // Unavailable must neither replace nor renew.
			got, gotUntil := pendingIdentityTimes(t, e, worker, run)
			if !got.Equal(old) || !gotUntil.Equal(until) {
				t.Fatal("unavailable coverage changed identity")
			}
			var bad ActiveSnapshot
			if err := json.Unmarshal([]byte(`{"pending_all":[{"run_id":"`+run.String()+`"}]}`), &bad); err != nil {
				t.Fatal(err)
			}
			apply(&bad)
			got, gotUntil = pendingIdentityTimes(t, e, worker, run)
			if !got.Equal(old) || !gotUntil.Equal(until) {
				t.Fatal("invalid coverage changed identity")
			}
			apply(&ActiveSnapshot{PendingAll: identity})
			got, gotUntil = pendingIdentityTimes(t, e, worker, run)
			if !got.Equal(old) || !gotUntil.After(until) {
				t.Fatal("renewal lost age or failed to renew")
			}
			apply(&ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{}})
			if n := pendingIdentityCount(t, e, worker); n != 0 {
				t.Fatalf("complete empty count=%d", n)
			}
		})
	}
}

func TestPendingAllValidationAndTupleReplacementLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	svc := snapshotSvc(e, testParams())
	worker := seedSnapshotWorker(t, e, user, "n1")
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
	apply := func(list []PendingOutcomeIdentity, active []ActiveRunEntry) bool {
		t.Helper()
		w, err := e.q.GetWorkerByID(e.ctx, worker)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := applySnapshot(t, svc, e, worker, &ActiveSnapshot{RegisterNonce: "n1", SnapshotEpoch: w.SnapshotEpoch + 1, Active: active, PendingAll: list}, snapshotModeHeartbeat)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	valid := PendingOutcomeIdentity{RunID: run.String(), ClaimGeneration: 1}
	apply([]PendingOutcomeIdentity{valid}, nil)
	since, until := pendingIdentityTimes(t, e, worker, run)
	over := make([]PendingOutcomeIdentity, svc.p.ActiveSnapshotMaxEntries+1)
	for i := range over {
		over[i] = PendingOutcomeIdentity{RunID: uuid.NewString(), ClaimGeneration: 1}
	}
	for _, list := range [][]PendingOutcomeIdentity{
		{{RunID: "bad", ClaimGeneration: 1}},
		{{RunID: run.String(), ClaimGeneration: -1}},
		{valid, valid},
		over,
	} {
		if !apply(list, []ActiveRunEntry{entry(run, 1, "running", false)}) {
			t.Fatal("invalid optional list rejected enclosing snapshot")
		}
		got, gotUntil := pendingIdentityTimes(t, e, worker, run)
		if !got.Equal(since) || !gotUntil.Equal(until) {
			t.Fatal("invalid list replaced or renewed coverage")
		}
	}
	// Rejected enclosing duplicate active entries carry no complete-empty coverage.
	if apply([]PendingOutcomeIdentity{}, []ActiveRunEntry{entry(run, 1, "running", false), entry(run, 1, "running", false)}) {
		t.Fatal("duplicate active accepted")
	}
	if n := pendingIdentityCount(t, e, worker); n != 1 {
		t.Fatal("rejected snapshot cleared coverage")
	}
	// Same run, distinct generations is a valid tuple set; only the exact current
	// generation may be stored. Complete omission deletes by tuple, not run alone.
	apply([]PendingOutcomeIdentity{valid, {RunID: run.String(), ClaimGeneration: 2}}, nil)
	if n := pendingIdentityCount(t, e, worker); n != 1 {
		t.Fatalf("exact generation count=%d", n)
	}
	apply([]PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}, nil)
	if n := pendingIdentityCount(t, e, worker); n != 0 {
		t.Fatal("omitted tuple retained by run-only membership")
	}
}

func TestPendingAllFreshnessEligibilityAggregatesLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	svc := snapshotSvc(e, testParams())
	worker := seedSnapshotWorker(t, e, user, "n1")
	emptyWorker := seedSnapshotWorker(t, e, user, "n2")
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
	apply := func(active []ActiveRunEntry) {
		t.Helper()
		w, err := e.q.GetWorkerByID(e.ctx, worker)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := applySnapshot(t, svc, e, worker, &ActiveSnapshot{RegisterNonce: "n1", SnapshotEpoch: w.SnapshotEpoch + 1, Active: active, PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 1}}}, snapshotModeHeartbeat); err != nil || !ok {
			t.Fatalf("apply %v %v", ok, err)
		}
	}
	// Upgrade age comes from a fresh eligible legacy snapshot before replacement.
	heartbeatSnapshot(t, svc, e, worker, entry(run, 1, "running", true))
	e.exec(`UPDATE worker_active_runs SET terminal_pending_since=now()-interval '1 hour' WHERE worker_id=$1`, worker)
	var seed time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT terminal_pending_since FROM worker_active_runs WHERE worker_id=$1`, worker).Scan(&seed); err != nil {
		t.Fatal(err)
	}
	apply(nil)
	got, _ := pendingIdentityTimes(t, e, worker, run)
	if !got.Equal(seed) {
		t.Fatal("upgrade lost fresh legacy age")
	}
	e.exec(`UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 second' WHERE worker_id=$1`, worker)
	apply(nil)
	got, _ = pendingIdentityTimes(t, e, worker, run)
	if !got.After(seed.Add(30 * time.Minute)) {
		t.Fatal("expired identity retained stale age")
	}
	apply([]ActiveRunEntry{entry(run, 1, "running", true)})
	e.exec(`UPDATE workers SET pending_overflow=true,pending_overflow_until=now()-interval '1 second' WHERE id=$1`, worker)
	rows, err := e.q.ListWorkerPendingOutcomeAggregates(e.ctx, []uuid.UUID{worker, emptyWorker})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("aggregate workers=%d", len(rows))
	}
	for _, r := range rows {
		want := int64(0)
		if r.WorkerID == worker {
			want = 1
		}
		if r.PendingCount != want || r.PendingOverflowFresh {
			t.Fatalf("aggregate %#v, want count=%d fresh=false", r, want)
		}
	}
	for _, mutation := range []string{
		`UPDATE runs SET claim_generation=2 WHERE id=$1`,
		`UPDATE runs SET claim_generation=1,claim_released_at=now() WHERE id=$1`,
		`UPDATE runs SET claim_released_at=NULL,status='failed',finished_at=now() WHERE id=$1`,
		`UPDATE runs SET status='running',finished_at=NULL,worker_id=NULL WHERE id=$1`,
	} {
		e.exec(mutation, run)
		fresh, err := e.q.ListFreshWorkerPendingOutcomeSince(e.ctx, worker)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh) != 0 {
			t.Fatalf("obsolete evidence seeds continuity: %#v", fresh)
		}
		rows, err = e.q.ListWorkerPendingOutcomeAggregates(e.ctx, []uuid.UUID{worker})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].PendingCount != 0 {
			t.Fatalf("obsolete aggregate: %#v", rows)
		}
		beforeSince, beforeUntil := pendingIdentityTimes(t, e, worker, run)
		apply(nil)
		afterSince, afterUntil := pendingIdentityTimes(t, e, worker, run)
		// Existing rows may remain until expiry/omission, but ineligible reports
		// must not renew them or become fresh health evidence.
		if !afterSince.Equal(beforeSince) || !afterUntil.Equal(beforeUntil) {
			t.Fatal("ineligible identity report changed age or renewed evidence")
		}
	}
}

func TestPendingAllSweepPrunesWithHealthDisabledLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	svc := snapshotSvc(e, testParams()) // No settings reader: health detector disabled.
	worker := seedSnapshotWorker(t, e, user, "n1")
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
	e.exec(`INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,reported_until) VALUES($1,$2,1,now()-interval '1 second')`, worker, run)
	if _, err := svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := pendingIdentityCount(t, e, worker); n != 0 {
		t.Fatalf("expired count=%d", n)
	}
	current, err := e.q.GetRunByID(e.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "running" {
		t.Fatalf("cleanup changed run: %s", current.Status)
	}
}
