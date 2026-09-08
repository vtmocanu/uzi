package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The write loses a race after SetState's initial ownership read. Change the
// stored row at that seam so returning the initial snapshot cannot pass.
type autopilotPlanRefusalStore struct {
	*fakeStore
	afterRefusal func()
}

func (f *autopilotPlanRefusalStore) SetRunAutopilotPlan(ctx context.Context, arg store.SetRunAutopilotPlanParams) (int64, error) {
	rows, err := f.fakeStore.SetRunAutopilotPlan(ctx, arg)
	f.afterRefusal()
	return rows, err
}

func TestAutopilotPlanRefusalReturnsConcurrentState(t *testing.T) {
	for _, status := range []string{
		"cancelled", "completed", "failed", "queued", "limit_wait", "pool_wait",
		"recovery_wait", "paused", "awaiting_approval", "awaiting_input", "awaiting_followup",
	} {
		t.Run(status, func(t *testing.T) {
			wkr := worker()
			fs := &fakeStore{runOwned: store.Run{
				ID: uuid.New(), WorkerID: pgconv.UUID(wkr.ID), Status: "running",
				AutoApprove: true, PlanSource: "agent",
			}}
			fs.setRunningRows = 1 // a wrongly continued running write would otherwise succeed
			raced := &autopilotPlanRefusalStore{fakeStore: fs, afterRefusal: func() {
				fs.runOwned.Status = status
				fs.runOwned.PlanMd = pgconv.TextOrNull("authoritative stored plan")
				fs.runOwned.SessionID = pgconv.TextOrNull("authoritative session")
			}}
			svc := New(raced, newBox(t), testParams())
			body := "the delayed autopilot plan"
			run, applied, err := svc.SetState(context.Background(), wkr, fs.runOwned.ID, StateRequest{
				State: "running", PlanMd: &body,
			})
			if err != nil {
				t.Fatalf("concurrent %s must return the authoritative state for 409, got error: %v", status, err)
			}
			if applied || run.ID != fs.runOwned.ID || run.Status != status ||
				run.PlanMd != fs.runOwned.PlanMd || run.SessionID != fs.runOwned.SessionID {
				t.Fatalf("refusal returned applied=%t status=%q plan=%q session=%q; want the current %s row with applied=false",
					applied, run.Status, run.PlanMd.String, run.SessionID.String, status)
			}
			if fs.setAutopilotPlanParams == nil {
				t.Fatal("the guarded plan write was never attempted")
			}
			if fs.setRunningParams != nil {
				t.Fatal("a refused plan write must not proceed to SetRunRunning")
			}
		})
	}
}

func TestAutopilotPlanRefusalKeepsWritableMismatchInvalid(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		for _, mismatch := range []string{"human-gated", "seeded", "different-body"} {
			t.Run(status+"/"+mismatch, func(t *testing.T) {
				wkr := worker()
				fs := &fakeStore{runOwned: store.Run{
					ID: uuid.New(), WorkerID: pgconv.UUID(wkr.ID), Status: "running",
					AutoApprove: true, PlanSource: "agent",
				}}
				raced := &autopilotPlanRefusalStore{fakeStore: fs, afterRefusal: func() {
					fs.runOwned.Status = status
					switch mismatch {
					case "human-gated":
						fs.runOwned.AutoApprove = false
					case "seeded":
						fs.runOwned.PlanSource = "seeded"
					case "different-body":
						fs.runOwned.PlanMd = pgconv.TextOrNull("a different stored plan")
					}
				}}
				svc := New(raced, newBox(t), testParams())
				body := "the refused autopilot plan"
				_, applied, err := svc.SetState(context.Background(), wkr, fs.runOwned.ID, StateRequest{
					State: "running", PlanMd: &body,
				})
				if !errors.Is(err, ErrInvalidState) || applied {
					t.Fatalf("writable mismatch: applied=%t err=%v, want ErrInvalidState and applied=false", applied, err)
				}
				if fs.setRunningParams != nil {
					t.Fatal("an invalid plan must not proceed to SetRunRunning")
				}
			})
		}
	}
}

func TestAutopilotPlanRefusalPropagatesRereadErrors(t *testing.T) {
	readFailure := errors.New("ownership reread failed")
	for _, tc := range []struct {
		name string
		read error
		want error
	}{
		{"ownership lost", pgx.ErrNoRows, ErrRunNotOwned},
		{"database error", readFailure, readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wkr := worker()
			fs := &fakeStore{runOwned: store.Run{
				ID: uuid.New(), WorkerID: pgconv.UUID(wkr.ID), Status: "running",
				AutoApprove: true, PlanSource: "agent",
			}}
			raced := &autopilotPlanRefusalStore{fakeStore: fs, afterRefusal: func() { fs.runOwnedErr = tc.read }}
			svc := New(raced, newBox(t), testParams())
			body := "the delayed autopilot plan"
			_, applied, err := svc.SetState(context.Background(), wkr, fs.runOwned.ID, StateRequest{
				State: "running", PlanMd: &body,
			})
			if !errors.Is(err, tc.want) || applied {
				t.Fatalf("reread failure: applied=%t err=%v, want %v and applied=false", applied, err, tc.want)
			}
			if fs.setRunningParams != nil {
				t.Fatal("a failed ownership reread must not proceed to SetRunRunning")
			}
		})
	}
}
