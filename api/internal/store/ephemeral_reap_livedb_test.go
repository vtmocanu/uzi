package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The store-query half of PRD #529 M5 (ephemeral orphan/failure GC reaper, Decision 6)
// against a REAL Postgres. ReapEphemeralWorkers' three orphan shapes, the busy guard, the
// online_since/created_at deadline comparison, the hosted_worker_tokens ON DELETE CASCADE,
// and the run's absence from the controller poll are all schema/timestamp behaviour the
// fake store cannot exhibit. Reuses fleetFixture + the seedEphemeralWorkerBound / workerExists
// / tokenRowCount / inControllerList helpers from ephemeral_teardown_livedb_test.go (same
// package). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// setWorkerClock overrides a worker's created_at and online_since directly (raw UPDATE),
// since CreateEphemeralHostedWorker sets neither, so the reaper's deadline comparisons are
// exercised deterministically against the test's chosen cutoff. A zero onlineSince leaves
// online_since NULL (the never-booted shape).
func setWorkerClock(fx *fleetFixture, workerID uuid.UUID, createdAt time.Time, onlineSince time.Time) {
	fx.t.Helper()
	var os any
	if onlineSince.IsZero() {
		os = nil
	} else {
		os = onlineSince
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE workers SET created_at = $2, online_since = $3 WHERE id = $1`,
		workerID, createdAt, os)
}

func TestReapEphemeralWorkersLiveDB(t *testing.T) {
	// A cutoff in the recent past; "old" timestamps precede it (past the deadline) and
	// "recent" ones follow it (still within grace). The reaper receives this cutoff directly.
	now := time.Now()
	cutoff := now.Add(-30 * time.Minute)
	old := now.Add(-1 * time.Hour) // strictly before cutoff
	recent := now                  // strictly after cutoff
	cut := pgtype.Timestamptz{Time: cutoff, Valid: true}

	t.Run("(a) bound run terminal (non-SetState path): reaped, token cascaded, gone from controller list", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		// Terminal via a direct status write (NOT the M4 SetState hook) — the backstop case.
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled', worker_id = $2 WHERE id = $1`, runA, wID)
		if tokenRowCount(fx, wID) != 1 {
			t.Fatalf("precondition: expected one parked token row for the worker")
		}

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: a terminal owning run should be reaped by clause (a)", wID)
		}
		if inControllerList(fx, wID) {
			t.Errorf("worker %s still in ListHostedWorkersForController — the controller would not reap the pod", wID)
		}
		if n := tokenRowCount(fx, wID); n != 0 {
			t.Errorf("hosted_worker_tokens rows = %d, want 0 — ON DELETE CASCADE did not remove the token", n)
		}
	})

	t.Run("(a) ephemeral_run_id NULL (hard-deleted run, FK SET NULL): reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		// Unlink the bound run (what the FK ON DELETE SET NULL leaves behind).
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET ephemeral_run_id = NULL WHERE id = $1`, wID)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: an unlinked ephemeral_run_id should be reaped by clause (a)", wID)
		}
	})

	t.Run("(b) never booted past the deadline (online_since NULL, created_at old): reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun() // stays queued/non-terminal, worker_id NULL: never claimed
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, old, time.Time{}) // created_at old, online_since NULL

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: a never-booted worker past the deadline should be reaped by clause (b)", wID)
		}
	})

	t.Run("(b) still booting (online_since NULL, created_at recent): NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, time.Time{}) // created_at within grace, online_since NULL

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped while still within the provision deadline (clause (b) grace)", wID)
		}
	})

	t.Run("(c) idle-stolen (online past deadline, bound run served by a sibling): reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, old, old) // online, past the deadline
		sibling := fx.worker("sibling", nil, false)
		// The bound run is non-terminal but claimed by the SIBLING, not this worker.
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runA, sibling)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: an idle-stolen worker past the deadline should be reaped by clause (c)", wID)
		}
		if !workerExists(fx, sibling) {
			t.Errorf("sibling worker %s reaped — the reaper must only delete the idle ephemeral worker", sibling)
		}
	})

	t.Run("(c) idle but within the deadline (online_since recent): NOT reaped (grace)", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, old, recent) // online, but only just — within grace
		sibling := fx.worker("sibling", nil, false)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runA, sibling)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped while still within the idle grace (clause (c) online_since >= cutoff)", wID)
		}
	})

	t.Run("healthy: actively running its own bound run (busy guard) is NOT reaped even with an old online_since", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, old, old) // old online_since — would trip (c) if the run were stolen
		// But the bound run is non-terminal and owned by THIS worker: the busy guard protects it.
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runA, wID)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped while actively holding its own non-terminal run — the busy guard failed", wID)
		}
	})

	t.Run("healthy: just-provisioned with a queued bound run is NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun() // queued, worker_id NULL
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, time.Time{}) // fresh, not yet online

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped right after provisioning while its bound run is still queued", wID)
		}
	})

	// Arm (g) (PRD #4320 / #4505): an online ephemeral worker without claude_provider_v1 bound to a
	// non-terminal provider-bound run can never claim it, so it is reaped (no deadline) and the run
	// becomes ephemeral-provisioning demand again.
	t.Run("(g) incapable online worker bound to a queued provider run: reaped, run listed again", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-incapable")
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent) // online, well within every deadline
		if unplaceableRunIDs(fx)[runA] {
			t.Fatalf("precondition: run %s already served by its ephemeral worker must not be listed", runA)
		}

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: an online worker without %s bound to a provider run should be reaped by clause (g)", wID, claudeProviderCap)
		}
		if !unplaceableRunIDs(fx)[runA] {
			t.Errorf("provider run %s not listed for provisioning after its incapable worker was reaped", runA)
		}
	})

	t.Run("(g) capable online worker bound to a queued provider run: NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-capable")
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, wID, []string{claudeProviderCap})

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s advertising %s reaped by clause (g)", wID, claudeProviderCap)
		}
	})

	t.Run("(g) incapable online worker bound to a vendor run: NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s bound to a vendor run reaped by clause (g)", wID)
		}
	})

	t.Run("(g) incapable worker bound to a provider run but not yet online: NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-booting")
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, time.Time{}) // online_since NULL: not registered yet

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped by clause (g) before it registered", wID)
		}
	})

	t.Run("(g) busy incapable worker (holds a non-terminal run): NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-busy")
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)
		pointRunAtWorker(fx, uuid.New(), wID, "running")

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped while holding a non-terminal run — the busy guard must still apply to clause (g)", wID)
		}
	})

	t.Run("(g) custody-held incapable worker: NOT reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-custody")
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO recovery_custody_holds
			(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
			VALUES ($1,$2,$3,$4,1,'open',$5,'ident',$5,$4)`, uuid.New(), fx.userID, fx.repoID, runA, wID)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped while an open custody hold names it — the custody guard must still apply to clause (g)", wID)
		}
	})

	t.Run("(g) incapable worker bound to a TERMINAL provider run is reaped by (a), not left", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-terminal")
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, runA)
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present: a terminal run is reaped by clause (a)", wID)
		}
	})

	// Pins the status guard inside arm (g): a TERMINAL provider-bound run does not make an incapable
	// worker reapable through (g). With a live ephemeral lease, arm (f) spares the worker from (a),
	// so only (g) could reap it.
	t.Run("(g) incapable worker with a live lease bound to a TERMINAL provider run: NOT reaped (status guard)", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{})
		bindRunToProvider(fx, runA, "reap-terminal-leased")
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, runA)
		wID := seedEphemeralWorkerBound(fx, runA)
		setWorkerClock(fx, wID, recent, recent)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET lease_since = now(), lease_repo_id = $2, lease_branch = 'agent/issue-1', draining_since = NULL WHERE id = $1`, wID, fx.repoID)

		lease := pgtype.Interval{Microseconds: int64(time.Hour / time.Microsecond), Valid: true}
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, lease); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s reaped: a live lease on a terminal run is spared by (f) and (g) must not apply to a terminal run", wID)
		}
	})

	t.Run("non-ephemeral worker with a terminal run is never touched", func(t *testing.T) {
		fx := newFleetFixture(t)
		var persistentID uuid.UUID
		if err := fx.pool.QueryRow(fx.ctx,
			`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, created_at)
			 VALUES ($1, 'persistent', $2, 'base', 'hosted', 'm', false, $3) RETURNING id`,
			fx.userID, tokenHash(), old).Scan(&persistentID); err != nil {
			t.Fatalf("insert persistent worker: %v", err)
		}
		runA := uuid.New()
		pointRunAtWorker(fx, runA, persistentID, "completed")

		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cut, pgtype.Interval{}); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, persistentID) {
			t.Errorf("persistent (non-ephemeral) worker %s was reaped — the reaper must only touch ephemeral rows", persistentID)
		}
	})
}
