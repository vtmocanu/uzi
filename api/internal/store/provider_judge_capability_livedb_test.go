package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4322 M1a: a provider-bound Claude judge is claimable and placeable only on a worker advertising
// claude_provider_judge_v1 in addition to claude_provider_v1. Every test seeds judge rows directly
// (kind 'judge', target_run_id set, the five binding columns set, harness claude); one worker is
// v1-only and one is judge-capable. Each non-judge or vendor-judge control proves the clause is scoped
// to a bound judge. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).

const claudeProviderJudgeCap = "claude_provider_judge_v1"

var (
	v1OnlyCaps    = []string{claudeProviderCap}
	judgeCapaCaps = []string{claudeProviderCap, claudeProviderJudgeCap}
)

// boundJudge seeds a provider-bound judge of a fresh cancelled target. age sets created_at, so a
// background judge older than the grace cutoff counts as demand for the ephemeral triggers.
func boundJudge(fx *fleetFixture, age time.Duration) uuid.UUID {
	fx.t.Helper()
	target := fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
	judge := fx.insertJudge(nil, target, time.Now().Add(-age))
	bindRunToProvider(fx, judge, "pj-"+uuid.NewString())
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, judge)
	return judge
}

// vendorJudge seeds an unbound judge (no provider binding) of a fresh cancelled target.
func vendorJudge(fx *fleetFixture, age time.Duration) uuid.UUID {
	fx.t.Helper()
	target := fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
	return fx.insertJudge(nil, target, time.Now().Add(-age))
}

// boundIssue seeds a provider-bound non-judge run, the control a judge-scoped clause must not touch.
func boundIssue(fx *fleetFixture) uuid.UUID {
	fx.t.Helper()
	id := fx.queuedRun()
	bindRunToProvider(fx, id, "pi-"+uuid.NewString())
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, id)
	return id
}

func providerJudgeClaim(fx *fleetFixture, worker uuid.UUID, caps []string) (store.Run, error) {
	now := time.Now()
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:                 pgtype.UUID{Bytes: worker, Valid: true},
		UserID:                   fx.userID,
		CodexCuratedModels:       []string{"gpt-6-sol"},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		WorkerCaps:               []string{},
		CapabilityAware:          true,
		WorkerProtocolCaps:       caps,
		SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
	})
}

func wantClaim(t *testing.T, got store.Run, err error, want uuid.UUID, why string) {
	t.Helper()
	if err != nil || got.ID != want {
		t.Fatalf("%s: claimed %s err=%v, want %s", why, got.ID, err, want)
	}
}

func wantNoClaim(t *testing.T, got store.Run, err error, why string) {
	t.Helper()
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s: claimed %s err=%v, want no rows", why, got.ID, err)
	}
}

// ClaimRun (runtime.sql, the claimant's provider clause).
func TestProviderJudgeCapabilityClaimRunLiveDB(t *testing.T) {
	t.Run("a v1-only worker cannot claim a bound judge; a judge-capable worker can", func(t *testing.T) {
		fx := newFleetFixture(t)
		judge := boundJudge(fx, time.Minute)
		v1 := fx.jobWorker("v1-only", capOf(2), false, v1OnlyCaps...)
		got, err := providerJudgeClaim(fx, v1, v1OnlyCaps)
		wantNoClaim(t, got, err, "v1-only worker, bound judge")
		capable := fx.jobWorker("judge-capable", capOf(2), false, judgeCapaCaps...)
		got, err = providerJudgeClaim(fx, capable, judgeCapaCaps)
		wantClaim(t, got, err, judge, "judge-capable worker, bound judge")
		if got.Kind != "judge" {
			t.Fatalf("claimed kind %q, want judge", got.Kind)
		}
	})
	t.Run("a non-judge provider run is still claimable by a v1-only worker", func(t *testing.T) {
		fx := newFleetFixture(t)
		run := boundIssue(fx)
		v1 := fx.jobWorker("v1-only", capOf(2), false, v1OnlyCaps...)
		got, err := providerJudgeClaim(fx, v1, v1OnlyCaps)
		wantClaim(t, got, err, run, "v1-only worker, bound issue run")
	})
	t.Run("a vendor judge needs no provider capability", func(t *testing.T) {
		fx := newFleetFixture(t)
		judge := vendorJudge(fx, time.Minute)
		old := fx.jobWorker("no-caps", capOf(2), false)
		got, err := providerJudgeClaim(fx, old, []string{})
		wantClaim(t, got, err, judge, "capability-less worker, vendor judge")
	})
}

