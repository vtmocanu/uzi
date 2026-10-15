package workersvc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the query's own owner fence separately from recovery capture, which
// already excludes foreign-owned rows in route-level tests. A valid frozen ledger
// for one worker must not authorize inserting evidence for a different worker.
func TestIssue2009M4IdentitySQLWorkerContextLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	owner := seedSnapshotWorker(t, e, user, "n1")
	reporter := seedSnapshotWorker(t, e, user, "n2")
	run := seedOutageRun(t, e, user, repo, owner, "running", "issue", 2, 0)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	qtx := e.q.WithTx(tx)
	workers := [2]uuid.UUID{owner, reporter}
	if workers[0].String() > workers[1].String() {
		workers[0], workers[1] = workers[1], workers[0]
	}
	// Hold both worker contexts before capturing ordered parents and targets.
	for _, worker := range workers {
		if _, err := qtx.GetWorkerForUpdate(e.ctx, worker); err != nil {
			t.Fatal(err)
		}
	}
	locks, err := captureWorkerRecoveryLocks(e.ctx, qtx, owner, nil, validatedFinalizeResume{}, []uuid.UUID{run})
	if err != nil {
		t.Fatal(err)
	}
	frozen, parents, err := locks.parameters()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.LockFrozenWorkerSnapshotRuns(e.ctx, store.LockFrozenWorkerSnapshotRunsParams{
		WorkerID: pgconv.UUID(owner), RunIds: []uuid.UUID{run}, FrozenTargets: frozen, LockedParentIds: parents,
	}); err != nil {
		t.Fatal(err)
	}
	params := store.UpsertFrozenWorkerPendingOutcomeIdentityParams{
		WorkerID: owner, RunID: run, ClaimGeneration: 2, LeaseSeconds: 120,
		FrozenTargets: frozen, LockedParentIds: parents,
	}
	if rows, err := qtx.UpsertFrozenWorkerPendingOutcomeIdentity(e.ctx, params); err != nil || rows != 1 {
		t.Fatalf("matching-owner positive control: rows=%d err=%v", rows, err)
	}
	params.WorkerID = reporter
	if rows, err := qtx.UpsertFrozenWorkerPendingOutcomeIdentity(e.ctx, params); err != nil || rows != 0 {
		t.Fatalf("foreign frozen owner accepted in reporter context: rows=%d err=%v", rows, err)
	}
	var count int
	if err := tx.QueryRow(e.ctx, "SELECT count(*) FROM worker_pending_outcome_identities WHERE worker_id=$1", reporter).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("SQL owner fence inserted reporter evidence for a foreign-owned run")
	}
}

func TestIssue2009M4ReportedGenerationWriteFenceLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, e, user, "n1")
	svc := snapshotSvc(e, testParams())
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
	for _, gen := range []int64{1, 3} {
		m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: gen}}})
		if pendingIdentityCount(t, e, worker) != 0 {
			t.Fatalf("reported generation %d inserted against frozen/current generation 2", gen)
		}
	}
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 1}, {RunID: run.String(), ClaimGeneration: 2}, {RunID: run.String(), ClaimGeneration: 3}}})
	var got int64
	if err := e.pool.QueryRow(e.ctx, "SELECT claim_generation FROM worker_pending_outcome_identities WHERE worker_id=$1 AND run_id=$2", worker, run).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 2 || pendingIdentityCount(t, e, worker) != 1 {
		t.Fatal("multi-generation report did not keep only current exact tuple")
	}
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 1}}})
	if pendingIdentityCount(t, e, worker) != 0 {
		t.Fatal("complete omission retained current tuple by run-only membership")
	}
}

func TestIssue2009M4ReleaseWriteFenceLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, e, user, "n1")
	svc := snapshotSvc(e, testParams())
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
	list := []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}
	e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
	if pendingIdentityCount(t, e, worker) != 0 {
		t.Fatal("released current tuple inserted")
	}
	e.exec("UPDATE runs SET claim_released_at=NULL WHERE id=$1", run)
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
	before, until := pendingIdentityTimes(t, e, worker, run)
	e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
	after, afterUntil := pendingIdentityTimes(t, e, worker, run)
	if !before.Equal(after) || !until.Equal(afterUntil) {
		t.Fatal("released current tuple renewed")
	}
}

func TestIssue2009M4OwnerReadAndTerminalFreshnessLiveDB(t *testing.T) {
	for _, control := range []string{"foreign-owner", "terminal", "expired", "released", "stale-generation"} {
		t.Run(control, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			svc := healthSvcLive(e)
			user, _, repo := e.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, e, user, "n1")
			other := seedSnapshotWorker(t, e, user, "n2")
			run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
			e.exec("INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,first_seen_at,reported_until) VALUES($1,$2,2,now()-interval '1 hour',now()+interval '1 hour')", worker, run)
			switch control {
			case "foreign-owner":
				e.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", run, other)
			case "terminal":
				e.exec("UPDATE runs SET status='completed' WHERE id=$1", run)
			case "expired":
				e.exec("UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 second' WHERE run_id=$1", run)
			case "released":
				e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
			case "stale-generation":
				e.exec("UPDATE runs SET claim_generation=3 WHERE id=$1", run)
			}
			// Read before any sweep or writer physically removes the dormant record.
			m4Aggregate(t, e, worker, 0, false)
			svc.detectRunHealth(e.ctx, time.Now())
			if _, reason := runHealthOf(t, e, run); reason == reasonOutcomeUndelivered {
				t.Fatal("ineligible stored evidence raised outcome health")
			}
			if pendingIdentityCount(t, e, worker) != 1 {
				t.Fatal("read unexpectedly pruned dormant evidence")
			}
			if control == "foreign-owner" {
				before, until := pendingIdentityTimes(t, e, worker, run)
				m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}})
				after, afterUntil := pendingIdentityTimes(t, e, worker, run)
				if !before.Equal(after) || !until.Equal(afterUntil) {
					t.Fatal("foreign-owned report renewed")
				}
			}
		})
	}
}

