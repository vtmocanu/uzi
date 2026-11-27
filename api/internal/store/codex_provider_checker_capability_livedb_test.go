package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4574 M2: a provider-bound Codex plan checker is claimable and placeable only on a worker
// advertising codex_provider_checker_v1 in addition to the full Codex provider set and
// cross_check_v1. Every test seeds the checker child directly (kind 'cross_check', Codex harness,
// the five model_provider_* columns set via bindCodexPlacementRun, a pending plan check row); one
// worker carries only the provider set and one also carries the checker capability, so the checker
// capability is the ONLY variable. The vendor Codex checker child is the control proving the clause
// is scoped to a provider-bound child. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (e2e/run-store-it.sh).

const codexProviderCheckerCap = "codex_provider_checker_v1"

var (
	// cpcBaseCaps is the Codex provider set plus cross_check_v1, without the checker capability.
	cpcBaseCaps = []string{"cross_check_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1"}
	// cpcCheckerCaps is cpcBaseCaps plus the checker capability.
	cpcCheckerCaps = append(append([]string{}, cpcBaseCaps...), codexProviderCheckerCap)
)

// boundCodexChecker seeds a queued Codex plan-checker child of a Claude lead running on its own
// worker. bound stamps the provider binding; unbound is the vendor-credential control.
func boundCodexChecker(fx *fleetFixture, bound bool) uuid.UUID {
	fx.t.Helper()
	leadWorker := fx.worker("lead-host", nil, false)
	lead, child := fx.queuedRun(), fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status='running',worker_id=$2,claim_generation=1,harness='claude',
		auto_approve=true,plan_cross_check_required=true WHERE id=$1`, lead, leadWorker)
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,harness='codex',
		report_only=true,budget_wall_seconds=1800,required_capabilities='{}',required_tools='{}',
		status_since=now()-interval '1 hour' WHERE id=$1`, child, lead)
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
		(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
		 checker_harness,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
		VALUES ($1,$2,'plan',1,1,'plan','[]','s',repeat('a',40),$3,'codex',now()+interval '5 minutes',false,0)`,
		lead, child, []byte("digest"))
	if bound {
		bindCodexPlacementRun(fx, child)
	}
	return child
}

func checkerClaim(fx *fleetFixture, worker uuid.UUID, caps []string) (store.Run, error) {
	now := time.Now()
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(worker), UserID: fx.userID,
		CodexCuratedModels:       []string{"gpt-6-sol"},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		WorkerCaps:               []string{}, WorkerProtocolCaps: caps, CapabilityAware: false})
}

// ClaimRun (runtime.sql, the claimant's provider clause).
func TestCodexProviderCheckerCapabilityClaimRunLiveDB(t *testing.T) {
	t.Run("a provider-only worker cannot claim a bound Codex checker; a checker-capable worker can", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
		child := boundCodexChecker(fx, true)
		base := fx.jobWorker("provider-only", capOf(4), false, cpcBaseCaps...)
		got, err := checkerClaim(fx, base, cpcBaseCaps)
		wantNoClaim(t, got, err, "provider-only worker, bound Codex checker")
		capable := fx.jobWorker("checker-capable", capOf(4), false, cpcCheckerCaps...)
		got, err = checkerClaim(fx, capable, cpcCheckerCaps)
		wantClaim(t, got, err, child, "checker-capable worker, bound Codex checker")
		if got.Kind != "cross_check" {
			t.Fatalf("claimed kind %q, want cross_check", got.Kind)
		}
	})
	t.Run("a vendor Codex checker is still claimable by a provider-only worker", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
		child := boundCodexChecker(fx, false)
		base := fx.jobWorker("provider-only", capOf(4), false, cpcBaseCaps...)
		got, err := checkerClaim(fx, base, cpcBaseCaps)
		wantClaim(t, got, err, child, "provider-only worker, vendor Codex checker")
	})
}

// The fleet-spread peer mirror: a busy claimant defers to a strictly less loaded eligible peer, so a
// peer that could never claim a bound Codex checker must not be a deferral target.
func TestCodexProviderCheckerCapabilityPeerLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		peerCaps     []string
		bound        bool
		wantDeferred bool
	}{
		{"a provider-only peer is not a deferral target for a bound Codex checker", cpcBaseCaps, true, false},
		{"a checker-capable peer is a deferral target for a bound Codex checker", cpcCheckerCaps, true, true},
		{"a provider-only peer is a deferral target for a vendor Codex checker", cpcBaseCaps, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			claimant := fx.jobWorker("claimant", capOf(2), false, cpcCheckerCaps...)
			fx.holdActive(claimant, 1)
			fx.jobWorker("peer", capOf(2), false, tc.peerCaps...)
			child := boundCodexChecker(fx, tc.bound)
			got, err := checkerClaim(fx, claimant, cpcCheckerCaps)
			if tc.wantDeferred {
				wantNoClaim(t, got, err, "claimant defers to the eligible less-loaded peer")
				return
			}
			wantClaim(t, got, err, child, "claimant with only an ineligible peer")
		})
	}
}

func TestCodexProviderCheckerCapabilityCountsLiveDB(t *testing.T) {
	t.Run("CountOnlineWorkersClaimableForRun", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
		bound, vendor := boundCodexChecker(fx, true), boundCodexChecker(fx, false)
		fx.jobWorker("provider-only", capOf(2), false, cpcBaseCaps...)
		if r := claimableRow(fx, bound); r.StrictEligible != 0 || r.Claimable != 0 {
			t.Fatalf("provider-only fleet, bound Codex checker: strict=%d claimable=%d, want 0 and 0", r.StrictEligible, r.Claimable)
		}
		if r := claimableRow(fx, vendor); r.Claimable != 1 {
			t.Fatalf("provider-only fleet, vendor Codex checker: claimable=%d, want 1", r.Claimable)
		}
		fx.jobWorker("checker-capable", capOf(2), false, cpcCheckerCaps...)
		if r := claimableRow(fx, bound); r.Claimable != 1 {
			t.Fatalf("with a checker-capable worker: claimable=%d, want 1", r.Claimable)
		}
	})
	t.Run("CountOnlineWorkersSatisfyingProvider", func(t *testing.T) {
		fx := newFleetFixture(t)
		fx.jobWorker("provider-only", capOf(2), false, cpcBaseCaps...)
		if got := satisfyingProvider(fx, "codex", "cross_check"); got != 0 {
			t.Fatalf("cross_check kind with a provider-only worker = %d, want 0", got)
		}
		if got := satisfyingProvider(fx, "codex", "issue"); got != 1 {
			t.Fatalf("issue kind with a provider-only worker = %d, want 1", got)
		}
		fx.jobWorker("checker-capable", capOf(2), false, cpcCheckerCaps...)
		if got := satisfyingProvider(fx, "codex", "cross_check"); got != 1 {
			t.Fatalf("cross_check kind with a checker-capable worker = %d, want 1", got)
		}
	})
}

// Placement: the capability-gap trigger (fn_placement_admitting_workers) and the saturation trigger
// (ListSaturationQueuedRunsForEphemeral's two copies).
func TestCodexProviderCheckerCapabilityPlacementLiveDB(t *testing.T) {
	setup := func(t *testing.T) *fleetFixture {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		t.Cleanup(func() {
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
			mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID)
		})
		return fx
	}
	iv := leaseInterval(leaseTwoHours)
	t.Run("gap trigger", func(t *testing.T) {
		fx := setup(t)
		bound, vendor := boundCodexChecker(fx, true), boundCodexChecker(fx, false)
		fx.jobWorker("provider-only", capOf(1), false, cpcBaseCaps...)
		got := listedUnplaceable(fx, 1000, iv)
		if !got[bound] {
			t.Fatalf("bound Codex checker %s not listed although only a provider-only worker is online", bound)
		}
		if got[vendor] {
			t.Fatalf("vendor Codex checker %s listed although a provider-only worker can claim it", vendor)
		}
		fx.jobWorker("checker-capable", capOf(1), false, cpcCheckerCaps...)
		if listedUnplaceable(fx, 1000, iv)[bound] {
			t.Fatalf("bound Codex checker %s listed although a checker-capable worker is online", bound)
		}
	})
	t.Run("saturation trigger: a free provider-only worker does not hide the full capable one", func(t *testing.T) {
		fx := setup(t)
		bound := boundCodexChecker(fx, true)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, cpcCheckerCaps...), 1)
		fx.jobWorker("base-free", capOf(1), false, cpcBaseCaps...)
		if !listedSaturation(fx, 1000, iv)[bound] {
			t.Fatalf("bound Codex checker %s not listed as saturation although the only capable worker is full", bound)
		}
	})
	t.Run("saturation trigger: a free capable worker is not saturation", func(t *testing.T) {
		fx := setup(t)
		bound := boundCodexChecker(fx, true)
		fx.holdActive(fx.jobWorker("capable-full", capOf(1), false, cpcCheckerCaps...), 1)
		fx.jobWorker("capable-free", capOf(1), false, cpcCheckerCaps...)
		if listedSaturation(fx, 1000, iv)[bound] {
			t.Fatalf("bound Codex checker %s listed as saturation although a capable worker has a free slot", bound)
		}
	})
}

// Wake: ListHibernationWakeDemand matches the claim, and ListStaleHibernationWakes drops a wake
// whose worker lacks the capability.
func TestCodexProviderCheckerCapabilityWakeLiveDB(t *testing.T) {
	one := int32(1)
	laneProtocol := func(caps []string) []string {
		return append([]string{"cross_check_lane_v1"}, caps...)
	}
	for _, tc := range []struct {
		name     string
		protocol []string
		want     bool
	}{
		{"provider-only sleeper", laneProtocol(cpcBaseCaps), false},
		{"checker-capable sleeper", laneProtocol(cpcCheckerCaps), true},
	} {
		t.Run("demand/"+tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
			h := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol, crossSlots: &one})
			child := boundCodexChecker(fx, true)
			setHibernated(fx, h, "hibernated")
			demanded := wakeDemandFor(fx, nil)[child] == h
			if demanded != tc.want {
				t.Fatalf("demand lists the bound Codex checker for the sleeper = %v, want %v", demanded, tc.want)
			}
			if claimed := claimsAfterWake(fx, h, child, nil, "cross_check", false); claimed != demanded {
				t.Fatalf("differential: demand=%v but the awake worker's claim=%v", demanded, claimed)
			}
		})
		t.Run("suppress/"+tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
			w := hostedWorker(fx, wakeWorkerOpts{protocol: tc.protocol, crossSlots: &one})
			child := boundCodexChecker(fx, true)
			setHibernated(fx, w, "waking")
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, child, w)
			suppressed, err := fx.q.HibernationWakeSuppresses(fx.ctx, store.HibernationWakeSuppressesParams{
				RunID: child, CodexCuratedModels: []string{"gpt-6-sol"}, WakeTimeout: leaseInterval(leaseTwoHours), EphemeralLease: leaseInterval(leaseTwoHours),
				CrossCheckEvaluatedAt: planCrossCheckTime(time.Now()), CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-time.Hour)),
			})
			if err != nil || suppressed != tc.want {
				t.Fatalf("wake suppression=(%v,%v) want %v", suppressed, err, tc.want)
			}
		})
	}
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
	t.Run("a provider-only worker's wake for a bound Codex checker is stale until it gains the capability", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: cpcBaseCaps})
		child := boundCodexChecker(fx, true)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, child, w)
		if !stale(fx, child) {
			t.Fatal("a wake of a provider-only worker for a bound Codex checker is not listed as stale")
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, cpcCheckerCaps)
		if stale(fx, child) {
			t.Fatal("a wake of a checker-capable worker is listed as stale")
		}
	})
	t.Run("a provider-only worker's wake for a vendor Codex checker is kept", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		w := hostedWorker(fx, wakeWorkerOpts{protocol: cpcBaseCaps})
		child := boundCodexChecker(fx, false)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, child, w)
		if stale(fx, child) {
			t.Fatal("a wake of a provider-only worker for a vendor Codex checker is listed as stale")
		}
	})
}

// Reap arm (g), through both statements: LockReapableEphemeralWorkers selects, DeleteLockedEphemeralWorkers
// re-checks the same predicate on a fresh snapshot.
func TestCodexProviderCheckerCapabilityReapLiveDB(t *testing.T) {
	seed := func(t *testing.T, bound bool, caps []string) (*fleetFixture, uuid.UUID, uuid.UUID, time.Time) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
		r := boundCodexChecker(fx, bound)
		w := seedEphemeralWorkerBound(fx, r)
		now := time.Now()
		setWorkerClock(fx, w, now, now)
		mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps)
		return fx, r, w, now
	}
	reaped := func(fx *fleetFixture, now time.Time) {
		fx.t.Helper()
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, pgtype.Interval{}); err != nil {
			fx.t.Fatal(err)
		}
	}
	t.Run("a provider-only ephemeral worker of a bound Codex checker is reaped, a capable one is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, true, cpcBaseCaps)
		reaped(fx, now)
		if workerExists(fx, w) {
			t.Fatal("provider-only worker bound to a Codex provider checker was not reaped")
		}
		fx, _, w, now = seed(t, true, cpcCheckerCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("checker-capable worker bound to a Codex provider checker was reaped")
		}
	})
	t.Run("a provider-only ephemeral worker of a vendor Codex checker is kept", func(t *testing.T) {
		fx, _, w, now := seed(t, false, cpcBaseCaps)
		reaped(fx, now)
		if !workerExists(fx, w) {
			t.Fatal("provider-only worker bound to a vendor Codex checker was reaped")
		}
	})
	t.Run("lock and delete statements each apply the clause", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps []string
			want bool
		}{{"provider-only", cpcBaseCaps, true}, {"checker-capable", cpcCheckerCaps, false}} {
			t.Run(tc.name, func(t *testing.T) {
				fx, _, w, now := seed(t, true, tc.caps)
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
	t.Run("busy and custody guards still protect a provider-only worker of a bound Codex checker", func(t *testing.T) {
		for _, protect := range []string{"busy", "custody"} {
			t.Run(protect, func(t *testing.T) {
				fx, run, w, now := seed(t, true, cpcBaseCaps)
				switch protect {
				case "busy":
					mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET worker_id=$2, status='running' WHERE id=$1", run, w)
				case "custody":
					mustExec(fx.ctx, t, fx.pool, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 SELECT $1,user_id,repo_id,id,1,'open',$2,'ident',$2,id FROM runs WHERE id=$3`, uuid.New(), w, run)
					// The hold's live_worker_id FK is ON DELETE RESTRICT: drop it before the fixture
					// cleanup (registered earlier, so it runs later) or the worker, run and user leak.
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
