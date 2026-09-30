package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSetStateRefusesParkingReportForJob pins PRD #1908 D-E in setState: a worker-reported
// parking status for a job run is refused (ErrInvalidState, and ErrJobNeverParks so the handler
// can name it) with nothing applied, while the same report for an issue run is not refused by
// that guard.
//
// MUTATION CHECK: neutralising the runkind.Job switch in setState turns every job subtest red.
func TestSetStateRefusesParkingReportForJob(t *testing.T) {
	for _, state := range []string{"paused", "awaiting_input", "awaiting_approval", "awaiting_followup"} {
		t.Run("job/"+state, func(t *testing.T) {
			wkr := worker()
			fs := &fakeStore{runOwned: store.Run{
				ID: uuid.New(), WorkerID: pgconv.UUID(wkr.ID), Status: "running", Kind: "job",
			}}
			svc := New(fs, newBox(t), testParams())
			run, applied, err := svc.SetState(context.Background(), wkr, fs.runOwned.ID, StateRequest{State: state})
			if !errors.Is(err, ErrInvalidState) || !errors.Is(err, ErrJobNeverParks) || applied {
				t.Fatalf("job %s report: applied=%t err=%v, want ErrInvalidState+ErrJobNeverParks and applied=false", state, applied, err)
			}
			if run.Status != "running" {
				t.Fatalf("refusal returned status %q, want the unchanged running row", run.Status)
			}
		})
	}
	// An issue run keeps its own pre-existing refusals (the fake store does not model the paused
	// and awaiting_approval writes, so only the two states that refuse before any write are pinned).
	for state, want := range map[string]string{
		"awaiting_input":    "awaiting_input requires open_question_id",
		"awaiting_followup": "awaiting_followup requires an interactive task run",
	} {
		t.Run("issue/"+state, func(t *testing.T) {
			wkr := worker()
			fs := &fakeStore{runOwned: store.Run{
				ID: uuid.New(), WorkerID: pgconv.UUID(wkr.ID), Status: "running", Kind: "issue",
			}}
			svc := New(fs, newBox(t), testParams())
			_, _, err := svc.SetState(context.Background(), wkr, fs.runOwned.ID, StateRequest{State: state})
			if !errors.Is(err, ErrInvalidState) || errors.Is(err, ErrJobNeverParks) || !strings.Contains(err.Error(), want) {
				t.Fatalf("issue %s report: err = %v, want the existing %q refusal, not the job one", state, err, want)
			}
		})
	}
}
