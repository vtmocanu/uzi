package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The store-query half of PRD #529 M4 (ephemeral teardown on run completion) against a
// REAL Postgres. DeleteEphemeralWorkerForRun's EMBEDDED busy guard (the NOT EXISTS
// subquery), the hosted_worker_tokens ON DELETE CASCADE, and the runs.worker_id ON DELETE
// SET NULL are all schema behaviour the fake store cannot exhibit — they only exist on
// real rows. Reuses fleetFixture (claim_fleet_placement_integration_test.go, same
// package). Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// seedEphemeralWorkerBound creates an ephemeral hosted worker bound to runID (via
// ephemeral_run_id) and parks a token row for it, so the cascade is observable. It returns
// the worker id.
func seedEphemeralWorkerBound(fx *fleetFixture, runID uuid.UUID) uuid.UUID {
	fx.t.Helper()
	w, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID:            fx.userID,
		Name:              "ephemeral-" + runID.String(),
		TokenHash:         tokenHash(),
		TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
		HostedSize:        pgtype.Text{String: "m", Valid: true},
		DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
		EphemeralRunID:    runID,
		AnthropicBindMode: "default",
	})
	if err != nil {
		fx.t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}
	if err := fx.q.UpsertHostedWorkerToken(fx.ctx, store.UpsertHostedWorkerTokenParams{
		WorkerID:        w.ID,
		TokenCiphertext: []byte{0x9},
	}); err != nil {
		fx.t.Fatalf("UpsertHostedWorkerToken: %v", err)
	}
	return w.ID
}

// pointRunAtWorker makes runID a run OWNED by workerID (worker_id = workerID) with the
// given status — the shape the run has when its worker reports state (the busy predicate
// keys on runs.worker_id, not on ephemeral_run_id).
func pointRunAtWorker(fx *fleetFixture, runID, workerID uuid.UUID, status string) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`,
		runID, fx.userID, fx.repoID, fx.nextIID(), status, workerID)
}

// workerExists reports whether the worker row is still present.
func workerExists(fx *fleetFixture, workerID uuid.UUID) bool {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE id = $1`, workerID).Scan(&n); err != nil {
		fx.t.Fatalf("count workers: %v", err)
	}
	return n > 0
}

// tokenRowCount reports how many hosted_worker_tokens rows exist for the worker.
func tokenRowCount(fx *fleetFixture, workerID uuid.UUID) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM hosted_worker_tokens WHERE worker_id = $1`, workerID).Scan(&n); err != nil {
		fx.t.Fatalf("count hosted_worker_tokens: %v", err)
	}
	return n
}

// inControllerList reports whether the worker appears in the controller's desired-state
// poll (a hosted row's presence == the pod should exist; its absence == teardown).
func inControllerList(fx *fleetFixture, workerID uuid.UUID) bool {
	fx.t.Helper()
	// disk_pressure derivation is irrelevant to a presence/absence check; pass any valid
	// params (PRD #837 M4 parameterized this query).
	rows, err := fx.q.ListHostedWorkersForController(fx.ctx, store.ListHostedWorkersForControllerParams{
		DiskPressureMinStreak: 2,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		fx.t.Fatalf("ListHostedWorkersForController: %v", err)
	}
	for _, r := range rows {
		if r.ID == workerID {
			return true
		}
	}
	return false
}

func TestDeleteEphemeralWorkerForRunLiveDB(t *testing.T) {
	t.Run("idle terminal run: worker deleted, cascade drops its token, gone from controller list", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{"docker"})
		wID := seedEphemeralWorkerBound(fx, runA)
		// runA is now owned by the worker and has gone terminal (completed).
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed', worker_id = $2 WHERE id = $1`, runA, wID)
		if tokenRowCount(fx, wID) != 1 {
			t.Fatalf("precondition: expected one parked token row for the worker")
		}

		rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		if rows != 1 {
			t.Fatalf("rows affected = %d, want 1 (the idle-terminal ephemeral worker must be dropped)", rows)
		}
		if workerExists(fx, wID) {
			t.Errorf("worker %s still present after teardown", wID)
		}
		if inControllerList(fx, wID) {
			t.Errorf("worker %s still in ListHostedWorkersForController — the controller would not reap the pod", wID)
		}
		if n := tokenRowCount(fx, wID); n != 0 {
			t.Errorf("hosted_worker_tokens rows = %d, want 0 — ON DELETE CASCADE did not remove the token", n)
		}
	})

	t.Run("bound run still running: guard holds, nothing deleted", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{"docker"})
		wID := seedEphemeralWorkerBound(fx, runA)
		// runA owned by the worker and NON-terminal.
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runA, wID)

		rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		if rows != 0 {
			t.Fatalf("rows affected = %d, want 0 (a worker holding a non-terminal run must NOT be torn down)", rows)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s deleted while its bound run is still running", wID)
		}
	})

	t.Run("busy guard: terminal bound run but a second non-terminal run points at the worker", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{"docker"})
		wID := seedEphemeralWorkerBound(fx, runA)
		// runA (the bound run) is terminal...
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed', worker_id = $2 WHERE id = $1`, runA, wID)
		// ...but a SECOND run points at the same worker and is still non-terminal.
		runB := uuid.New()
		pointRunAtWorker(fx, runB, wID, "running")

		rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		if rows != 0 {
			t.Fatalf("rows affected = %d, want 0 (the busy guard must forbid deleting a worker that still holds a non-terminal run)", rows)
		}
		if !workerExists(fx, wID) {
			t.Errorf("worker %s deleted despite holding a second non-terminal run %s", wID, runB)
		}
	})

	t.Run("non-ephemeral worker / no bound ephemeral worker: no-op and idempotent", func(t *testing.T) {
		fx := newFleetFixture(t)
		// A PERSISTENT hosted worker with a terminal run pointed at it — nothing ephemeral.
		var persistentID uuid.UUID
		if err := fx.pool.QueryRow(fx.ctx,
			`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled)
			 VALUES ($1, 'persistent', $2, 'base', 'hosted', 'm', false) RETURNING id`,
			fx.userID, tokenHash()).Scan(&persistentID); err != nil {
			t.Fatalf("insert persistent worker: %v", err)
		}
		runA := uuid.New()
		pointRunAtWorker(fx, runA, persistentID, "completed")

		// runA is not bound to any ephemeral worker (ephemeral_run_id), so the delete matches nothing.
		rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		if rows != 0 {
			t.Fatalf("rows affected = %d, want 0 (a non-ephemeral worker must never be torn down by this query)", rows)
		}
		if !workerExists(fx, persistentID) {
			t.Errorf("persistent worker %s was deleted by the ephemeral teardown query", persistentID)
		}
		// A second call for the same run is a harmless no-op (idempotent).
		rows2, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun (2nd): %v", err)
		}
		if rows2 != 0 {
			t.Fatalf("second call rows affected = %d, want 0 (idempotent no-op)", rows2)
		}

		// A run id that exists but has no worker at all: also 0 rows.
		orphan := fx.queuedRun()
		rows3, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, orphan)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun (orphan run): %v", err)
		}
		if rows3 != 0 {
			t.Fatalf("orphan-run rows affected = %d, want 0", rows3)
		}
	})
}
