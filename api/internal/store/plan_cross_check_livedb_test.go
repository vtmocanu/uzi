package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckSnapshotLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	mustExec(ctx, t, f.pool, `UPDATE users SET plan_cross_check_enabled = true WHERE id = $1`, f.userID)
	run, err := f.q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID: f.userID, RepoID: f.repoID, IssueIid: pgtype.Int8{Int64: 2, Valid: true},
		IssueTitle: "plan check", IssueDescription: "candidate", Harness: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSelfImproveRun: %v", err)
	}
	if !run.PlanCrossCheckRequired {
		t.Fatal("opted-in self-improve run did not snapshot requirement")
	}
	mustExec(ctx, t, f.pool, `UPDATE users SET plan_cross_check_enabled = false WHERE id = $1`, f.userID)
	var required bool
	if err := f.pool.QueryRow(ctx, `SELECT plan_cross_check_required FROM runs WHERE id = $1`, run.ID).Scan(&required); err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("toggle changed existing run snapshot")
	}
}

func TestPlanCrossCheckGuardsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true, auto_approve = true, plan_source = 'agent' WHERE id = $1`, f.runID)
	planRows, err := f.q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("unchecked"), CandidateDigest: []byte("digest"),
	})
	if err != nil {
		t.Fatalf("SetRunAutopilotPlan: %v", err)
	}
	if planRows != 0 {
		t.Fatalf("unchecked plan wrote %d rows", planRows)
	}
	progressRows, err := f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: f.runID, WorkerID: pgU(f.workerID), MilestonesCompleted: []byte(`["m1"]`),
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if progressRows != 0 {
		t.Fatalf("pre-plan progress wrote %d rows", progressRows)
	}
	completedRows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		ID: f.runID, WorkerID: pgU(f.workerID),
	})
	if err != nil {
		t.Fatalf("SetRunCompleted: %v", err)
	}
	if completedRows != 0 {
		t.Fatalf("unchecked run completed: %d rows", completedRows)
	}
}
