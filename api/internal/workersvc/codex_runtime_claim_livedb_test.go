package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestCodexCurrentRuntimePlacementLiveDB(t *testing.T) {
	for _, lane := range []string{"ordinary", "review", "judge"} {
		t.Run(lane, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			oldCaps := []string{capability.CodexHarnessV1}
			worker := e.seedWorker(t, oldCaps)
			runID := e.seedCodexQueuedRun(t)
			if lane != "ordinary" {
				target := e.seedCodexQueuedRun(t)
				e.exec(t, `UPDATE runs SET status='completed' WHERE id=$1`, target)
				if lane == "review" {
					e.exec(t, `UPDATE runs SET kind='task', issue_iid=NULL, branch='fixture-task' WHERE id=$1`, target)
					e.exec(t, `UPDATE runs SET kind='task', issue_iid=NULL, branch='fixture-task', dispatched_at=now(), review_target_run_id=$2 WHERE id=$1`, runID, target)
				} else {
					e.exec(t, `UPDATE runs SET kind='judge', repo_id=NULL, issue_iid=NULL, branch=NULL, target_run_id=$2 WHERE id=$1`, runID, target)
				}
			}
			if _, err := e.q.ClaimRun(e.ctx, e.claimParams(worker, oldCaps, false)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("old runtime claimed Codex with capability kill-switch off: %v", err)
			}
			if got := e.runStatus(t, runID); got != "queued" {
				t.Fatalf("blocked run status=%s", got)
			}
			svc := e.permitService(t)
			if reason := svc.queuedReason(e.ctx, time.Now(), e.codexIndicatingQueuedRow(runID)); reason != reasonNoCurrentCodexRuntime {
				t.Fatalf("queued reason=%q", reason)
			}
			current := append(oldCaps, capability.CodexRuntimeV2)
			if _, err := e.pool.Exec(e.ctx, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, worker, current); err != nil {
				t.Fatal(err)
			}
			claimed, err := e.q.ClaimRun(e.ctx, e.claimParams(worker, current, false))
			if err != nil || claimed.ID != runID {
				t.Fatalf("current runtime could not claim same run: %v", err)
			}
		})
	}
}
