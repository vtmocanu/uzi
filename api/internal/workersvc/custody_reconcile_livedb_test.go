package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestReconcileCustodyReleasesLiveDB is the live-DB proof of the PRD #1296 M4 (D3)
// custody-release RECONCILER backstop, end to end against a REAL Postgres:
//
//	(1) Complete a run holding an OPEN custody hold and SIMULATE the best-effort terminal
//	    release failing (leave the live FKs set) — the ephemeral worker is then NOT reapable
//	    (ReapEphemeralWorkers skips it on the custody predicate).
//	(2) ReconcileCustodyReleases releases that stuck completed-run hold idempotently, after
//	    which ReapEphemeralWorkers deletes the worker (release-then-reap).
//	(3) A FAILED run's open hold with no ready capture is NOT released by the reconciler
//	    (release is never inferred from a failed status), so its worker stays retained.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestReconcileCustodyReleasesLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := New(q, newBox(t), testParams())

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rec4-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rec4', 'https://forge.e2e/g/rec4', 'main', true)`, repoID, connID)

	// seedWorkerRun creates an ephemeral hosted worker bound to a run in the given terminal
	// status, plus an OPEN custody hold naming that worker/run as its live holder — the
	// state a completion leaves behind when the best-effort terminal release failed.
	var iid int64
	seedWorkerRun := func(status string) (workerID, runID, holdID uuid.UUID) {
		workerID, runID, holdID = uuid.New(), uuid.New(), uuid.New()
		iid++
		// The two FKs are circular (workers.ephemeral_run_id → runs, runs.worker_id →
		// workers), so seed in order: run (worker_id NULL) → ephemeral worker bound to it →
		// then point the run at the worker and set the terminal status.
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 'do x', 'ctx', 'queued')`,
			runID, userID, repoID, iid)
		exec(`INSERT INTO workers (id, user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral, ephemeral_run_id, status)
		      VALUES ($1, $2, $3, $4, 'base', 'hosted', 'm', false, true, $5, 'online')`,
			workerID, userID, "eph-"+workerID.String(), workerID[:], runID)
		exec(`UPDATE runs SET status = $2, worker_id = $3 WHERE id = $1`, runID, status, workerID)
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $5, $4)`,
			holdID, userID, repoID, runID, workerID)
		return
	}
	workerExists := func(id uuid.UUID) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count workers: %v", err)
		}
		return n > 0
	}
	holdState := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read hold state: %v", err)
		}
		return s
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	// (1) A completed run whose terminal release failed: worker is custody-held, so reap skips it.
	compWorker, _, compHold := seedWorkerRun("completed")
	// (3) A failed run's hold, seeded up front so one reconcile pass covers both cases.
	failWorker, _, failHold := seedWorkerRun("failed")

	if _, err := q.ReapEphemeralWorkers(ctx, cutoff); err != nil {
		t.Fatalf("ReapEphemeralWorkers(pre-reconcile): %v", err)
	}
	if !workerExists(compWorker) {
		t.Fatalf("completed-run worker reaped before its stuck custody hold was reconciled")
	}

	// (2) The reconciler releases the stuck completed-run hold (and NOT the failed one).
	// The candidate query is instance-wide, so on a shared throwaway DB other tests' holds
	// may also be released — assert on THESE two entities' states, not on the total count.
	released, err := svc.ReconcileCustodyReleases(ctx)
	if err != nil {
		t.Fatalf("ReconcileCustodyReleases: %v", err)
	}
	if released < 1 {
		t.Fatalf("ReconcileCustodyReleases released %d holds, want >= 1 (at least the completed-run hold)", released)
	}
	if s := holdState(compHold); s != "released" {
		t.Fatalf("completed-run hold state = %q after reconcile, want released", s)
	}
	if s := holdState(failHold); s != "open" {
		t.Fatalf("failed-run hold state = %q after reconcile, want open (never release on failed w/o a ready capture)", s)
	}

	// release-then-reap: the completed-run worker is now reapable; the failed-run worker stays.
	if _, err := q.ReapEphemeralWorkers(ctx, cutoff); err != nil {
		t.Fatalf("ReapEphemeralWorkers(post-reconcile): %v", err)
	}
	if workerExists(compWorker) {
		t.Fatalf("completed-run worker still present after release+reap")
	}
	if !workerExists(failWorker) {
		t.Fatalf("failed-run worker reaped despite retaining custody")
	}

	// Idempotent: a second reconcile releases nothing new — every warranted hold present is
	// already released, and the failed-run hold is never a candidate.
	if released, err := svc.ReconcileCustodyReleases(ctx); err != nil {
		t.Fatalf("ReconcileCustodyReleases(again): %v", err)
	} else if released != 0 {
		t.Fatalf("second ReconcileCustodyReleases released %d, want 0 (idempotent)", released)
	}
	if s := holdState(failHold); s != "open" {
		t.Fatalf("failed-run hold state = %q after second reconcile, want still open", s)
	}
}
