package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

func TestPlanCrossCheckCompletionAfterParkLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	mustExec(ctx, t, f.pool, `UPDATE runs SET plan_cross_check_required = true,
		auto_approve = true, completion_contract_version = 1 WHERE id = $1`, f.runID)
	rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
		ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("review me"),
	})
	if err != nil || rows != 1 {
		t.Fatalf("park: rows=%d err=%v", rows, err)
	}
	var autoApprove bool
	if err := f.pool.QueryRow(ctx, `SELECT auto_approve FROM runs WHERE id = $1`, f.runID).Scan(&autoApprove); err != nil {
		t.Fatal(err)
	}
	if autoApprove {
		t.Fatal("park did not clear auto_approve")
	}

	complete := func(want int64) {
		t.Helper()
		rows, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			ID: f.runID, WorkerID: pgU(f.workerID),
		})
		if err != nil || rows != want {
			t.Fatalf("SetRunCompleted: rows=%d err=%v, want %d", rows, err, want)
		}
	}
	attempt := func(want bool) {
		t.Helper()
		count, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
			RunID: f.runID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
		})
		if want {
			if err != nil || count != 1 {
				t.Fatalf("RecordCompletionAttempt: count=%d err=%v, want 1", count, err)
			}
		} else if err != pgx.ErrNoRows {
			t.Fatalf("RecordCompletionAttempt: count=%d err=%v, want no rows", count, err)
		}
	}
	assertNoAttempt := func() {
		t.Helper()
		var count int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM run_completion_attempts WHERE run_id = $1`, f.runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("refused completion still inserted %d attempts", count)
		}
	}

	complete(0)
	attempt(false)
	assertNoAttempt()
	// A reclaim can change status without reviewing the stored plan.
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	complete(0)
	attempt(false)
	assertNoAttempt()

	input, err := f.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: f.runID, Body: pgT("{}"), AgentSource: pgT("own"), AgentExclusions: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("CreateApprovePlanInput: %v", err)
	}
	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET applied_at = now(), disposition = 'superseded' WHERE id = $1`, input.ID)
	complete(0)
	attempt(false)
	assertNoAttempt()

	mustExec(ctx, t, f.pool, `UPDATE run_user_inputs SET disposition = 'applied' WHERE id = $1`, input.ID)
	attempt(true)
	complete(1)

	// An unrequired run still accepts both completion writes without a plan.
	var unrequiredID uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO runs
		(user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status, completion_contract_version)
		VALUES ($1, $2, $3, 2, 'unrequired', 'd', 'running', 1) RETURNING id`,
		f.userID, f.repoID, f.workerID).Scan(&unrequiredID); err != nil {
		t.Fatal(err)
	}
	count, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{
		RunID: unrequiredID, WorkerID: pgU(f.workerID), Unmet: []byte(`[]`),
	})
	if err != nil || count != 1 {
		t.Fatalf("unrequired attempt: count=%d err=%v", count, err)
	}
	rows, err = f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		ID: unrequiredID, WorkerID: pgU(f.workerID),
	})
	if err != nil || rows != 1 {
		t.Fatalf("unrequired completion: rows=%d err=%v", rows, err)
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