// The fleet-spread peer mirror: a busy claimant defers a fresh run to a strictly less loaded eligible
// peer, so a peer that could never claim a bound judge must not be a deferral target.
func TestProviderJudgeCapabilityPeerLiveDB(t *testing.T) {
	busyClaimant := func(fx *fleetFixture) uuid.UUID {
		w := fx.jobWorker("claimant", capOf(2), false, judgeCapaCaps...)
		fx.holdActive(w, 1)
		return w
	}
	t.Run("a v1-only peer is not a deferral target for a bound judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-v1", capOf(2), false, v1OnlyCaps...)
		judge := boundJudge(fx, 0)
		got, err := providerJudgeClaim(fx, claimant, judgeCapaCaps)
		wantClaim(t, got, err, judge, "claimant with only a v1-only peer")
	})
	t.Run("a judge-capable peer is a deferral target for a bound judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-capable", capOf(2), false, judgeCapaCaps...)
		boundJudge(fx, 0)
		got, err := providerJudgeClaim(fx, claimant, judgeCapaCaps)
		wantNoClaim(t, got, err, "claimant with a capable less-loaded peer defers")
	})
	t.Run("a v1-only peer is still a deferral target for a non-judge provider run", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-v1", capOf(2), false, v1OnlyCaps...)
		run := fx.queuedRun()
		bindRunToProvider(fx, run, "peer-issue-"+uuid.NewString())
		got, err := providerJudgeClaim(fx, claimant, judgeCapaCaps)
		wantNoClaim(t, got, err, "claimant defers a bound issue run to the v1 peer")
	})
}

func claimableRow(fx *fleetFixture, run uuid.UUID) store.CountOnlineWorkersClaimableForRunRow {
	fx.t.Helper()
	now := time.Now()
	row, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID:                    run,
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		StaleRequeueCutoff:       pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		CapabilityAware:          true,
		DockerRepoAllowlist:      []uuid.UUID{},
		CodexCuratedModels:       []string{"gpt-6-sol"},
	})
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
	}
	return row
}

func satisfyingProvider(fx *fleetFixture, engine, kind string) int64 {
	fx.t.Helper()
	n, err := fx.q.CountOnlineWorkersSatisfyingProvider(fx.ctx, store.CountOnlineWorkersSatisfyingProviderParams{UserID: fx.userID, Engine: engine, Kind: kind})
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersSatisfyingProvider: %v", err)
	}
	return n
}

func TestProviderJudgeCapabilityCountsLiveDB(t *testing.T) {
	t.Run("CountOnlineWorkersClaimableForRun", func(t *testing.T) {
		fx := newFleetFixture(t)
		judge := boundJudge(fx, time.Minute)
		issue := boundIssue(fx)
		fx.jobWorker("v1-only", capOf(2), false, v1OnlyCaps...)
		if r := claimableRow(fx, judge); r.StrictEligible != 0 || r.Claimable != 0 {
			t.Fatalf("v1-only fleet, bound judge: strict=%d claimable=%d, want 0 and 0", r.StrictEligible, r.Claimable)
		}
		if r := claimableRow(fx, issue); r.StrictEligible != 1 || r.Claimable != 1 {
			t.Fatalf("v1-only fleet, bound issue run: strict=%d claimable=%d, want 1 and 1", r.StrictEligible, r.Claimable)
		}
		fx.jobWorker("judge-capable", capOf(2), false, judgeCapaCaps...)
		if r := claimableRow(fx, judge); r.StrictEligible != 1 || r.Claimable != 1 {
			t.Fatalf("with a judge-capable worker: strict=%d claimable=%d, want 1 and 1", r.StrictEligible, r.Claimable)
		}
	})
	t.Run("CountOnlineWorkersSatisfyingProvider", func(t *testing.T) {
		fx := newFleetFixture(t)
		fx.jobWorker("v1-only", capOf(2), false, v1OnlyCaps...)
		if got := satisfyingProvider(fx, "claude", "judge"); got != 0 {
			t.Fatalf("judge kind with a v1-only worker = %d, want 0", got)
		}
		for _, kind := range []string{"issue", ""} {
			if got := satisfyingProvider(fx, "claude", kind); got != 1 {
				t.Fatalf("kind %q with a v1-only worker = %d, want 1", kind, got)
			}
		}
		fx.jobWorker("judge-capable", capOf(2), false, judgeCapaCaps...)
		if got := satisfyingProvider(fx, "claude", "judge"); got != 1 {
			t.Fatalf("judge kind with a judge-capable worker = %d, want 1", got)
		}
		if got := satisfyingProvider(fx, "claude", "issue"); got != 2 {
			t.Fatalf("issue kind with both workers = %d, want 2", got)
		}
	})
}

