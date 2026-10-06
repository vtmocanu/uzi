package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

func TestDindMaintenanceCompletionReadiness(t *testing.T) {
	for _, name := range []string{
		"ready", "stopping", "old_registration", "missing_nonce", "empty_nonce",
		"missing_registration_floor", "registration_only", "pre_registration_heartbeat",
		"missing_heartbeat", "stale_heartbeat", "future_heartbeat", "missing_capability",
		"offline", "unfenced", "parked_busy", "custody",
	} {
		t.Run(name, func(t *testing.T) {
			w, now := maintenanceFixture()
			w.MaintenancePhase = "recycling"
			w.Status = "online"
			w.SnapshotRegisterNonce = pgconv.TextOrNull("replacement")
			w.DindRegisterFloor = pgconv.Time(now.Add(-2 * time.Second))
			w.LastHeartbeatAt = pgconv.Time(now.Add(-time.Second))
			tx := &maintenanceTestTx{}
			switch name {
			case "stopping":
				w.MaintenancePhase = "stopping"
			case "old_registration":
				w.SnapshotRegisterNonce = pgconv.TextOrNull(w.MaintenanceRegisterNonce)
			case "missing_nonce":
				w.SnapshotRegisterNonce.Valid = false
			case "empty_nonce":
				w.SnapshotRegisterNonce.String = " "
			case "missing_registration_floor":
				w.DindRegisterFloor.Valid = false
			case "registration_only":
				w.LastHeartbeatAt = w.DindRegisterFloor
			case "pre_registration_heartbeat":
				w.LastHeartbeatAt = pgconv.Time(now.Add(-3 * time.Second))
			case "missing_heartbeat":
				w.LastHeartbeatAt.Valid = false
			case "stale_heartbeat":
				w.DindRegisterFloor = pgconv.Time(now.Add(-time.Minute))
				w.LastHeartbeatAt = pgconv.Time(now.Add(-46 * time.Second))
			case "future_heartbeat":
				w.LastHeartbeatAt = pgconv.Time(now.Add(time.Second))
			case "missing_capability":
				w.ProtocolCapabilities = nil
			case "offline":
				w.Status = "offline"
			case "unfenced":
				w.MaintenanceFenced = false
			case "parked_busy":
				tx.nonterminal = 1
			case "custody":
				tx.held = true
			}
			tx.worker = w
			op := *DindMaintenanceFromWorker(w)
			op.Phase = "complete"
			got, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op)
			if name != "ready" {
				if !errors.Is(err, ErrDindMaintenanceConflict) || tx.committed || len(tx.writeArgs) != 0 {
					t.Fatalf("completion must fail closed: got=%+v err=%v committed=%v writes=%v", got, err, tx.committed, tx.writeArgs)
				}
				return
			}
			if err != nil || !tx.committed || len(tx.writeArgs) == 0 {
				t.Fatalf("replacement readiness: got=%+v err=%v committed=%v writes=%v", got, err, tx.committed, tx.writeArgs)
			}
			want := []string{"GetWorkerForUpdate", "CountWorkerNonTerminalRuns", "DindMaintenanceCustodyHeld", "SetDindMaintenance"}
			if !reflect.DeepEqual(tx.calls, want) {
				t.Fatalf("completion lock/check/write ordering: got %v want %v", tx.calls, want)
			}
			// The fake returns its input worker; the LiveDB test checks the persisted
			// fence and operation binding returned by SetDindMaintenance.
			if got.RegisterNonce != w.MaintenanceRegisterNonce {
				t.Fatal("completion changed the operation registration binding")
			}
		})
	}
}
