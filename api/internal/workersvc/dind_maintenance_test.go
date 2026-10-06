package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These fixtures test Service admission and transaction ordering, not SQL execution.
// The delegated live-DB suite must prove the meter, drain and claim query behavior.
type maintenanceTestTx struct {
	pgx.Tx
	worker      store.Worker
	nonterminal int64
	held        bool
	calls       []string
	writeArgs   []any
	committed   bool
}
type maintenanceTestBeginner struct{ tx *maintenanceTestTx }

func (b maintenanceTestBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }
func (tx *maintenanceTestTx) Commit(context.Context) error              { tx.committed = true; return nil }
func (tx *maintenanceTestTx) Rollback(context.Context) error            { return nil }

type maintenanceTestRow struct {
	worker *store.Worker
	value  any
}

func (r maintenanceTestRow) Scan(dest ...any) error {
	if r.worker == nil {
		reflect.ValueOf(dest[0]).Elem().Set(reflect.ValueOf(r.value))
		return nil
	}
	row := reflect.ValueOf(*r.worker)
	if len(dest) != row.NumField() {
		return errors.New("worker fixture scan shape changed")
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(row.Field(i))
	}
	return nil
}
func (tx *maintenanceTestTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	name := strings.Fields(sql)[2]
	tx.calls = append(tx.calls, name)
	switch name {
	case "GetWorkerForUpdate":
		return maintenanceTestRow{worker: &tx.worker}
	case "CountWorkerNonTerminalRuns":
		return maintenanceTestRow{value: tx.nonterminal}
	case "DindMaintenanceCustodyHeld":
		return maintenanceTestRow{value: tx.held}
	case "SetDindMaintenance":
		tx.writeArgs = args
		return maintenanceTestRow{worker: &tx.worker}
	default:
		panic("unexpected maintenance query: " + name)
	}
}
func maintenanceFixture() (store.Worker, time.Time) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return store.Worker{
		ID: uuid.New(), UserID: uuid.New(), Kind: "hosted", DockerEnabled: pgtype.Bool{Bool: true, Valid: true},
		ProtocolCapabilities:  []string{capability.DindMaintenanceV1},
		SnapshotRegisterNonce: pgconv.TextOrNull("registration"),
		DindMeterAt:           pgconv.Time(now.Add(-time.Second)), DindPressureStreak: 2,
		MaintenanceID: pgconv.UUID(uuid.New()), MaintenanceNonce: "operation",
		MaintenancePhase: "ready", MaintenanceDeploymentUid: "deployment", MaintenancePvcUid: "pvc",
		MaintenanceRegisterNonce: "registration", MaintenanceFenced: true, MaintenanceReadyAck: true,
		MaintenanceActivityFloor: pgconv.Time(now.Add(-10 * time.Second)), MaintenanceAckAt: pgconv.Time(now.Add(-time.Second)),
	}, now
}
func maintenanceService(tx *maintenanceTestTx, now time.Time) *Service {
	return &Service{txBeginner: maintenanceTestBeginner{tx}, now: func() time.Time { return now }, p: Params{DiskPressureThreshold: 0.9}}
}
func TestDindMaintenanceACKPruneOutcomeIsAdvisory(t *testing.T) {
	for _, pruned := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed_or_unknown", true: "pruned_without_resample"}[pruned], func(t *testing.T) {
			w, now := maintenanceFixture()
			w.DindMeterAt = pgconv.Time(now.Add(-time.Hour)) // no fresh resample
			tx := &maintenanceTestTx{worker: w}
			zero := 0
			_, err := maintenanceService(tx, now).AckDindMaintenance(context.Background(), w.ID, DindMaintenanceReadyACK{
				DindMaintenance: *DindMaintenanceFromWorker(w), LocalClaims: &zero, LocalExecutions: &zero,
				CustodyClear: true, CustodyCheckedAt: now.Add(-time.Second), Pruned: pruned,
			})
			if err != nil || !tx.committed {
				t.Fatalf("advisory prune ACK: err=%v committed=%v", err, tx.committed)
			}
			want := []string{"GetWorkerForUpdate", "CountWorkerNonTerminalRuns", "DindMaintenanceCustodyHeld", "SetDindMaintenance"}
			if !reflect.DeepEqual(tx.calls, want) {
				t.Fatalf("calls=%v want=%v", tx.calls, want)
			}
		})
	}
}
func TestDindMaintenanceCancellationAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  string
		below  bool
		age    time.Duration
		reason string
		worker bool
		accept bool
	}{
		{"zero_streak_is_not_below", "ready", false, time.Second, "", false, false},
		{"fresh_below", "ready", true, time.Second, "below_threshold", true, true},
		{"stale_below", "ready", true, time.Minute, "", true, false},
		{"before_activity_below", "ready", true, 20 * time.Second, "", true, false},
		{"controller_toggle_off", "requested", false, time.Hour, "recycle_disabled", false, true},
		{"worker_cannot_toggle_off", "ready", false, time.Second, "recycle_disabled", true, false},
		{"cannot_cancel_stopping", "stopping", true, time.Second, "recycle_disabled", false, false},
		{"cannot_cancel_recycling", "recycling", true, time.Second, "recycle_disabled", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, now := maintenanceFixture()
			w.MaintenancePhase = tc.phase
			w.DindPressureStreak = 0
			w.DindBelowThreshold = tc.below
			w.DindMeterAt = pgconv.Time(now.Add(-tc.age))
			tx := &maintenanceTestTx{worker: w}
			s := maintenanceService(tx, now)
			op := *DindMaintenanceFromWorker(w)
			op.Phase = "cancelled"
			op.Reason = tc.reason
			var err error
			if tc.worker {
				_, err = s.AckDindMaintenance(context.Background(), w.ID, DindMaintenanceReadyACK{DindMaintenance: op})
			} else {
				_, err = s.TransitionDindMaintenance(context.Background(), w.ID, op)
			}
			if tc.accept {
				if err != nil || !tx.committed {
					t.Fatalf("cancel err=%v committed=%v", err, tx.committed)
				}
			} else {
				if !errors.Is(err, ErrDindMaintenanceConflict) || len(tx.writeArgs) != 0 {
					t.Fatalf("refusal err=%v writes=%v", err, tx.writeArgs)
				}
			}
		})
	}
}
func TestDindMaintenanceTransitionRetriesDoNotMoveClocks(t *testing.T) {
	for _, phase := range []string{"requested", "ready", "stopping", "recycling", "complete", "cancelled"} {
		t.Run(phase, func(t *testing.T) {
			w, now := maintenanceFixture()
			w.MaintenancePhase = phase
			tx := &maintenanceTestTx{worker: w}
			got, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, *DindMaintenanceFromWorker(w))
			if err != nil || got.Phase != phase || !tx.committed || len(tx.writeArgs) != 0 {
				t.Fatalf("retry got=%+v err=%v writes=%v", got, err, tx.writeArgs)
			}
		})
	}
}
func TestDindMaintenanceReadyRefusesAnyNonterminalRun(t *testing.T) {
	w, now := maintenanceFixture()
	w.MaintenancePhase = "requested"
	w.MaintenanceFenced = false
	tx := &maintenanceTestTx{worker: w, nonterminal: 1}
	op := *DindMaintenanceFromWorker(w)
	op.Phase = "ready"
	_, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op)
	if !errors.Is(err, ErrDindMaintenanceConflict) || tx.committed || len(tx.writeArgs) != 0 {
		t.Fatalf("ready with nonterminal err=%v writes=%v", err, tx.writeArgs)
	}
}
func TestDindMaintenanceRefreshRetainsFenceAndNeedsTwoSamples(t *testing.T) {
	for _, streak := range []int32{1, 2} {
		t.Run(map[int32]string{1: "one_sample_refused", 2: "two_samples_refresh"}[streak], func(t *testing.T) {
			w, now := maintenanceFixture()
			w.MaintenancePhase = "requested"
			w.MaintenanceRegisterNonce = "previous"
			w.DindPressureStreak = streak
			tx := &maintenanceTestTx{worker: w}
			op := *DindMaintenanceFromWorker(w)
			op.RegisterNonce = "registration"
			op.DeploymentUID = "new-deployment"
			op.PVCUID = "new-pvc"
			_, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op)
			if streak == 1 {
				if !errors.Is(err, ErrDindMaintenanceConflict) {
					t.Fatalf("one sample err=%v", err)
				}
				return
			}
			if err != nil || !tx.committed {
				t.Fatalf("refresh err=%v", err)
			}
			if tx.writeArgs[1] == "operation" || tx.writeArgs[3] != "new-deployment" || tx.writeArgs[4] != "new-pvc" ||
				tx.writeArgs[5] != "registration" || tx.writeArgs[6] != true || tx.writeArgs[7] != false || tx.writeArgs[10] != true {
				t.Fatalf("refresh did not rotate binding and retain fence: %v", tx.writeArgs)
			}
		})
	}
}
func TestDindMaintenanceRefreshFromPolledOldBinding(t *testing.T) {
	w, now := maintenanceFixture()
	w.MaintenancePhase = "requested"
	w.MaintenanceRegisterNonce = "previous"
	tx := &maintenanceTestTx{worker: w}
	// The poll exposes the old operation binding. The controller need not know
	// the worker's newly issued registration nonce to refresh requested intent.
	op := *DindMaintenanceFromWorker(w)
	_, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op)
	if err != nil || !tx.committed || len(tx.writeArgs) == 0 || tx.writeArgs[5] != "registration" {
		t.Fatalf("refresh from poll: err=%v writes=%v", err, tx.writeArgs)
	}
}

