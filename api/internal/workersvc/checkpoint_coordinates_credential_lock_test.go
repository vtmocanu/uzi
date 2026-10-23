package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type credentialCheckpointLockFake struct {
	Store
	prior     store.Run
	locked    store.LockCredentialDisabledRunForPromotionRow
	worker    store.Worker
	workerErr error
	calls     []string
}

func (f *credentialCheckpointLockFake) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	f.calls = append(f.calls, "pre-read")
	return f.prior, nil
}
func (f *credentialCheckpointLockFake) GetWorkerForUpdate(context.Context, uuid.UUID) (store.Worker, error) {
	f.calls = append(f.calls, "worker")
	return f.worker, f.workerErr
}
func (f *credentialCheckpointLockFake) LockCredentialDisabledRunForPromotion(context.Context, store.LockCredentialDisabledRunForPromotionParams) (store.LockCredentialDisabledRunForPromotionRow, error) {
	f.calls = append(f.calls, "run")
	return f.locked, nil
}
func (f *credentialCheckpointLockFake) OptInCheckpointCoordinates(context.Context) error {
	f.calls = append(f.calls, "opt-in")
	return nil
}

func (f *credentialCheckpointLockFake) SetRunCredentialOverride(context.Context, store.SetRunCredentialOverrideParams) (int64, error) {
	f.calls = append(f.calls, "override")
	return 1, nil
}
func (f *credentialCheckpointLockFake) SettleCredentialDisabledPause(context.Context, store.SettleCredentialDisabledPauseParams) (store.SettleCredentialDisabledPauseRow, error) {
	f.calls = append(f.calls, "settle")
	return store.SettleCredentialDisabledPauseRow{ID: f.locked.ID, Status: "paused"}, nil
}

func TestCheckpointCoordinatesCredentialLockAdmission(t *testing.T) {
	for _, mode := range []string{"compatible", "incompatible", "foreign worker", "missing worker", "raced worker", "raced enrollment", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			owner, worker, runID := uuid.New(), uuid.New(), uuid.New()
			marker := []byte(`{"version":1}`)
			f := &credentialCheckpointLockFake{
				prior:  store.Run{ID: runID, UserID: owner, WorkerID: pgconv.UUID(worker), CheckpointCoordinates: marker},
				locked: store.LockCredentialDisabledRunForPromotionRow{ID: runID, UserID: owner, WorkerID: pgconv.UUID(worker), CheckpointCoordinates: marker},
				worker: store.Worker{ID: worker, UserID: owner, ProtocolCapabilities: []string{capability.TaskCheckpointCoordinatesV1}},
			}
			wantCalls := "pre-read,worker,run"
			switch mode {
			case "compatible":
				wantCalls += ",opt-in,override,settle"
			case "incompatible":
				f.worker.ProtocolCapabilities = nil
				wantCalls = "pre-read,worker"
			case "foreign worker":
				f.worker.UserID = uuid.New()
				wantCalls = "pre-read,worker"
			case "missing worker":
				f.workerErr = pgx.ErrNoRows
				wantCalls = "pre-read,worker"
			case "raced worker":
				f.locked.WorkerID = pgconv.UUID(uuid.New())
			case "raced enrollment":
				f.prior.CheckpointCoordinates = nil
				wantCalls = "pre-read,run"
			case "legacy":
				f.prior.CheckpointCoordinates = nil
				f.locked.CheckpointCoordinates = nil
				wantCalls = "pre-read,run,override,settle"
			}
			f.locked.Status = "paused"
			f.locked.HoldReason = pgconv.Text("credential_disabled")
			f.locked.PauseRequestedAt.Valid = true
			svc := &Service{q: f, p: testParams()}
			status, err := svc.reassignCredentialDisabledRun(context.Background(), owner, runID, &CredentialOverride{Mode: CredentialOverrideModeAuto})
			success := mode == "compatible" || mode == "legacy"
			if success && (err != nil || status != "paused") {
				t.Fatalf("status=%s err=%v", status, err)
			}
			if !success && err != ErrCredentialSwitchRaced {
				t.Fatalf("want fail closed, got %v", err)
			}
			if calls := strings.Join(f.calls, ","); calls != wantCalls {
				t.Fatalf("calls=%s want=%s", calls, wantCalls)
			}
		})
	}
}
