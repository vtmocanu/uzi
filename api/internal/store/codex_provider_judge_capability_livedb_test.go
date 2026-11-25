package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4574 M1: a provider-bound Codex judge is claimable and placeable only on a worker advertising
// codex_provider_judge_v1 in addition to the full Codex provider set (codex_harness_v1,
// codex_runtime_v2, codex_runtime_v3, codex_provider_v1). Every test seeds judge rows directly (kind
// 'judge', target_run_id set, the provider binding set, harness codex); one worker carries only the
// provider set and one also carries the judge capability, so the judge capability is the ONLY variable.
// The non-judge Codex provider run is the control proving the clause is scoped to a bound judge.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).

const codexProviderJudgeCap = "codex_provider_judge_v1"

var (
	// cpjBaseCaps is the full Codex provider trio plus codex_provider_v1, without the judge capability.
	cpjBaseCaps = []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1"}
	// cpjJudgeCaps is cpjBaseCaps plus the judge capability.
	cpjJudgeCaps = []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1", codexProviderJudgeCap}
)

// boundCodexJudge seeds a Codex provider-bound judge of a fresh cancelled target. age sets
// created_at, so a background judge older than the grace cutoff counts as demand for the ephemeral
// triggers.
func boundCodexJudge(fx *fleetFixture, age time.Duration) uuid.UUID {
	fx.t.Helper()
	target := fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
	judge := fx.insertJudge(nil, target, time.Now().Add(-age))
	bindCodexPlacementRun(fx, judge)
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, judge)
	return judge
}

// boundCodexIssue seeds a Codex provider-bound non-judge run, the control a judge-scoped clause must
// not touch.
func boundCodexIssue(fx *fleetFixture) uuid.UUID {
	fx.t.Helper()
	id := fx.queuedRun()
	bindCodexPlacementRun(fx, id)
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, id)
	return id
}

// ClaimRun (runtime.sql, the claimant's provider clause).
func TestCodexProviderJudgeCapabilityClaimRunLiveDB(t *testing.T) {
	t.Run("a provider-only worker cannot claim a bound Codex judge; a judge-capable worker can", func(t *testing.T) {
		fx := newFleetFixture(t)
		judge := boundCodexJudge(fx, time.Minute)
		base := fx.jobWorker("provider-only", capOf(2), false, cpjBaseCaps...)
		got, err := providerJudgeClaim(fx, base, cpjBaseCaps)
		wantNoClaim(t, got, err, "provider-only worker, bound Codex judge")
		capable := fx.jobWorker("judge-capable", capOf(2), false, cpjJudgeCaps...)
		got, err = providerJudgeClaim(fx, capable, cpjJudgeCaps)
		wantClaim(t, got, err, judge, "judge-capable worker, bound Codex judge")
		if got.Kind != "judge" {
			t.Fatalf("claimed kind %q, want judge", got.Kind)
		}
	})
	t.Run("a non-judge Codex provider run is still claimable by a provider-only worker", func(t *testing.T) {
		fx := newFleetFixture(t)
		run := boundCodexIssue(fx)
		base := fx.jobWorker("provider-only", capOf(2), false, cpjBaseCaps...)
		got, err := providerJudgeClaim(fx, base, cpjBaseCaps)
		wantClaim(t, got, err, run, "provider-only worker, bound Codex issue run")
	})
}

// The fleet-spread peer mirror: a busy claimant defers a fresh run to a strictly less loaded eligible
// peer, so a peer that could never claim a bound Codex judge must not be a deferral target.
func TestCodexProviderJudgeCapabilityPeerLiveDB(t *testing.T) {
	busyClaimant := func(fx *fleetFixture) uuid.UUID {
		w := fx.jobWorker("claimant", capOf(2), false, cpjJudgeCaps...)
		fx.holdActive(w, 1)
		return w
	}
	t.Run("a provider-only peer is not a deferral target for a bound Codex judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-base", capOf(2), false, cpjBaseCaps...)
		judge := boundCodexJudge(fx, 0)
		got, err := providerJudgeClaim(fx, claimant, cpjJudgeCaps)
		wantClaim(t, got, err, judge, "claimant with only a provider-only peer")
	})
	t.Run("a judge-capable peer is a deferral target for a bound Codex judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-capable", capOf(2), false, cpjJudgeCaps...)
		boundCodexJudge(fx, 0)
		got, err := providerJudgeClaim(fx, claimant, cpjJudgeCaps)
		wantNoClaim(t, got, err, "claimant with a capable less-loaded peer defers")
	})
	t.Run("a provider-only peer is still a deferral target for a non-judge Codex provider run", func(t *testing.T) {
		fx := newFleetFixture(t)
		claimant := busyClaimant(fx)
		fx.jobWorker("peer-base", capOf(2), false, cpjBaseCaps...)
		boundCodexIssue(fx)
		got, err := providerJudgeClaim(fx, claimant, cpjJudgeCaps)
		wantNoClaim(t, got, err, "claimant defers a bound Codex issue run to the provider-only peer")
	})
}

