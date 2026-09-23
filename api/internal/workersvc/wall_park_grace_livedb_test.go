package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1497 M1's SWEEP-level behaviour: the boot-grace gating of
// ParkRunsAtWall (the review Fix 2) and the queued-health resolver's "restart worker" reason. Both
// need a real Postgres (the sweep runs the guarded multi-CTE statements; the resolver runs the new
// per-run availability query). Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// seedWallOwner seeds one owner + forge connection + repo and returns their ids (a trimmed
// seedReevalOwner without the anthropic-token fixtures the sweep grace / resolver tests do not need).
func seedWallOwner(t *testing.T, env codexTestEnv) (userID, repoID uuid.UUID) {
	t.Helper()
	userID = uuid.New()
	env.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("wallgrace-%s@e2e", userID))
	connID, repoID := uuid.New(), uuid.New()
	env.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	          VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte("x"))
	env.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	          VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String())
	return userID, repoID
}

// TestWallParkGraceBoundaryLiveDB pins the review Fix 2: ParkRunsAtWall is inside the boot-grace-gated
// stale-worker block, so during boot grace a run whose worker is only TRANSIENTLY stale (a returning
// worker after an api outage) is NOT server-parked from under it; once the grace has elapsed the same
// Sweep parks it. It drives Sweep with grace active then inactive over the same seeded row.
func TestWallParkGraceBoundaryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	p := testParams()
	p.SweeperBootGrace = 10 * time.Minute
	svc := New(env.q, env.box, p)
	userID, repoID := seedWallOwner(t, env)

	// A worker whose heartbeat is stale (> WorkerHeartbeatStale=45s) and that lacks wall_park_v1, so
	// ParkRunsAtWall's stale/incapable arm would server-park its out-of-time run.
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
	          VALUES ($1, $2, 'returning', $3, 'online', now() - interval '5 minutes', 'nonce-A')`, workerID, userID, workerID[:])
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, worker_id, started_at, status_since, budget_wall_seconds, claim_generation)
	          VALUES ($1, $2, $3, 'issue', 987001, 't', 'd', 'running', $4, now() - interval '2 hours', now() - interval '30 seconds', 60, 1)`,
		runID, userID, repoID, workerID)

	statusNow := func() string { return statusOf(t, env, runID) }

	// Grace ACTIVE: readyAt = now, so now < readyAt + 10m. The stale-worker passes (incl. the moved
	// ParkRunsAtWall) are suppressed, so the run is NOT parked.
	svc.SetReadyAt(time.Now())
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatalf("Sweep (grace active): %v", err)
	}
	if got := statusNow(); got != "running" {
		t.Fatalf("during boot grace the run must NOT be server-parked; status = %q, want running (Fix 2)", got)
	}

	// Grace ELAPSED: readyAt far in the past, so the stale-worker passes run and ParkRunsAtWall parks
	// the out-of-time run.
	svc.SetReadyAt(time.Now().Add(-20 * time.Minute))
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatalf("Sweep (grace elapsed): %v", err)
	}
	if got := statusNow(); got != "paused" {
		t.Fatalf("after boot grace the run must be server-parked; status = %q, want paused", got)
	}
	var hold string
	if err := env.pool.QueryRow(env.ctx, `SELECT hold_reason FROM runs WHERE id=$1`, runID).Scan(&hold); err != nil {
		t.Fatalf("read hold_reason: %v", err)
	}
	if hold != "budget_exhausted" {
		t.Fatalf("hold_reason = %q, want budget_exhausted", hold)
	}
}

