package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckSnapshotWritersLiveDB(t *testing.T) {
	for _, writer := range []string{"CreateRun", "CreatePromptRun", "CreateSelfImproveRun", "CreateCIFixRun", "CreateAutoMRReworkRun", "CreateManualMRReworkRunAndAdvance"} {
		for _, consent := range []bool{false, true} {
			for _, auto := range []bool{false, true} {
				if !auto && writer != "CreateRun" && writer != "CreatePromptRun" && writer != "CreateCIFixRun" {
					continue
				}
				for _, seeded := range []bool{false, true} {
					if seeded && writer != "CreateRun" {
						continue
					}
					t.Run(fmt.Sprintf("%s/consent=%t/auto=%t/seeded=%t", writer, consent, auto, seeded), func(t *testing.T) {
						ctx := context.Background()
						fx := setupPlanCrossCheckDeletion(ctx, t)
						f := fx.f
						mustExec(ctx, t, f.pool, "UPDATE users SET plan_cross_check_enabled=$2 WHERE id=$1", f.userID, consent)
						var run store.Run
						var err error
						switch writer {
						case "CreateRun":
							source := "agent"
							var plan pgtype.Text
							if seeded {
								source = "seeded"
								plan = pgT("seeded plan")
							}
							run, err = f.q.CreateRun(ctx, store.CreateRunParams{UserID: f.userID, RepoID: f.repoID, IssueIid: pgtype.Int8{Int64: 2, Valid: true}, IssueTitle: "snapshot", IssueDescription: "candidate", AutoApprove: auto, PlanSource: source, PlanMd: plan, TriggerSource: "manual", Harness: "claude"})
						case "CreatePromptRun":
							scheduleID := uuid.New()
							mustExec(ctx, t, f.pool, `INSERT INTO run_schedules (id,user_id,repo_id,target,prompt,timing,run_at)
        VALUES ($1,$2,$3,'prompt','candidate','once',now())`, scheduleID, f.userID, f.repoID)
							run, err = f.q.CreatePromptRun(ctx, store.CreatePromptRunParams{UserID: f.userID, RepoID: f.repoID, IssueTitle: "snapshot", IssueDescription: "candidate", ScheduleID: scheduleID, AutoApprove: auto, Harness: "claude"})
						case "CreateSelfImproveRun":
							run, err = f.q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{UserID: f.userID, RepoID: f.repoID, IssueIid: pgtype.Int8{Int64: 2, Valid: true}, IssueTitle: "snapshot", IssueDescription: "candidate", Harness: "claude"})
						case "CreateCIFixRun":
							run, err = f.q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{UserID: f.userID, RepoID: f.repoID, IssueTitle: "snapshot", IssueDescription: "candidate", PipelineID: pgtype.Int8{Int64: 2, Valid: true}, PipelineRef: pgT("snapshot-ci"), FailureSnapshot: []byte("{}"), CiConfigPaths: []string{}, AutoApprove: auto, Harness: "claude"})
						case "CreateAutoMRReworkRun":
							run, err = f.q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{UserID: f.userID, RepoID: f.repoID, IssueTitle: "snapshot", IssueDescription: "candidate", PipelineRef: pgT("snapshot-auto"), MrIid: pgtype.Int8{Int64: 2, Valid: true}, TargetRunID: pgU(f.runID), ReviewComments: []byte("[]"), TriggerSource: "mr_rework", Harness: "claude"})
						case "CreateManualMRReworkRunAndAdvance":
							run, err = f.q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{UserID: f.userID, RepoID: f.repoID, IssueTitle: "snapshot", IssueDescription: "candidate", PipelineRef: pgT("snapshot-manual"), MrIid: pgtype.Int8{Int64: 2, Valid: true}, TargetRunID: pgU(f.runID), ReviewComments: []byte("[]"), Harness: "claude", HighWater: 3})
						}
						if err != nil {
							t.Fatal(err)
						}
						want := consent && auto && !seeded
						if run.PlanCrossCheckRequired != want {
							t.Fatalf("snapshot=%v want=%v", run.PlanCrossCheckRequired, want)
						}
						mustExec(ctx, t, f.pool, "UPDATE users SET plan_cross_check_enabled=$2 WHERE id=$1", f.userID, !consent)
						persisted, err := f.q.GetRunByID(ctx, run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if persisted.PlanCrossCheckRequired != want {
							t.Fatalf("toggle rewrote snapshot=%v want=%v", persisted.PlanCrossCheckRequired, want)
						}
					})
				}
			}
		}
	}
}

