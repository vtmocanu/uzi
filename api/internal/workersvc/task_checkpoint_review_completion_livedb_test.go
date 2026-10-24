package workersvc

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
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
			// ReviewRunner reports only completed + generation, without a branch.
			completed, applied, _, err := f.svc.SetStateReport(f.env.ctx, f.wkr, review.ID, StateRequest{
				State: "completed", ClaimGeneration: &b.ClaimGeneration,
			})
			if !enrolled {
				// Known bug #3393: unenrolled task review completion is refused; flip this when #3393 lands.
				// Legacy task completion still attempts NULL, which runs_kind_shape
				// refuses. This fix must not change that separate existing behavior.
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_kind_shape" || applied {
					t.Fatalf("legacy omission changed: applied=%v err=%v", applied, err)
				}
				retained := mustRun(t, f.env, review.ID)
				if retained.Status != review.Status || retained.Branch != review.Branch || countTaskRunsForThenFixOriginal(t, f.env, root.ID) != 0 {
					t.Fatal("legacy refusal changed the review or enqueued then-fix")
				}
				return
			}
			if err != nil || !applied || completed.Status != "completed" {
				t.Fatalf("review completion applied=%v status=%s err=%v", applied, completed.Status, err)
			}
			if completed.Branch != root.Branch || !bytes.Equal(completed.CheckpointCoordinates, review.CheckpointCoordinates) {
				t.Fatal("omitted branch lost the enrolled work branch or coordinates")
			}
			fixID, _ := firstTaskRunForThenFixOriginal(t, f.env, root.ID)
			fix := mustRun(t, f.env, fixID)
			if fix.Branch != root.Branch || !fix.ThenFixOfRunID.Valid || fix.ThenFixOfRunID.Bytes != root.ID || !fix.DispatchedAt.Valid {
				t.Fatal("completion did not enqueue then-fix on the inherited root branch")
			}
			c := claim(fix)
			if c.CheckpointCoordinates.WorkBranch != root.Branch.String || c.CheckpointCoordinates.CheckpointKey != "uzi/task/"+fix.ID.String() {
				t.Fatal("then-fix claim lost the inherited work branch or own checkpoint key")
			}
		})
	}
}