// ListSaturationQueuedRunsForEphemeral carries the clause twice: in the "a capable worker exists"
// test and in the "a capable worker has a free slot" test.
func TestProviderJudgeCapabilitySaturationLiveDB(t *testing.T) {
	t.Run("a full v1-only worker is not a capable worker for a bound judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("v1-full", capOf(1), false, v1OnlyCaps...), 1)
		if saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s listed as saturation although no worker is capable of it", judge)
		}
		if !unplaceableRunIDs(fx)[judge] {
			t.Fatalf("judge %s not listed by the gap trigger although no worker is capable of it", judge)
		}
	})
	t.Run("a v1-only worker with a free slot does not hide saturation of the capable workers", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, judgeCapaCaps...), 1)
		fx.jobWorker("v1-free", capOf(1), false, v1OnlyCaps...)
		if !saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s not listed as saturation although the only capable worker is full", judge)
		}
	})
	t.Run("a capable worker with a free slot is not saturation", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, judgeCapaCaps...), 1)
		fx.jobWorker("capable-free", capOf(1), false, judgeCapaCaps...)
		if saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s listed as saturation although a capable worker has a free slot", judge)
		}
	})
	t.Run("a full v1-only worker still makes a bound non-judge run saturation", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		run := boundIssue(fx)
		fx.holdActive(fx.jobWorker("v1-full", capOf(1), false, v1OnlyCaps...), 1)
		if !saturationRunIDs(fx)[run] {
			t.Fatalf("bound issue run %s not listed as saturation", run)
		}
	})
}

// The three placement-function callers inherit the clause from fn_placement_admitting_workers.
func TestProviderJudgeCapabilityPlacementCallersLiveDB(t *testing.T) {
	t.Run("ListUnplaceableQueuedRunsForEphemeral", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundJudge(fx, time.Hour)
		issue := boundIssue(fx)
		fx.jobWorker("v1-only", capOf(1), false, v1OnlyCaps...)
		got := unplaceableRunIDs(fx)
		if !got[judge] {
			t.Fatalf("bound judge %s not listed although only a v1-only worker is online", judge)
		}
		if got[issue] {
			t.Fatalf("bound issue run %s listed although a v1 worker can claim it", issue)
		}
		fx.jobWorker("judge-capable", capOf(1), false, judgeCapaCaps...)
		if unplaceableRunIDs(fx)[judge] {
			t.Fatalf("bound judge %s listed although a judge-capable worker is online", judge)
		}
	})
	t.Run("ListHibernationWakeDemand matches the claim", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			protocol []string
			want     bool
		}{
			{"v1-only sleeper", v1OnlyCaps, false},
			{"judge-capable sleeper", judgeCapaCaps, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
				h := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol})
				judge := boundJudge(fx, time.Minute)
				setHibernated(fx, h, "hibernated")
				demanded := wakeDemandFor(fx, nil)[judge] == h
				if demanded != tc.want {
					t.Fatalf("demand lists the judge for the sleeper = %v, want %v", demanded, tc.want)
				}
				if claimed := claimsAfterWake(fx, h, judge, nil, "", false); claimed != demanded {
					t.Fatalf("differential: demand=%v but the awake worker's claim=%v", demanded, claimed)
				}
			})
		}
	})
	t.Run("HibernationWakeSuppresses", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			protocol []string
			want     bool
		}{
			{"v1-only waking worker", v1OnlyCaps, false},
			{"judge-capable waking worker", judgeCapaCaps, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
				w := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol})
				judge := boundJudge(fx, time.Minute)
				setHibernated(fx, w, "waking")
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, judge, w)
				suppressed, err := fx.q.HibernationWakeSuppresses(fx.ctx, store.HibernationWakeSuppressesParams{
					RunID: judge, CodexCuratedModels: []string{"gpt-6-sol"}, WakeTimeout: leaseInterval(leaseTwoHours), EphemeralLease: leaseInterval(leaseTwoHours),
					CrossCheckEvaluatedAt: planCrossCheckTime(time.Now()), CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-time.Hour)),
				})
				if err != nil || suppressed != tc.want {
					t.Fatalf("wake suppression=(%v,%v) want %v", suppressed, err, tc.want)
				}
			})
		}
	})
}

