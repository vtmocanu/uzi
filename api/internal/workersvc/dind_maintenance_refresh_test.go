package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestDindMaintenanceAtomicRefreshFenceLiveDB(t *testing.T) {
	env, w, svc, op := dindLiveFixture(t)
	fresh := uuid.NewString()
	if _, err := env.q.RegisterWorker(env.ctx, store.RegisterWorkerParams{ID: w.ID, ProtocolCapabilities: []string{capability.DindMaintenanceV1}, SnapshotRegisterNonce: pgconv.TextOrNull(fresh)}); err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE workers SET dind_register_floor=now()-interval '1 minute',dind_meter_at=date_trunc('second',now())-interval '1 second',dind_pressure_streak=2 WHERE id=$1`, w.ID)
	op.Phase = "ready"
	op.DeploymentUID = "replacement-deployment"
	run := dindLiveRun(t, env, w, "issue", "paused", true)
	if _, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("busy refresh accepted: %v", err)
	}
	current, err := env.q.GetWorkerByID(env.ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.MaintenanceDeploymentUid != w.MaintenanceDeploymentUid || current.MaintenanceRegisterNonce != w.MaintenanceRegisterNonce || current.MaintenancePhase != "requested" {
		t.Fatal("failed refresh lost legacy scheduling binding")
	}
	env.exec("UPDATE runs SET status='completed' WHERE id=$1", run)
	got, err := svc.TransitionDindMaintenance(env.ctx, w.ID, op)
	if err != nil || got == nil || got.Phase != "ready" || !got.Fenced || got.ReadyACK || got.Nonce == op.Nonce || got.RegisterNonce != fresh || got.DeploymentUID != op.DeploymentUID {
		t.Fatalf("atomic replacement fence: %+v %v", got, err)
	}
}

func TestDindMaintenanceAtomicRefreshReadiness(t *testing.T) {
	for _, busy := range []bool{false, true} {
		w, now := maintenanceFixture()
		w.MaintenancePhase = "requested"
		w.SnapshotRegisterNonce = pgconv.TextOrNull("replacement")
		tx := &maintenanceTestTx{worker: w}
		if busy {
			tx.nonterminal = 1
		}
		op := *DindMaintenanceFromWorker(w)
		op.Phase = "ready"
		op.DeploymentUID = "replacement-deployment"
		got, err := maintenanceService(tx, now.Add(time.Second)).TransitionDindMaintenance(context.Background(), w.ID, op)
		if busy {
			if !errors.Is(err, ErrDindMaintenanceConflict) || tx.committed || len(tx.writeArgs) != 0 {
				t.Fatalf("busy refresh did not fail closed: %+v %v", got, err)
			}
		} else if err != nil || !tx.committed {
			t.Fatalf("idle refresh: %+v %v", got, err)
		}
	}
}