func TestIssue2009M4FreshContinuityAndLegacyMinimumLiveDB(t *testing.T) {
	for _, control := range []string{"expired-no-snapshot", "expired-fresh-snapshot", "fresh-identity-older", "fresh-snapshot-older"} {
		t.Run(control, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, _, repo := e.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, e, user, "n1")
			svc := healthSvcLive(e)
			run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
			list := []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}
			m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
			e.exec("UPDATE worker_pending_outcome_identities SET first_seen_at=now()-interval '2 hours' WHERE run_id=$1", run)
			old, _ := pendingIdentityTimes(t, e, worker, run)
			expected := old
			if control != "expired-no-snapshot" {
				heartbeatSnapshot(t, svc, e, worker, entry(run, 2, "running", true))
				e.exec("UPDATE worker_active_runs SET terminal_pending_since=now()-interval '1 hour' WHERE run_id=$1", run)
				if control == "fresh-snapshot-older" {
					e.exec("UPDATE worker_active_runs SET terminal_pending_since=now()-interval '3 hours' WHERE run_id=$1", run)
				}
				if control == "expired-fresh-snapshot" || control == "fresh-snapshot-older" {
					s, _ := readPendingSince(t, e, worker, run)
					expected = s.Time
				}
			}
			if control == "expired-no-snapshot" || control == "expired-fresh-snapshot" {
				e.exec("UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 second' WHERE run_id=$1", run)
			}
			before := time.Now().Add(-time.Second)
			m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
			got, _ := pendingIdentityTimes(t, e, worker, run)
			if control == "expired-no-snapshot" {
				if got.Before(before) {
					t.Fatalf("expired identity retained old age: %v", got)
				}
			} else if !got.Equal(expected) {
				t.Fatalf("fresh evidence minimum=%v want=%v", got, expected)
			}
		})
	}
}

func TestIssue2009M4OptionalJSONAndEnclosingFencesLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, e, user, "n1")
	svc := snapshotSvc(e, testParams())
	run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
	list := []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{PendingAll: list})
	since, until := pendingIdentityTimes(t, e, worker, run)
	for _, wire := range []string{
		"{}",
		`{"pending_all":null}`,
		`{"pending_all":[{"run_id":"bad","claim_generation":2}]}`,
		`{"pending_all":[{"run_id":"` + run.String() + `","claim_generation":-1}]}`,
		`{"pending_all":[{"run_id":"` + run.String() + `","claim_generation":2},{"run_id":"` + run.String() + `","claim_generation":2}]}`,
	} {
		var snap ActiveSnapshot
		if err := json.Unmarshal([]byte(wire), &snap); err != nil {
			t.Fatal(err)
		}
		snap.Active = []ActiveRunEntry{entry(run, 2, "running", true)}
		snap.PendingOverflow = true
		m4Heartbeat(t, e, svc, worker, &snap)
		got, gotUntil := pendingIdentityTimes(t, e, worker, run)
		if !got.Equal(since) || !gotUntil.Equal(until) {
			t.Fatalf("optional JSON renewed identity: %s", wire)
		}
		if row, ok := readActiveRun(t, e, worker, run); !ok || !row.terminalPending {
			t.Fatal("invalid optional data lost legitimate active protection")
		}
		if flag, fresh := workerOverflow(t, e, worker); !flag || !fresh {
			t.Fatal("invalid optional data lost overflow protection")
		}
	}
	for _, control := range []string{"stale-epoch", "wrong-nonce", "invalid-active"} {
		w := hbWorker(t, e, worker)
		snap := &ActiveSnapshot{SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, PendingAll: []PendingOutcomeIdentity{}}
		switch control {
		case "stale-epoch":
			snap.SnapshotEpoch = w.SnapshotEpoch
		case "wrong-nonce":
			snap.RegisterNonce = "wrong"
		case "invalid-active":
			snap.Active = []ActiveRunEntry{entry(run, 2, "running", true), entry(run, 2, "running", true)}
		}
		if applied, err := applySnapshot(t, svc, e, worker, snap, snapshotModeHeartbeat); err != nil || applied {
			t.Fatalf("%s applied=%v err=%v", control, applied, err)
		}
		got, gotUntil := pendingIdentityTimes(t, e, worker, run)
		if !got.Equal(since) || !gotUntil.Equal(until) || pendingIdentityCount(t, e, worker) != 1 {
			t.Fatalf("%s changed identities", control)
		}
	}
	m4Heartbeat(t, e, svc, worker, &ActiveSnapshot{Active: []ActiveRunEntry{entry(run, 2, "running", true)}, PendingAll: []PendingOutcomeIdentity{}})
	if pendingIdentityCount(t, e, worker) != 0 {
		t.Fatal("complete empty did not clear identities")
	}
	m4Aggregate(t, e, worker, 1, false)
}
