package workersvc

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type liveCustodyClaimFixture struct {
	*custodyContinuationFixture
	now                  time.Time
	runs, holds, workers []uuid.UUID
}

func newLiveCustodyClaimFixture(t *testing.T, env codexTestEnv, n int, guarded bool) *liveCustodyClaimFixture {
	t.Helper()
	f := &liveCustodyClaimFixture{custodyContinuationFixture: newCustodyContinuationFixture(t, env),
		now: time.Now().UTC().Truncate(time.Microsecond)}
	// Deliberately differs from the default: tests prove the configured window reaches SQL.
	f.svc.p.WorkerHeartbeatStale = 137 * time.Second
	f.svc.now = func() time.Time { return f.now }
	env.exec("UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", f.workerID, f.now)
	t.Cleanup(func() { env.exec("DELETE FROM recovery_captures WHERE user_id=$1", f.userID) })
	for range n {
		worker := uuid.New()
		env.exec("INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at) VALUES ($1,$2,'healthy',$3,'offline',$4)",
			worker, f.userID, worker[:], f.now)
		run := f.seedRun("running", 1)
		env.exec("UPDATE runs SET worker_id=$1 WHERE id=$2", worker, run)
		hold := uuid.New()
		env.exec(`INSERT INTO recovery_custody_holds
   (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
   VALUES ($1,$2,$3,$4,1,'open',$5,'fixture',$6,$4,$7)`, hold, f.userID, f.repoID, run, uuid.New(), worker, guarded)
		f.workers = append(f.workers, worker)
		f.runs = append(f.runs, run)
		f.holds = append(f.holds, hold)
	}
	return f
}

func (f *liveCustodyClaimFixture) capture(i int, state string, at time.Time) {
	f.env.exec(`INSERT INTO recovery_captures
  (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,created_at)
  VALUES ($1,$2,$3,$4,'fixture','abc',$5,$6,$7)`, uuid.New(), f.holds[i], f.runs[i], f.userID, uuid.NewString(), state, at)
}