// ListStaleHibernationWakes (hibernation.sql, the CASE form): a recorded wake whose worker lacks the
// judge capability can no longer serve the bound judge.
func TestProviderJudgeCapabilityStaleWakeLiveDB(t *testing.T) {
	stale := func(fx *fleetFixture, run uuid.UUID) bool {
		fx.t.Helper()
		rows, err := fx.q.ListStaleHibernationWakes(fx.ctx, 10000)
		if err != nil {
			fx.t.Fatal(err)
		}
		for _, r := range rows {
			if r.RunID == run {
				return true
			}
		}
		return false
	}
	t.Run("a v1-only worker's wake for a bound judge is stale", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: v1OnlyCaps})
		judge := boundJudge(fx, time.Minute)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, judge, w)
		if !stale(fx, judge) {
			t.Fatal("a wake of a v1-only worker for a bound judge is not listed as stale")
		}
	})
	t.Run("a judge-capable worker's wake is kept until it loses the capability", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: judgeCapaCaps})
		judge := boundJudge(fx, time.Minute)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, judge, w)
		if stale(fx, judge) {
			t.Fatal("a wake of a judge-capable worker is listed as stale")
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, v1OnlyCaps)
		if !stale(fx, judge) {
			t.Fatal("a wake whose worker lost the judge capability is not listed as stale")
		}
	})
	t.Run("a v1-only worker's wake for a bound non-judge run is kept", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: v1OnlyCaps})
		run := boundIssue(fx)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, run, w)
		if stale(fx, run) {
			t.Fatal("a wake of a v1 worker for a bound issue run is listed as stale")
		}
	})
}

