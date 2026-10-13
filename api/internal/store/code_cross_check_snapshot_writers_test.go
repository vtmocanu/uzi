package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckSnapshotWritersLiveDB(t *testing.T) {
	for _, writer := range []string{"CreateRun", "CreatePromptRun", "CreateSelfImproveRun", "CreateCIFixRun", "CreateAutoMRReworkRun", "CreateManualMRReworkRunAndAdvance", "CreateTaskRun", "CreateTaskReviewRun", "CreateThenFixRun"} {
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
						mustExec(ctx, t, f.pool, "UPDATE users SET code_cross_check_enabled=$2, plan_cross_check_enabled=NOT $2 WHERE id=$1", f.userID, consent)
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
						case "CreateTaskRun", "CreateTaskReviewRun", "CreateThenFixRun":
							taskID := uuid.New()
							branch := pgT("uzi/task/" + taskID.String())
							run, err = f.q.CreateTaskRun(ctx, store.CreateTaskRunParams{RunID: taskID, UserID: f.userID, RepoID: f.repoID, Branch: branch, IssueTitle: "task", IssueDescription: "candidate", Harness: "claude"})
							if err != nil {
								t.Fatal(err)
							}
							if writer != "CreateTaskRun" {
								mustExec(ctx, t, f.pool, "UPDATE runs SET status='completed',dispatched_at=now() WHERE id=$1", taskID)
							}
							if writer == "CreateTaskReviewRun" {
								run, err = f.q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{RunID: uuid.New(), UserID: f.userID, RepoID: f.repoID, Branch: branch, TargetRunID: pgU(taskID), IssueTitle: "review", Harness: "claude"})
							}
							if writer == "CreateThenFixRun" {
								run, err = f.q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{RunID: uuid.New(), UserID: f.userID, RepoID: f.repoID, Branch: branch, ThenFixOfRunID: pgU(taskID), IssueTitle: "fix", IssueDescription: "candidate", Harness: "claude"})
							}
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
						want := consent
						if run.CodeCrossCheckRequired != want {
							t.Fatalf("snapshot=%v want=%v", run.CodeCrossCheckRequired, want)
						}
						mustExec(ctx, t, f.pool, "UPDATE users SET code_cross_check_enabled=$2, plan_cross_check_enabled=NOT $2 WHERE id=$1", f.userID, !consent)
						persisted, err := f.q.GetRunByID(ctx, run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if persisted.CodeCrossCheckRequired != want {
							t.Fatalf("toggle rewrote snapshot=%v want=%v", persisted.CodeCrossCheckRequired, want)
						}
					})
				}
			}
		}
	}
}