// This drives the public worker service, not merely the discounted count helper.
func TestFreshClaimCustodyAdmissionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cases := []struct {
		name      string
		guarded   bool
		exhausted bool
		counted   int64
		mutate    func(*liveCustodyClaimFixture, int)
	}{
		{name: "eight healthy", counted: 0},
		{name: "eight exhaustion no capture guarded=false", guarded: false, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
		}},
		{name: "eight exhaustion no capture guarded=true", guarded: true, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
		}},
		{name: "eight exhaustion available guarded=false", guarded: false, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "available", f.now)
		}},
		{name: "eight exhaustion available guarded=true", guarded: true, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "available", f.now)
		}},
		{name: "eight exhaustion preparing guarded=false", guarded: false, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "preparing", f.now)
		}},
		{name: "eight exhaustion preparing guarded=true", guarded: true, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "preparing", f.now)
		}},
		{name: "eight exhaustion uploading guarded=false", guarded: false, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "uploading", f.now)
		}},
		{name: "eight exhaustion uploading guarded=true", guarded: true, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "uploading", f.now)
		}},
		{name: "eight exhaustion needs_action guarded=false", guarded: false, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "needs_action", f.now)
		}},
		{name: "eight exhaustion needs_action guarded=true", guarded: true, exhausted: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", f.runs[i])
			f.capture(i, "needs_action", f.now)
		}},
		{name: "needs action", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) { f.capture(i, "needs_action", f.now) }},
		{name: "guarded earlier archive and latest needs action", guarded: true, counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.capture(i, "available", f.now.Add(-time.Second))
			f.capture(i, "needs_action", f.now)
		}},
		{name: "stale heartbeat", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", f.workers[i], f.now.Add(-f.svc.p.WorkerHeartbeatStale-time.Microsecond))
		}},
		{name: "exact heartbeat boundary", counted: 0, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", f.workers[i], f.now.Add(-f.svc.p.WorkerHeartbeatStale))
		}},
		{name: "after heartbeat boundary", counted: 0, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", f.workers[i], f.now.Add(-f.svc.p.WorkerHeartbeatStale+time.Microsecond))
		}},
		{name: "null heartbeat", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE workers SET last_heartbeat_at=NULL WHERE id=$1", f.workers[i])
		}},
		{name: "older generation", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET generation=0 WHERE id=$1", f.holds[i])
		}},
		{name: "future generation", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET generation=2 WHERE id=$1", f.holds[i])
		}},
		{name: "released claim", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET claim_released_at=$2 WHERE id=$1", f.runs[i], f.now)
		}},
		{name: "mismatched live worker", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1", f.holds[i], f.workerID)
		}},
		{name: "null live worker", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1", f.holds[i])
		}},
		{name: "null run worker", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET worker_id=NULL WHERE id=$1", f.runs[i])
		}},
		{name: "null live run", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET live_run_id=NULL WHERE id=$1", f.holds[i])
		}},
		{name: "mismatched live run", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", f.holds[i], f.runs[(i+1)%len(f.runs)])
		}},
		{name: "terminal run", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='completed' WHERE id=$1", f.runs[i])
		}},
		{name: "paused run", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='paused' WHERE id=$1", f.runs[i])
		}},
		{name: "recovery wait", counted: 8, mutate: func(f *liveCustodyClaimFixture, i int) {
			f.env.exec("UPDATE runs SET status='recovery_wait' WHERE id=$1", f.runs[i])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLiveCustodyClaimFixture(t, env, 8, tc.guarded)
			for i := range f.runs {
				if tc.mutate != nil {
					tc.mutate(f, i)
				}
			}
			if tc.exhausted {
				owners, err := env.q.ListCustodyHoldsForOwner(env.ctx, store.ListCustodyHoldsForOwnerParams{UserID: f.userID})
				if err != nil {
					t.Fatal(err)
				}
				workers, err := env.q.ListOpenCustodyHoldsForWorkers(env.ctx, f.workers)
				if err != nil {
					t.Fatal(err)
				}
				if len(owners) != 8 || len(workers) != 8 {
					t.Fatalf("exhaustion listings owners=%d workers=%d", len(owners), len(workers))
				}
				for _, row := range owners {
					if !row.DecisionNeeded || row.RecoveryWaitCause != "worker_requeue_exhausted" {
						t.Fatalf("owner exhaustion facts=%+v", row)
					}
				}
				for _, row := range workers {
					if !row.DecisionNeeded || row.RecoveryWaitCause != "worker_requeue_exhausted" {
						t.Fatalf("worker exhaustion facts=%+v", row)
					}
				}
			}
			fresh := f.seedRun("queued", 0)
			cutoff := pgconv.Time(f.now.Add(-f.svc.p.WorkerHeartbeatStale))
			adm, err := env.q.GetCustodyAdmissionForRun(env.ctx, store.GetCustodyAdmissionForRunParams{
				UserID: f.userID, RunID: fresh, CustodyHoldLimit: custodyHoldLimit, HeartbeatCutoff: cutoff})
			if err != nil {
				t.Fatal(err)
			}
			if adm.OpenHolds != 8 || adm.AdmissionCountedHolds != tc.counted || adm.ContinuationExempt {
				t.Fatalf("admission facts = %+v, want total8/count%d/no exemption", adm, tc.counted)
			}
			reason := f.svc.queuedReason(env.ctx, f.now, store.ListActiveRunsForHealthRow{ID: fresh, UserID: f.userID, Kind: "issue"})
			if (reason == reasonCustodyLimit) != (tc.counted >= 8) {
				t.Fatalf("health reason=%q count=%d", reason, tc.counted)
			}
			got, _ := f.claim(t)
			if tc.counted < 8 {
				if got != fresh {
					t.Fatalf("fresh claim=%s want%s", got, fresh)
				}
			} else {
				if got != uuid.Nil {
					t.Fatalf("blocked claim=%s", got)
				}
				var status string
				if err := env.pool.QueryRow(env.ctx, "SELECT status FROM runs WHERE id=$1", fresh).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "queued" {
					t.Fatalf("blocked fresh status=%s", status)
				}
			}
			if f.openHolds(t) != 8+int64(boolInt(tc.counted < 8)) {
				t.Fatal("accounting released a hold or failed to open claimed custody")
			}
			// A zero admission count is never permission to prune the source.
			if tc.name == "eight healthy" {
				workers, err := env.q.ListWorkersByUser(env.ctx, f.userID)
				if err != nil {
					t.Fatal(err)
				}
				retained := 0
				for _, w := range workers {
					if w.RetainingUnpublishedWork {
						retained++
					}
				}
				if retained != 9 {
					t.Fatalf("retaining_unpublished_work workers=%d want9", retained)
				}
				env.exec("UPDATE workers SET kind='hosted', hosted_size='s', template_declared='base' WHERE id=$1", f.workers[0])
				desired, err := env.q.ListHostedWorkersForController(env.ctx, store.ListHostedWorkersForControllerParams{HeartbeatCutoff: cutoff})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, w := range desired {
					if w.ID == f.workers[0] {
						found = true
						if !w.CustodyHeld {
							t.Fatal("healthy admission discount cleared custody_held")
						}
					}
				}
				if !found {
					t.Fatal("hosted custody worker missing from desired state")
				}
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Both healthy claims can be admitted; loss of live evidence can subsequently exceed
// the fixed allowance. This remains an admission gate, not an owner-wide ceiling.
func TestCustodyConcurrentAdmissionPressureLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newLiveCustodyClaimFixture(t, env, 0, false)
	f.seedUnrelatedOpenHolds(7)
	first, second := f.seedRun("queued", 0), f.seedRun("queued", 0)
	other := uuid.New()
	env.exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at) VALUES($1,$2,'second',$3,'online',$4)", other, f.userID, other[:], f.now)
	workers := []store.Worker{f.wkr, {ID: other, UserID: f.userID, Name: "second", Status: "online", ProtocolCapabilities: f.wkr.ProtocolCapabilities}}
	start := make(chan struct{})
	results := make(chan error, 2)
	ids := make(chan uuid.UUID, 2)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w store.Worker) {
			defer wg.Done()
			<-start
			payload, err := f.svc.Claim(env.ctx, w, nil)
			if err != nil {
				results <- err
				return
			}
			if payload == nil {
				results <- fmt.Errorf("concurrent claim was idle")
				return
			}
			id, err := uuid.Parse(payload.RunID)
			if err != nil {
				results <- err
				return
			}
			ids <- id
			results <- nil
		}(w)
	}
	close(start)
	wg.Wait()
	close(results)
	close(ids)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uuid.UUID]bool{}
	for id := range ids {
		seen[id] = true
	}
	if !seen[first] || !seen[second] || len(seen) != 2 {
		t.Fatalf("concurrent claims=%v", seen)
	}
	env.exec("UPDATE runs SET status='paused' WHERE id=ANY($1)", []uuid.UUID{first, second})
	fresh := f.seedRun("queued", 0)
	adm, err := env.q.GetCustodyAdmissionForRun(env.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.userID, RunID: fresh, CustodyHoldLimit: custodyHoldLimit, HeartbeatCutoff: pgconv.Time(f.now.Add(-f.svc.p.WorkerHeartbeatStale))})
	if err != nil {
		t.Fatal(err)
	}
	if adm.OpenHolds != 9 || adm.AdmissionCountedHolds != 9 {
		t.Fatalf("post-claim pressure=%+v", adm)
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("later fresh claim=%s; want blocked", got)
	}
}
