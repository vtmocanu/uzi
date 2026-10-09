package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCrossCheckLaneAffinityIsNotReleasedWorkerLiveDB(t *testing.T) {
	f := laneFixture(t)
	now := time.Now()
	rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, CodexCuratedModels())
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, r := range rows {
		if r.ID != f.runID {
			continue
		}
		seen = true
		if !r.WorkerID.Valid || r.ReleasedWorkerID.Valid {
			t.Fatal("affinity confused with released process")
		}
		if got := f.svc.queuedReason(f.env.ctx, now, r); got != reasonPlanCrossCheckWaiting {
			t.Fatalf("affinity health reason: %s", got)
		}
	}
	if !seen {
		t.Fatal("child missing")
	}
	// An ordinary run's actual released incarnation still reaches the restart rung.
	child := mustRun(t, f.env, f.runID)
	ordinary := seedOutageRun(t, f.env, f.userID, uuid.UUID(child.RepoID.Bytes), f.workerID, "queued", "issue", 1, 0)
	f.env.exec("UPDATE workers SET last_heartbeat_at=now(),snapshot_register_nonce='nonce-U2' WHERE id=$1", f.workerID)
	f.env.exec("UPDATE runs SET released_worker_id=$2,released_worker_nonce='nonce-U2' WHERE id=$1", ordinary, f.workerID)
	rows, err = f.env.q.ListActiveRunsForHealth(f.env.ctx, CodexCuratedModels())
	if err != nil {
		t.Fatal(err)
	}
	seen = false
	for _, r := range rows {
		if r.ID != ordinary {
			continue
		}
		seen = true
		if !r.ReleasedWorkerID.Valid {
			t.Fatal("released identity lost")
		}
		if got := f.svc.queuedReason(f.env.ctx, now, r); !strings.Contains(got, "restart worker ") {
			t.Fatalf("released rung lost: %s", got)
		}
	}
	if !seen {
		t.Fatal("released run missing")
	}
}