func TestCodexProviderJudgeCapabilityCountsLiveDB(t *testing.T) {
	t.Run("CountOnlineWorkersClaimableForRun", func(t *testing.T) {
		fx := newFleetFixture(t)
		judge := boundCodexJudge(fx, time.Minute)
		issue := boundCodexIssue(fx)
		fx.jobWorker("provider-only", capOf(2), false, cpjBaseCaps...)
		if r := claimableRow(fx, judge); r.StrictEligible != 0 || r.Claimable != 0 {
			t.Fatalf("provider-only fleet, bound Codex judge: strict=%d claimable=%d, want 0 and 0", r.StrictEligible, r.Claimable)
		}
		if r := claimableRow(fx, issue); r.StrictEligible != 1 || r.Claimable != 1 {
			t.Fatalf("provider-only fleet, bound Codex issue run: strict=%d claimable=%d, want 1 and 1", r.StrictEligible, r.Claimable)
		}
		fx.jobWorker("judge-capable", capOf(2), false, cpjJudgeCaps...)
		if r := claimableRow(fx, judge); r.StrictEligible != 1 || r.Claimable != 1 {
			t.Fatalf("with a judge-capable worker: strict=%d claimable=%d, want 1 and 1", r.StrictEligible, r.Claimable)
		}
	})
	t.Run("CountOnlineWorkersSatisfyingProvider", func(t *testing.T) {
		fx := newFleetFixture(t)
		fx.jobWorker("provider-only", capOf(2), false, cpjBaseCaps...)
		if got := satisfyingProvider(fx, "codex", "judge"); got != 0 {
			t.Fatalf("judge kind with a provider-only worker = %d, want 0", got)
		}
		for _, kind := range []string{"issue", ""} {
			if got := satisfyingProvider(fx, "codex", kind); got != 1 {
				t.Fatalf("kind %q with a provider-only worker = %d, want 1", kind, got)
			}
		}
		fx.jobWorker("judge-capable", capOf(2), false, cpjJudgeCaps...)
		if got := satisfyingProvider(fx, "codex", "judge"); got != 1 {
			t.Fatalf("judge kind with a judge-capable worker = %d, want 1", got)
		}
		if got := satisfyingProvider(fx, "codex", "issue"); got != 2 {
			t.Fatalf("issue kind with both workers = %d, want 2", got)
		}
	})
}

// ListSaturationQueuedRunsForEphemeral carries the clause twice: in the "a capable worker exists"
// test and in the "a capable worker has a free slot" test.
func TestCodexProviderJudgeCapabilitySaturationLiveDB(t *testing.T) {
	t.Run("a full provider-only worker is not a capable worker for a bound Codex judge", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundCodexJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("base-full", capOf(1), false, cpjBaseCaps...), 1)
		if saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s listed as saturation although no worker is capable of it", judge)
		}
		if !unplaceableRunIDs(fx)[judge] {
			t.Fatalf("judge %s not listed by the gap trigger although no worker is capable of it", judge)
		}
	})
	t.Run("a provider-only worker with a free slot does not hide saturation of the capable workers", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundCodexJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, cpjJudgeCaps...), 1)
		fx.jobWorker("base-free", capOf(1), false, cpjBaseCaps...)
		if !saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s not listed as saturation although the only capable worker is full", judge)
		}
	})
	t.Run("a capable worker with a free slot is not saturation", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundCodexJudge(fx, time.Hour)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, cpjJudgeCaps...), 1)
		fx.jobWorker("capable-free", capOf(1), false, cpjJudgeCaps...)
		if saturationRunIDs(fx)[judge] {
			t.Fatalf("judge %s listed as saturation although a capable worker has a free slot", judge)
		}
	})
	t.Run("a full provider-only worker still makes a bound non-judge Codex run saturation", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		run := boundCodexIssue(fx)
		fx.holdActive(fx.jobWorker("base-full", capOf(1), false, cpjBaseCaps...), 1)
		if !saturationRunIDs(fx)[run] {
			t.Fatalf("bound Codex issue run %s not listed as saturation", run)
		}
	})
}