// TestQueuedReasonReleasedWorkerLiveDB pins the queued-health resolver's new rung (PRD #1497 D19):
// when NO online worker can claim the run (CountOnlineWorkersClaimableForRun == 0) and the released
// pair names an ONLINE worker, queuedReason prints the restart-worker reason — not the
// allowlist/capability one. The only online worker is the released incarnation itself (eligible by
// the fence-blind rungs, but excluded from the per-run claimable count), which is exactly the state a
// single-worker deployment reaches after a server park + resume.
func TestQueuedReasonReleasedWorkerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams()) // nil dockerAllowlist → the repo/allowlist rung is skipped
	userID, repoID := seedWallOwner(t, env)

	// The released incarnation: online, fresh heartbeat, nonce-A.
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
	          VALUES ($1, $2, 'the-only-worker', $3, 'online', now(), 'nonce-A')`, workerID, userID, workerID[:])
	// A queued run resumed from a server park: worker_id NULL, released pair = (workerID, nonce-A).
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, worker_id, started_at, status_since, released_worker_id, released_worker_nonce, claim_generation)
	          VALUES ($1, $2, $3, 'issue', 987101, 't', 'd', 'queued', NULL, now() - interval '2 hours', now() - interval '5 minutes', $4, 'nonce-A', 2)`,
		runID, userID, repoID, workerID)

	rows, err := svc.q.ListActiveRunsForHealth(env.ctx, nil)
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	var row store.ListActiveRunsForHealthRow
	found := false
	for _, r := range rows {
		if r.ID == runID {
			row, found = r, true
			break
		}
	}
	if !found {
		t.Fatalf("run %s not listed for health", runID)
	}
	reason := svc.queuedReason(env.ctx, time.Now(), row)
	want := "waiting for another worker, or restart worker the-only-worker (its previous process was released at the time limit)"
	if reason != want {
		t.Fatalf("queuedReason = %q, want %q", reason, want)
	}
}

// TestReportWallParkLiveDB pins the worker `wall_park` report seam (PRD #1497 M1, D4/D16), the
// service half of the WorkerRunWallPark handler: a fenced report on a past-deadline row calls
// SetRunWallPark and answers paused/applied; a report the fence REJECTS because the server already
// parked the row is answered paused idempotently and, when it carries a head the server park lacked,
// records it via RecordWallParkCapturedHead.
func TestReportWallParkLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	userID, repoID := seedWallOwner(t, env)
	gen := int64(1)

	// A single worker for both cases.
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
	          VALUES ($1, $2, 'w', $3, 'online', now(), 'nonce-A')`, workerID, userID, workerID[:])
	wkr := store.Worker{ID: workerID, UserID: userID}
	mkRun := func(iid int64, status, hold string, released bool, head string) uuid.UUID {
		id := uuid.New()
		var rel any
		if released {
			rel = time.Now().UTC()
		}
		var holdCol, headCol any
		if hold != "" {
			holdCol = hold
		}
		if head != "" {
			headCol = head
		}
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		             status, worker_id, started_at, status_since, budget_wall_seconds, claim_generation,
		             claim_released_at, hold_reason, hold_captured_head)
		          VALUES ($1,$2,$3,'issue',$4,'t','d',$5,$6, now()-interval '2 hours', now()-interval '30 seconds', 60, $7, $8, $9, $10)`,
			id, userID, repoID, iid, status, workerID, gen, rel, holdCol, headCol)
		return id
	}

	t.Run("a fenced report parks and answers paused", func(t *testing.T) {
		id := mkRun(988001, "running", "", false, "")
		run, applied, err := svc.ReportWallPark(env.ctx, wkr, id, "head-1", true, &gen)
		if err != nil || !applied {
			t.Fatalf("ReportWallPark = (applied %v, %v), want (true, nil)", applied, err)
		}
		if run.Status != "paused" || run.HoldReason.String != "budget_exhausted" {
			t.Fatalf("run = (%q,%q), want (paused, budget_exhausted)", run.Status, run.HoldReason.String)
		}
		if run.HoldCapturedHead.String != "head-1" {
			t.Fatalf("hold_captured_head = %q, want head-1", run.HoldCapturedHead.String)
		}
	})

	t.Run("a report the fence rejects (server already parked) is idempotent paused + records the head", func(t *testing.T) {
		// The server already parked it: paused, budget_exhausted, released, head NULL.
		id := mkRun(988002, "paused", "budget_exhausted", true, "")
		run, applied, err := svc.ReportWallPark(env.ctx, wkr, id, "head-2", true, &gen)
		if err != nil || !applied {
			t.Fatalf("idempotent ReportWallPark = (applied %v, %v), want (true, nil)", applied, err)
		}
		if run.Status != "paused" {
			t.Fatalf("status = %q, want paused (idempotent)", run.Status)
		}
		if run.HoldCapturedHead.String != "head-2" {
			t.Fatalf("hold_captured_head = %q, want head-2 (recorded via RecordWallParkCapturedHead)", run.HoldCapturedHead.String)
		}
	})
}
