package store_test

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var runtimeV3OldProtocols = []string{"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1", "codex_phase_models_v1"}

func runtimeV3Protocols() []string {
	return append(append([]string{}, runtimeV3OldProtocols...), "codex_runtime_v3")
}

// Only the capable-EXISTS arm can change this result: the sole worker is full.
func TestCodexRuntimeV3SaturationCapableArmLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	w := phaseWorkerRow(fx, "busy-old", capOf(1), runtimeV3OldProtocols)
	fx.holdActive(w, 1)
	run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex"})
	if phaseListedSaturation(fx, leaseInterval(leaseTwoHours))[run] {
		t.Fatal("capable EXISTS admitted v2-only saturated worker")
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, runtimeV3Protocols())
	if !phaseListedSaturation(fx, leaseInterval(leaseTwoHours))[run] {
		t.Fatal("capable EXISTS rejected same saturated worker upgraded to v3")
	}
}

// The busy current worker keeps EXISTS true; only the free-slot NOT EXISTS arm
// can incorrectly suppress provisioning because of the idle old worker.
func TestCodexRuntimeV3SaturationFreeSlotArmLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	busy := phaseWorkerRow(fx, "busy-current", capOf(1), runtimeV3Protocols())
	fx.holdActive(busy, 1)
	idle := phaseWorkerRow(fx, "idle-old", capOf(1), runtimeV3OldProtocols)
	run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex"})
	if !phaseListedSaturation(fx, leaseInterval(leaseTwoHours))[run] {
		t.Fatal("free-slot arm treated idle v2-only worker as current placement")
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", idle, runtimeV3Protocols())
	if phaseListedSaturation(fx, leaseInterval(leaseTwoHours))[run] {
		t.Fatal("free-slot arm ignored same idle worker upgraded to v3")
	}
}

func TestCodexRuntimeV3HibernationDemandLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex", noCaps: true})
	w := hostedWorker(fx, wakeWorkerOpts{protocol: runtimeV3OldProtocols})
	setHibernated(fx, w, "hibernated")
	if got := wakeDemandFor(fx, nil)[run]; got != uuid.Nil {
		t.Fatalf("v2-only wake target=%s", got)
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, runtimeV3Protocols())
	if got := wakeDemandFor(fx, nil)[run]; got != w {
		t.Fatalf("v3 wake target=%s want=%s", got, w)
	}
}

func TestCodexRuntimeV3HibernationSuppressionLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex", noCaps: true})
	w := hostedWorker(fx, wakeWorkerOpts{protocol: runtimeV3OldProtocols})
	setHibernated(fx, w, "waking")
	mustExec(fx.ctx, t, fx.pool, "INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())", run, w)
	check := func(want bool) {
		t.Helper()
		got, err := fx.q.HibernationWakeSuppresses(fx.ctx, store.HibernationWakeSuppressesParams{
			RunID: run, CodexCuratedModels: []string{"gpt-6-sol"}, WakeTimeout: leaseInterval(leaseTwoHours), EphemeralLease: leaseInterval(leaseTwoHours),
			CrossCheckEvaluatedAt: planCrossCheckTime(time.Now()), CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-time.Hour)),
		})
		if err != nil || got != want {
			t.Fatalf("runtime wake suppression=%v want=%v err=%v", got, want, err)
		}
	}
	check(false)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, runtimeV3Protocols())
	check(true)
}

func TestCodexRuntimeV3LeaseNoBypassLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := seedLeasedWorker(fx, leaseRun{iid: i64p(199)}, 0)
	run := insertLeaseRun(fx, leaseRun{iid: i64p(199)})
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET harness='codex' WHERE id=$1", run)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.id, runtimeV3OldProtocols)
	claim := func(protocol []string) (store.Run, error) {
		return claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now(), func(p *store.ClaimRunParams) {
			p.WorkerProtocolCaps = protocol
			p.CapabilityAware = false
			p.CodexCuratedModels = []string{"gpt-6-sol"}
		})
	}
	if _, err := claim(runtimeV3OldProtocols); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease bypassed v3 with capability checking off: %v", err)
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='awaiting_approval',required_capabilities='{gpu}' WHERE id=$1", run)
	n, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: run, UserID: fx.userID})
	if err != nil || n != 1 {
		t.Fatalf("owner clear=(%d,%v)", n, err)
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='queued' WHERE id=$1", run)
	if _, err := claim(runtimeV3OldProtocols); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("owner clear bypassed leased runtime: %v", err)
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.id, runtimeV3Protocols())
	got, err := claim(runtimeV3Protocols())
	wantClaimed(t, got, err, run)
}

func TestCodexRuntimeV3PlacementGapLeaseLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	w := seedLeasedWorker(fx, leaseRun{iid: i64p(200)}, 0)
	run := insertLeaseRun(fx, leaseRun{iid: i64p(200)})
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET harness='codex',required_capabilities='{docker}',status_since=now()-interval '1 hour' WHERE id=$1", run)
	check := func(want bool) {
		t.Helper()
		if got := phaseListedUnplaceable(fx, leaseInterval(leaseTwoHours))[run]; got != want {
			t.Fatalf("leased placement gap=%v want=%v", got, want)
		}
	}
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.id, runtimeV3OldProtocols)
	check(true)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.id, runtimeV3Protocols())
	check(false)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET lease_since=now()-interval '3 hours' WHERE id=$1", w.id)
	check(true)
}

func TestCodexRuntimeV3PlacementGapLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)
	w := phaseWorkerRow(fx, "placement", capOf(1), runtimeV3OldProtocols)
	run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex"})
	check := func(want bool) {
		t.Helper()
		if got := phaseListedUnplaceable(fx, leaseInterval(leaseTwoHours))[run]; got != want {
			t.Fatalf("placement gap=%v want=%v", got, want)
		}
	}
	check(true)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, runtimeV3Protocols())
	check(false)
	// Current runtime does not waive other capability or planning contracts.
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET required_capabilities='{gpu}' WHERE id=$1", run)
	check(true)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET required_capabilities='{docker}' WHERE id=$1", run)
	mustExec(fx.ctx, t, fx.pool, "UPDATE users SET planning_codex_model='gpt-6-sol' WHERE id=$1", fx.userID)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'codex_phase_models_v1') WHERE id=$1", w)
	check(true)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, runtimeV3Protocols())
	check(false)
}
