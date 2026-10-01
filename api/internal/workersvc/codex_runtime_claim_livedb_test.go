package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
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

func TestCodexRuntimePeerSpreadLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
	me := e.seedSpreadWorker(t, caps, 2)
	e.seedActiveRunOwnedBy(t, me)
	e.seedSpreadWorker(t, []string{capability.CodexHarnessV1}, 2)
	runID := e.seedCodexQueuedRun(t)
	claimed, err := e.q.ClaimRun(e.ctx, e.claimParams(me, caps, false))
	if err != nil || claimed.ID != runID {
		t.Fatalf("current busy worker deferred to an old runtime peer: %v", err)
	}
}

func TestCodexRuntimeClaimableCountLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1})
	runID := e.seedCodexQueuedRun(t)
	if n := e.claimableForRun(t, runID); n != 0 {
		t.Fatalf("old runtime counted as claimable: %d", n)
	}
	e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2})
	if n := e.claimableForRun(t, runID); n != 1 {
		t.Fatalf("current runtime not counted: %d", n)
	}
}

func TestCodexRuntimeMixedFleetHealthCountsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	old := e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1, capability.CompletionInterlockV1, capability.CodexCompletionInterlockV1})
	e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CompletionInterlockV1})
	assertCounts := func(want int64) {
		t.Helper()
		custom, err := e.q.CountOnlineWorkersSatisfyingCustomCodex(e.ctx, e.userID)
		if err != nil || custom != want {
			t.Errorf("custom-capable current runtime count=%d want=%d err=%v", custom, want, err)
		}
		completion, err := e.q.CountOnlineWorkersSatisfyingCodexCompletion(e.ctx, store.CountOnlineWorkersSatisfyingCodexCompletionParams{UserID: e.userID, CustomRoot: true})
		if err != nil || completion != want {
			t.Errorf("completion-capable current runtime count=%d want=%d err=%v", completion, want, err)
		}
	}
	assertCounts(0)
	e.exec(t, `UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,'codex_runtime_v2') WHERE id=$1`, old)
	assertCounts(1)
}