func TestPlanCrossCheckExcludedSnapshotWritersLiveDB(t *testing.T) {
	for _, writer := range []string{"chat", "chat continue", "judge", "job", "task", "task review", "then fix", "cross_check"} {
		t.Run(writer, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, "UPDATE users SET plan_cross_check_enabled=true WHERE id=$1", f.userID)
			var run store.Run
			var err error
			switch writer {
			case "job":
				run, err = f.q.CreateJobRun(ctx, store.CreateJobRunParams{RunID: uuid.New(), UserID: f.userID, JobType: pgT("research"), IssueTitle: "job", IssueDescription: "candidate", Harness: "claude"})
			case "chat continue":
				original, createErr := f.q.CreateChatRun(ctx, store.CreateChatRunParams{RunID: uuid.New(), UserID: f.userID, IssueTitle: "original", IssueDescription: "candidate"})
				if createErr != nil {
					t.Fatal(createErr)
				}
				mustExec(ctx, t, f.pool, "UPDATE runs SET status='completed' WHERE id=$1", original.ID)
				run, err = f.q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{UserID: f.userID, IssueTitle: "continue", ResumeOfRunID: pgU(original.ID), WorkerID: pgU(f.workerID)})
			case "task", "task review", "then fix":
				taskID := uuid.New()
				branch := pgT("uzi/task/" + taskID.String())
				original, createErr := f.q.CreateTaskRun(ctx, store.CreateTaskRunParams{RunID: taskID, UserID: f.userID, RepoID: f.repoID, Branch: branch, IssueTitle: "task", IssueDescription: "candidate", Harness: "claude"})
				if createErr != nil {
					t.Fatal(createErr)
				}
				run = original
				if writer != "task" {
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='completed',dispatched_at=now() WHERE id=$1", taskID)
				}
				if writer == "task review" {
					run, err = f.q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{RunID: uuid.New(), UserID: f.userID, RepoID: f.repoID, Branch: branch, TargetRunID: pgU(taskID), IssueTitle: "review", Harness: "claude"})
				}
				if writer == "then fix" {
					run, err = f.q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{RunID: uuid.New(), UserID: f.userID, RepoID: f.repoID, Branch: branch, ThenFixOfRunID: pgU(taskID), IssueTitle: "fix", IssueDescription: "candidate", Harness: "claude"})
				}
			case "chat":
				run, err = f.q.CreateChatRun(ctx, store.CreateChatRunParams{RunID: uuid.New(), UserID: f.userID, IssueTitle: "chat", IssueDescription: "candidate"})
			case "judge":
				run, err = f.q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{UserID: f.userID, TargetRunID: pgU(f.runID), IssueTitle: "judge", IssueDescription: "candidate", TriggerSource: "manual", Harness: "claude"})
			case "cross_check":
				mustExec(ctx, t, f.pool, "UPDATE runs SET auto_approve=true WHERE id=$1", f.runID)
				run, err = f.q.CreatePlanCrossCheckChild(ctx, store.CreatePlanCrossCheckChildParams{ChildID: uuid.New(), ChildHarness: "codex", BudgetWallSeconds: 300, LeadRunID: f.runID, UserID: f.userID, WorkerID: pgU(f.workerID), ClaimGeneration: 1})
			}
			if err != nil {
				t.Fatal(err)
			}
			if run.ID == uuid.Nil {
				t.Fatalf("%s did not insert a run", writer)
			}
			if run.PlanCrossCheckRequired {
				t.Fatalf("%s inherited snapshot", writer)
			}
			mustExec(ctx, t, f.pool, "UPDATE users SET plan_cross_check_enabled=false WHERE id=$1", f.userID)
			persisted, err := f.q.GetRunByID(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.PlanCrossCheckRequired {
				t.Fatalf("%s persisted an excluded snapshot", writer)
			}
		})
	}
}
