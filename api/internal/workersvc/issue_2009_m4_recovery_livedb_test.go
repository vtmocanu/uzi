package workersvc

import (
	"context"
	"testing"
	"time"
)

func TestIssue2009M4IdentityOnlyFrozenRecoveryDriftLiveDB(t *testing.T) {
	for _, drift := range []string{"owner", "generation", "kind", "target", "association"} {
		t.Run(drift, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, _, repo := e.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, e, user, "n1")
			foreign := seedSnapshotWorker(t, e, user, "other")
			lead := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
			other := seedOutageRun(t, e, user, repo, foreign, "running", "issue", 1, 0)
			child := seedRecoveryCheck(t, e, user, repo, worker, lead)
			svc := snapshotSvc(e, testParams())
			hook := recoveryHook(e.pool, true)
			svc.SetTxBeginner(hook)
			ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
			result := make(chan error, 1)
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-result
				}
			}()
			w := hbWorker(t, e, worker)
			snap := &ActiveSnapshot{RegisterNonce: "n1", SnapshotEpoch: 1, PendingAll: []PendingOutcomeIdentity{{RunID: child.String(), ClaimGeneration: 1}}}
			go func() { _, err := svc.Heartbeat(ctx, w, nil, nil, snap); result <- err }()
			awaitRecoveryCapture(t, ctx, hook)
			var sql string
			var arg any
			switch drift {
			case "owner":
				sql = "UPDATE runs SET worker_id=$2 WHERE id=$1"
				arg = foreign
			case "generation":
				sql = "UPDATE runs SET claim_generation=2 WHERE id=$1"
			case "kind":
				sql = "UPDATE runs SET kind='issue',target_run_id=NULL,issue_iid=$2 WHERE id=$1"
				arg = nextOutageIID()
			case "target":
				sql = "UPDATE runs SET target_run_id=$2 WHERE id=$1"
				arg = other
			case "association":
				sql = "DELETE FROM cross_checks WHERE checker_run_id=$1"
			}
			var err error
			if arg == nil {
				_, err = e.pool.Exec(ctx, sql, child)
			} else {
				_, err = e.pool.Exec(ctx, sql, child, arg)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(hook.release)
			select {
			case err = <-result:
				joined = true
			case <-ctx.Done():
				t.Fatal("identity-only frozen recovery did not finish", ctx.Err())
			}
			if err != nil {
				t.Fatal(err)
			}
			if pendingIdentityCount(t, e, worker) != 0 || countActiveRuns(t, e, worker) != 0 {
				t.Fatalf("%s drift stored identity or active row", drift)
			}
			if workerEpoch(t, e, worker) != 1 {
				t.Fatal("enclosing snapshot was not accepted")
			}
		})
	}
}

func TestIssue2009M4IdentityOnlyParentWaitBeforeWriteLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	worker := seedSnapshotWorker(t, e, user, "n1")
	lead := seedOutageRun(t, e, user, repo, worker, "running", "issue", 1, 0)
	child := seedRecoveryCheck(t, e, user, repo, worker, lead)
	svc := snapshotSvc(e, testParams())
	hook := recoveryHook(e.pool, false)
	svc.SetTxBeginner(hook)
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	blocker, err := e.pool.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err = blocker.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", lead); err != nil {
		cancel()
		t.Fatal(err)
	}
	result := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-result
		}
	}()
	w := hbWorker(t, e, worker)
	snap := &ActiveSnapshot{RegisterNonce: "n1", SnapshotEpoch: 1, PendingAll: []PendingOutcomeIdentity{{RunID: child.String(), ClaimGeneration: 1}}}
	go func() { _, err := svc.Heartbeat(ctx, w, nil, nil, snap); result <- err }()
	observeRecoveryParentWait(t, ctx, e, hook)
	if pendingIdentityCount(t, e, worker) != 0 {
		t.Fatal("identity FK write preceded canonical parent lock")
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		joined = true
	case <-ctx.Done():
		t.Fatal("identity write did not finish after parent unlock", ctx.Err())
	}
	if err != nil {
		t.Fatal(err)
	}
	if pendingIdentityCount(t, e, worker) != 1 || countActiveRuns(t, e, worker) != 0 {
		t.Fatal("identity-only snapshot did not write after parent wait")
	}
}

func TestIssue2009M4IdentityHasNoLifecycleLeaseLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	svc := healthSvcLive(e)
	o := seedReevalOwner(t, e, BindModeAuto, false)
	e.exec("UPDATE workers SET snapshot_register_nonce='n1',snapshot_epoch=0,last_heartbeat_at=now() WHERE id=$1", o.workerID)
	run := seedRunningRun(t, e, o, nextOutageIID(), 0)
	setRunGeneration(t, e, run, 2)
	m4Heartbeat(t, e, svc, o.workerID, &ActiveSnapshot{PendingAll: []PendingOutcomeIdentity{{RunID: run.String(), ClaimGeneration: 2}}})
	m4Aggregate(t, e, o.workerID, 1, false)
	lease, err := e.q.RunHasPendingOutcomeLease(e.ctx, run)
	if err != nil || lease {
		t.Fatalf("identity acquired lifecycle lease=%v err=%v", lease, err)
	}
	current, err := e.q.GetRunByID(e.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := svc.hasLivePoller(e.ctx, current); err != nil || !live {
		t.Fatalf("identity suppressed fresh poller=%v err=%v", live, err)
	}
	e.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", o.workerID)
	if live, err := svc.hasLivePoller(e.ctx, current); err != nil || live {
		t.Fatalf("identity invented stale poller=%v err=%v", live, err)
	}
}