func TestDindMaintenanceRequestBounds(t *testing.T) {
	w, _ := maintenanceFixture()
	valid := *DindMaintenanceFromWorker(w)
	if !validMaintenanceRequest(valid) {
		t.Fatal("valid operation rejected")
	}
	for _, mutate := range []func(*DindMaintenance){
		func(op *DindMaintenance) { op.Phase = "force" },
		func(op *DindMaintenance) { op.ID = "unknown-id" },
		func(op *DindMaintenance) { op.DeploymentUID = strings.Repeat("x", 129) },
		func(op *DindMaintenance) { op.PVCUID = "pvc\n" },
		func(op *DindMaintenance) { op.Reason = "force" },
	} {
		op := valid
		mutate(&op)
		if validMaintenanceRequest(op) {
			t.Fatalf("invalid instruction accepted: %+v", op)
		}
	}
}

func TestDindMaintenanceStoppingRequiresFreshCustodyACKButNotMeter(t *testing.T) {
	w, now := maintenanceFixture()
	w.DindMeterAt = pgconv.Time(now.Add(-time.Hour))
	tx := &maintenanceTestTx{worker: w}
	op := *DindMaintenanceFromWorker(w)
	op.Phase = "stopping"
	if _, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op); err != nil {
		t.Fatalf("stopping without resample: %v", err)
	}
	w.MaintenanceAckAt = pgconv.Time(now.Add(-time.Minute))
	tx = &maintenanceTestTx{worker: w}
	if _, err := maintenanceService(tx, now).TransitionDindMaintenance(context.Background(), w.ID, op); !errors.Is(err, ErrDindMaintenanceConflict) {
		t.Fatalf("stale custody ACK: %v", err)
	}
}
