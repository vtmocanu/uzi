package workersvc

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestTaskCheckpointReviewCompletionLiveDB(t *testing.T) {
	for _, enrolled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enrolled_%v", enrolled), func(t *testing.T) {
			f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
			caps := []string{capability.RecoveryArchiveV1, capability.AdviceClaimFenceV1}
			if enrolled {
				caps = append(caps, capability.TaskCheckpointCoordinatesV1)
			}
			f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, caps)
			f.wkr.ProtocolCapabilities = caps
			root, err := f.svc.CreateTaskRun(f.env.ctx, f.userID, f.repoID, "review completion", "main", false, true, true, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.DispatchTaskRun(f.env.ctx, f.userID, root.ID); err != nil {
				t.Fatal(err)
			}
			claim := func(want store.Run) *ClaimPayload {
				t.Helper()
				p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
				if err != nil || p == nil || p.RunID != want.ID.String() || (p.CheckpointCoordinates != nil) != enrolled {
					t.Fatalf("claim=%+v err=%v", p, err)
				}
				return p
			}
			a := claim(root)
			_, applied, _, err := f.svc.SetStateReport(f.env.ctx, f.wkr, root.ID, StateRequest{
				State: "completed", ClaimGeneration: &a.ClaimGeneration, Branch: &root.Branch.String,
			})
			if err != nil || !applied {
				t.Fatalf("root completion applied=%v err=%v", applied, err)
			}
			reviewID, _ := firstTaskRunForReviewTarget(t, f.env, root.ID)
			review := mustRun(t, f.env, reviewID)
			b := claim(review)
			review = mustRun(t, f.env, reviewID)
			if err := f.svc.PostTaskReview(f.env.ctx, f.wkr, root.ID, TaskReviewSubmission{
				Status: "complete", SummaryMd: "one finding", Findings: []TaskReviewFinding{{
					File: "main.go", Severity: "warning", SummaryMd: "fix this", RationaleMd: "fixture finding",
				}},
			}, AdviceClaim{RunID: &review.ID, Generation: &b.ClaimGeneration}); err != nil {
				t.Fatal(err)
			}
			if enrolled {
				wrong := "feature/foreign"
				_, _, _, err := f.svc.SetStateReport(f.env.ctx, f.wkr, review.ID, StateRequest{
					State: "completed", ClaimGeneration: &b.ClaimGeneration, Branch: &wrong,
				})
				requireCoordinateSQLState(t, err, "23514")
				retained := mustRun(t, f.env, review.ID)
				if retained.Status != review.Status || retained.Branch != review.Branch ||
					!bytes.Equal(retained.CheckpointCoordinates, review.CheckpointCoordinates) {
					t.Fatal("foreign branch changed the review or its frozen coordinates")
				}
			}
			// ReviewRunner's exact body (agent/src/review-runner.ts: { status: "completed",
			// claim_generation }), without a branch. Enrolled or not, a task completion keeps
			// its server-named branch (#3393) and the completed review enqueues then-fix.
			completed, applied, _, err := f.svc.SetStateReport(f.env.ctx, f.wkr, review.ID, StateRequest{
				State: "completed", ClaimGeneration: &b.ClaimGeneration,
			})
			if err != nil || !applied || completed.Status != "completed" {
				t.Fatalf("review completion applied=%v status=%s err=%v", applied, completed.Status, err)
			}
			if completed.Branch != root.Branch || !bytes.Equal(completed.CheckpointCoordinates, review.CheckpointCoordinates) {
				t.Fatal("omitted branch lost the task work branch or changed its coordinates")
			}
			fixID, _ := firstTaskRunForThenFixOriginal(t, f.env, root.ID)
			fix := mustRun(t, f.env, fixID)
			if fix.Branch != root.Branch || !fix.ThenFixOfRunID.Valid || fix.ThenFixOfRunID.Bytes != root.ID || !fix.DispatchedAt.Valid {
				t.Fatal("completion did not enqueue then-fix on the inherited root branch")
			}
			c := claim(fix)
			// An unenrolled then-fix claim carries no descriptor (claim asserts that).
			if enrolled && (c.CheckpointCoordinates.WorkBranch != root.Branch.String || c.CheckpointCoordinates.CheckpointKey != "uzi/task/"+fix.ID.String()) {
				t.Fatal("then-fix claim lost the inherited work branch or own checkpoint key")
			}
		})
	}
}

// TestRunCompletedBranchOmissionControlsLiveDB pins the #3393 preservation to task runs: an
// issue run's branchless completion still writes NULL, and an unenrolled task's explicit
// branch still replaces the stored one.
func TestRunCompletedBranchOmissionControlsLiveDB(t *testing.T) {
	f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
	complete := func(id uuid.UUID, branch *string) store.Run {
		t.Helper()
		n, err := f.env.q.SetRunCompleted(f.env.ctx, store.SetRunCompletedParams{
			ID: id, WorkerID: pgconv.UUID(f.workerID), Branch: stripNULParam(branch),
		})
		if err != nil || n != 1 {
			t.Fatalf("SetRunCompleted = %d, %v; want 1 row", n, err)
		}
		run := mustRun(t, f.env, id)
		if run.Status != "completed" {
			t.Fatalf("status = %s, want completed", run.Status)
		}
		return run
	}
	t.Run("issue_omitted_branch_stores_null", func(t *testing.T) {
		f.nextIID++
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, branch)
		            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1, 'agent/issue-control')`,
			id, f.userID, f.repoID, f.nextIID, f.workerID)
		if run := complete(id, nil); run.Branch.Valid {
			t.Fatalf("issue completion without branch kept %q, want NULL", run.Branch.String)
		}
	})
	t.Run("unenrolled_task_explicit_branch_stored", func(t *testing.T) {
		id := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, worker_id, claim_generation, branch)
		            VALUES ($1, $2, $3, 'task', 't', 'd', 'running', $4, 1, 'uzi/task-created')`,
			id, f.userID, f.repoID, f.workerID)
		explicit := "uzi/task-explicit"
		run := complete(id, &explicit)
		if run.Branch.String != explicit || run.CheckpointCoordinates != nil {
			t.Fatalf("explicit task branch stored %q (coordinates %s), want %q", run.Branch.String, run.CheckpointCoordinates, explicit)
		}
	})
}