// Reap arm (g), through both statements: LockReapableEphemeralWorkers selects, DeleteLockedEphemeralWorkers
// re-checks the same predicate on a fresh snapshot.
func TestProviderJudgeCapabilityReapLiveDB(t *testing.T) {
	seed := func(t *testing.T, run func(*fleetFixture) uuid.UUID, caps []string) (*fleetFixture, uuid.UUID, uuid.UUID, time.Time) {
		fx := newFleetFixture(t)
		r := run(fx)
		w := seedEphemeralWorkerBound(fx, r)
		now := time.Now()
		setWorkerClock(fx, w, now, now)
		mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps)
		return fx, r, w, now
	}
	judgeRun := func(fx *fleetFixture) uuid.UUID { return boundJudge(fx, time.Minute) }
	reaped := func(fx *fleetFixture, now time.Time) {
		fx.t.Helper()
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, pgtype.Interval{}); err != nil {
			fx.t.Fatal(err)
		}
	}
	t.Run("a v1-only ephemeral worker of a bound judge is reaped, a judge-capable one is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, judgeRun, v1OnlyCaps)
		reaped(fx, now)
		if workerExists(fx, w) {
			t.Fatal("v1-only worker bound to a provider judge was not reaped")
		}
		fx, _, w, now = seed(t, judgeRun, judgeCapaCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("judge-capable worker bound to a provider judge was reaped")
		}
	})
	t.Run("a v1-only ephemeral worker of a bound non-judge run is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, boundIssue, v1OnlyCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("v1 worker bound to a provider issue run was reaped")
		}
	})
	t.Run("lock and delete statements each apply the clause", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps []string
			want bool
		}{{"v1-only", v1OnlyCaps, true}, {"judge-capable", judgeCapaCaps, false}} {
			t.Run(tc.name, func(t *testing.T) {
				fx, _, w, now := seed(t, judgeRun, tc.caps)
				tx, err := fx.pool.Begin(fx.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(fx.ctx) }()
				q := store.New(tx)
				cutoff := pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
				ids, err := q.LockReapableEphemeralWorkers(fx.ctx, store.LockReapableEphemeralWorkersParams{DeadlineCutoff: cutoff})
				if err != nil {
					t.Fatal(err)
				}
				locked := false
				for _, id := range ids {
					locked = locked || id == w
				}
				if locked != tc.want {
					t.Fatalf("lock statement selected the worker = %v, want %v", locked, tc.want)
				}
				n, err := q.DeleteLockedEphemeralWorkers(fx.ctx, store.DeleteLockedEphemeralWorkersParams{Ids: []uuid.UUID{w}, DeadlineCutoff: cutoff})
				wantN := int64(0)
				if tc.want {
					wantN = 1
				}
				if err != nil || n != wantN {
					t.Fatalf("delete statement removed %d (err=%v), want %d", n, err, wantN)
				}
			})
		}
	})
	t.Run("busy and custody guards still protect a v1-only worker of a bound judge", func(t *testing.T) {
		for _, protect := range []string{"busy", "custody"} {
			t.Run(protect, func(t *testing.T) {
				fx, run, w, now := seed(t, judgeRun, v1OnlyCaps)
				switch protect {
				case "busy":
					mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET worker_id=$2, status='running' WHERE id=$1", run, w)
				case "custody":
					mustExec(fx.ctx, t, fx.pool, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 SELECT $1,user_id,repo_id,id,1,'open',$2,'ident',$2,id FROM runs WHERE id=$3`, uuid.New(), w, run)
					// The hold's live_worker_id FK is ON DELETE RESTRICT: drop it before the fixture
					// cleanup (registered earlier, so it runs later) or the worker, run and user leak
					// into the shared LiveDB database.
					t.Cleanup(func() {
						mustExec(fx.ctx, t, fx.pool, "DELETE FROM recovery_custody_holds WHERE run_id=$1", run)
					})
				}
				reaped(fx, now)
				if !workerExists(fx, w) {
					t.Fatalf("%s-protected worker was reaped", protect)
				}
			})
		}
	})
}

// A direct insert of each new skip reason succeeds against the widened CHECK (00532/00533).
func TestJudgeSkipsProviderReasonsCheckLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	for _, reason := range []string{"provider_lane_unsupported", "provider_source_unusable", "provider_model_unmapped"} {
		target := fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
		n, err := fx.q.RecordJudgeSkip(fx.ctx, store.RecordJudgeSkipParams{TargetRunID: target, Harness: "claude", Reason: reason})
		if err != nil || n != 1 {
			t.Fatalf("RecordJudgeSkip(%s) = %d, %v; want 1 row", reason, n, err)
		}
		got, err := fx.q.GetJudgeSkipForTarget(fx.ctx, target)
		if err != nil || got.Reason != reason {
			t.Fatalf("GetJudgeSkipForTarget(%s) = %+v, %v", reason, got, err)
		}
	}
	target := fx.queuedRun()
	_, err := fx.q.RecordJudgeSkip(fx.ctx, store.RecordJudgeSkipParams{TargetRunID: target, Harness: "claude", Reason: "not_a_reason"})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an unknown reason must still violate the CHECK (23514), got %v", err)
	}
}