// The placement-function callers inherit the clause from fn_placement_admitting_workers.
func TestCodexProviderJudgeCapabilityPlacementCallersLiveDB(t *testing.T) {
	t.Run("ListUnplaceableQueuedRunsForEphemeral", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		judge := boundCodexJudge(fx, time.Hour)
		issue := boundCodexIssue(fx)
		fx.jobWorker("provider-only", capOf(1), false, cpjBaseCaps...)
		got := unplaceableRunIDs(fx)
		if !got[judge] {
			t.Fatalf("bound Codex judge %s not listed although only a provider-only worker is online", judge)
		}
		if got[issue] {
			t.Fatalf("bound Codex issue run %s listed although a provider-only worker can claim it", issue)
		}
		fx.jobWorker("judge-capable", capOf(1), false, cpjJudgeCaps...)
		if unplaceableRunIDs(fx)[judge] {
			t.Fatalf("bound Codex judge %s listed although a judge-capable worker is online", judge)
		}
	})
	t.Run("ListHibernationWakeDemand matches the claim", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			protocol []string
			want     bool
		}{
			{"provider-only sleeper", cpjBaseCaps, false},
			{"judge-capable sleeper", cpjJudgeCaps, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
				h := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol})
				judge := boundCodexJudge(fx, time.Minute)
				setHibernated(fx, h, "hibernated")
				demanded := wakeDemandFor(fx, nil)[judge] == h
				if demanded != tc.want {
					t.Fatalf("demand lists the Codex judge for the sleeper = %v, want %v", demanded, tc.want)
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
			{"provider-only waking worker", cpjBaseCaps, false},
			{"judge-capable waking worker", cpjJudgeCaps, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
				w := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol})
				judge := boundCodexJudge(fx, time.Minute)
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
// Codex judge capability can no longer serve the bound Codex judge.
func TestCodexProviderJudgeCapabilityStaleWakeLiveDB(t *testing.T) {
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
	t.Run("a provider-only worker's wake for a bound Codex judge is stale", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: cpjBaseCaps})
		judge := boundCodexJudge(fx, time.Minute)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, judge, w)
		if !stale(fx, judge) {
			t.Fatal("a wake of a provider-only worker for a bound Codex judge is not listed as stale")
		}
	})
	t.Run("a judge-capable worker's wake is kept until it loses the capability", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: cpjJudgeCaps})
		judge := boundCodexJudge(fx, time.Minute)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, judge, w)
		if stale(fx, judge) {
			t.Fatal("a wake of a judge-capable worker is listed as stale")
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, cpjBaseCaps)
		if !stale(fx, judge) {
			t.Fatal("a wake whose worker lost the Codex judge capability is not listed as stale")
		}
	})
	t.Run("a provider-only worker's wake for a bound non-judge Codex run is kept", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: cpjBaseCaps})
		run := boundCodexIssue(fx)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, run, w)
		if stale(fx, run) {
			t.Fatal("a wake of a provider-only worker for a bound Codex issue run is listed as stale")
		}
	})
}

// Reap arm (g), through both statements: LockReapableEphemeralWorkers selects, DeleteLockedEphemeralWorkers
// re-checks the same predicate on a fresh snapshot.
func TestCodexProviderJudgeCapabilityReapLiveDB(t *testing.T) {
	seed := func(t *testing.T, run func(*fleetFixture) uuid.UUID, caps []string) (*fleetFixture, uuid.UUID, uuid.UUID, time.Time) {
		fx := newFleetFixture(t)
		r := run(fx)
		w := seedEphemeralWorkerBound(fx, r)
		now := time.Now()
		setWorkerClock(fx, w, now, now)
		mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps)
		return fx, r, w, now
	}
	judgeRun := func(fx *fleetFixture) uuid.UUID { return boundCodexJudge(fx, time.Minute) }
	reaped := func(fx *fleetFixture, now time.Time) {
		fx.t.Helper()
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, pgtype.Interval{}); err != nil {
			fx.t.Fatal(err)
		}
	}
	t.Run("a provider-only ephemeral worker of a bound Codex judge is reaped, a judge-capable one is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, judgeRun, cpjBaseCaps)
		reaped(fx, now)
		if workerExists(fx, w) {
			t.Fatal("provider-only worker bound to a Codex provider judge was not reaped")
		}
		fx, _, w, now = seed(t, judgeRun, cpjJudgeCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("judge-capable worker bound to a Codex provider judge was reaped")
		}
	})
	t.Run("a provider-only ephemeral worker of a bound non-judge Codex run is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, boundCodexIssue, cpjBaseCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("provider-only worker bound to a Codex provider issue run was reaped")
		}
	})
	t.Run("lock and delete statements each apply the clause", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps []string
			want bool
		}{{"provider-only", cpjBaseCaps, true}, {"judge-capable", cpjJudgeCaps, false}} {
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
	t.Run("busy and custody guards still protect a provider-only worker of a bound Codex judge", func(t *testing.T) {
		for _, protect := range []string{"busy", "custody"} {
			t.Run(protect, func(t *testing.T) {
				fx, run, w, now := seed(t, judgeRun, cpjBaseCaps)
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
