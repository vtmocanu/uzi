package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The store-query half of PRD #1296 M4 (D3/D4) cleanup safety, against a REAL Postgres.
// These behaviours only exist on real rows and cannot be exhibited by a fake store:
//
//   - The custody-skip predicate INSIDE both teardown DELETEs (DeleteEphemeralWorkerForRun
//     and ReapEphemeralWorkers): a worker holding an OPEN custody hold is gracefully SKIPPED,
//     and once custody is released the SAME reap deletes it (held-worker-not-reaped, then
//     release-then-reap).
//   - ListReleasableCustodyHolds' predicate: it lists a completed-run hold and a hold with a
//     READY capture, but NEVER a failed/running-run hold with no ready capture.
//   - CountOpenCustodyHoldsForWorker / CountOpenCustodyHoldsForRepo, the DeleteWorker and
//     repo-delete guards' predicates.
//   - The custody signals on the two worker-list reads: ListHostedWorkersForController's
//     custody_held (controller wire) and ListWorkersByUser's retaining_unpublished_work
//     (owner surface).
//
// Reuses fleetFixture / seedEphemeralWorkerBound (same package). Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints `ok` with
// PASS=0 is INVALID, not green.

// insertCustodyHold opens a custody hold row directly (bypassing ClaimRun so the test can
// control state, live FKs, repo and run precisely) at generation 1. liveWorker/liveRun
// nil-valued means a released-style hold with the FK nulled. Returns the hold id.
func insertCustodyHold(fx *fleetFixture, runID uuid.UUID, repoID pgtype.UUID, liveWorker pgtype.UUID, state string) uuid.UUID {
	return insertCustodyHoldGen(fx, runID, repoID, liveWorker, state, 1)
}

// insertCustodyHoldGen is insertCustodyHold with an explicit generation, for multi-hold
// fixtures where a run carries a generation-1 orphan hold alongside a generation-2 hold (a
// cross-worker re-claim after a transient worker loss).
func insertCustodyHoldGen(fx *fleetFixture, runID uuid.UUID, repoID pgtype.UUID, liveWorker pgtype.UUID, state string, generation int64) uuid.UUID {
	fx.t.Helper()
	holdID := uuid.New()
	origWorker := uuid.New()
	liveRun := pgtype.UUID{}
	if liveWorker.Valid {
		// An OPEN hold points its live_run_id at the run alongside live_worker_id.
		liveRun = pgtype.UUID{Bytes: runID, Valid: true}
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO recovery_custody_holds
		   (id, user_id, repo_id, run_id, generation, state,
		    original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'ident', $8, $9)`,
		holdID, fx.userID, repoID, runID, generation, state, origWorker, liveWorker, liveRun)
	return holdID
}

// holdIsOpen reports whether the hold is still state='open'.
func holdIsOpen(fx *fleetFixture, holdID uuid.UUID) bool {
	fx.t.Helper()
	var s string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&s); err != nil {
		fx.t.Fatalf("read hold state: %v", err)
	}
	return s == "open"
}

func TestTeardownDeletesSkipCustodyHeldWorkerLiveDB(t *testing.T) {
	t.Run("DeleteEphemeralWorkerForRun skips a custody-held worker, reaps it after release", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{"docker"})
		wID := seedEphemeralWorkerBound(fx, runA)
		// The bound run is terminal (completed) — normally reap-eligible.
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed', worker_id = $2 WHERE id = $1`, runA, wID)
		// But an OPEN custody hold names this worker as its live holder.
		holdID := insertCustodyHold(fx, runA, pgtype.UUID{Bytes: fx.repoID, Valid: true},
			pgtype.UUID{Bytes: wID, Valid: true}, "open")

		// The teardown DELETE must SKIP it (0 rows), leaving the last local source alive.
		rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun: %v", err)
		}
		if rows != 0 {
			t.Fatalf("rows affected = %d, want 0 (a custody-held worker must be skipped, not deleted)", rows)
		}
		if !workerExists(fx, wID) {
			t.Fatalf("worker %s deleted despite holding an open custody hold", wID)
		}

		// Release custody (null the live FK, state released) — the per-hold release primitive.
		if _, err := fx.q.ReleaseCustodyHold(fx.ctx, store.ReleaseCustodyHoldParams{ID: holdID}); err != nil {
			t.Fatalf("ReleaseCustodyHold: %v", err)
		}
		if holdIsOpen(fx, holdID) {
			t.Fatalf("hold %s still open after ReleaseCustodyHold", holdID)
		}

		// Now the SAME teardown deletes it.
		rows, err = fx.q.DeleteEphemeralWorkerForRun(fx.ctx, runA)
		if err != nil {
			t.Fatalf("DeleteEphemeralWorkerForRun(after release): %v", err)
		}
		if rows != 1 {
			t.Fatalf("rows affected = %d, want 1 (release must let teardown proceed)", rows)
		}
		if workerExists(fx, wID) {
			t.Fatalf("worker %s still present after release-then-teardown", wID)
		}
	})

	t.Run("ReapEphemeralWorkers skips a custody-held worker, reaps it after release", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := queuedRunWithCaps(fx, []string{"docker"})
		wID := seedEphemeralWorkerBound(fx, runA)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed', worker_id = $2 WHERE id = $1`, runA, wID)
		holdID := insertCustodyHold(fx, runA, pgtype.UUID{Bytes: fx.repoID, Valid: true},
			pgtype.UUID{Bytes: wID, Valid: true}, "open")

		// cutoff: any value works — a completed bound run satisfies the "no live bound run"
		// disjunct regardless of the deadline.
		cutoff := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
		if _, err := fx.q.ReapEphemeralWorkers(fx.ctx, cutoff); err != nil {
			t.Fatalf("ReapEphemeralWorkers: %v", err)
		}
		if !workerExists(fx, wID) {
			t.Fatalf("worker %s reaped despite holding an open custody hold", wID)
		}

		if _, err := fx.q.ReleaseCustodyHold(fx.ctx, store.ReleaseCustodyHoldParams{ID: holdID}); err != nil {
			t.Fatalf("ReleaseCustodyHold: %v", err)
		}
		if holdIsOpen(fx, holdID) {
			t.Fatalf("hold %s still open after release", holdID)
		}

		if _, err := fx.q.ReapEphemeralWorkers(fx.ctx, cutoff); err != nil {
			t.Fatalf("ReapEphemeralWorkers(after release): %v", err)
		}
		if workerExists(fx, wID) {
			t.Fatalf("worker %s still present after release — reap must proceed once custody is released", wID)
		}
	})
}

func TestListReleasableCustodyHoldsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	repo := pgtype.UUID{Bytes: fx.repoID, Valid: true}

	// A worker to hang the holds' original provenance off (live_worker_id needs a real worker).
	wID := fx.worker("holder", nil, false)
	liveW := pgtype.UUID{Bytes: wID, Valid: true}

	newRun := func(status string) uuid.UUID {
		id := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`,
			id, fx.userID, fx.repoID, fx.nextIID(), status, wID)
		return id
	}

	// (1) completed run, open hold → releasable (full publication). claim_generation = 1
	// matches the hold's generation (insertCustodyHold hardcodes 1), so the generation-aware
	// completed disjunct (h.generation = r.claim_generation) qualifies it.
	completedRun := newRun("completed")
	completedHold := insertCustodyHold(fx, completedRun, repo, liveW, "open")
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET claim_generation = 1 WHERE id = $1`, completedRun)

	// (2) failed run, open hold, NO capture → NOT releasable (never infer success from failed).
	failedRun := newRun("failed")
	failedHold := insertCustodyHold(fx, failedRun, repo, liveW, "open")

	// (3) failed run, open hold, WITH a ready ('available') capture → releasable (durable capture).
	failedCaptured := newRun("failed")
	failedCapturedHold := insertCustodyHold(fx, failedCaptured, repo, liveW, "open")
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO recovery_captures
		   (hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
		 VALUES ($1, $2, $3, 'ident', 'H', 'k', 'available')`,
		failedCapturedHold, failedCaptured, fx.userID)

	// (4) running run, open hold, no capture → NOT releasable.
	runningRun := newRun("running")
	runningHold := insertCustodyHold(fx, runningRun, repo, liveW, "open")

	// (5) already-released hold on a completed run → NOT in list (state != 'open').
	releasedRun := newRun("completed")
	releasedHold := insertCustodyHold(fx, releasedRun, repo, pgtype.UUID{}, "released")

	// (6) MULTI-HOLD generation guard: a COMPLETED run at claim_generation = 2 carries an
	// orphaned generation-1 hold (a crashed worker's, never released) alongside the current
	// generation-2 hold. Only the CURRENT generation actually completed/published, so only the
	// generation-2 hold is releasable via the completed path; the generation-1 orphan (whose
	// uncaptured committed work is a different copy) must NOT be listed. Without the
	// h.generation = r.claim_generation guard, BOTH would be listed and a per-run release would
	// then drop generation-1's last copy — the exact HIGH hazard.
	multiRun := newRun("completed")
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, multiRun)
	orphanGen1Hold := insertCustodyHoldGen(fx, multiRun, repo, liveW, "open", 1)
	currentGen2Hold := insertCustodyHoldGen(fx, multiRun, repo, liveW, "open", 2)

	holds, err := fx.q.ListReleasableCustodyHolds(fx.ctx)
	if err != nil {
		t.Fatalf("ListReleasableCustodyHolds: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, h := range holds {
		got[h.ID] = true
	}
	// The list is instance-wide, so assert membership rather than an exact count.
	if !got[completedHold] {
		t.Errorf("completed-run hold %s not listed — a full-publication hold must be releasable", completedHold)
	}
	if !got[failedCapturedHold] {
		t.Errorf("failed-run hold WITH a ready capture %s not listed — a durable capture must be releasable", failedCapturedHold)
	}
	if got[failedHold] {
		t.Errorf("failed-run hold with NO ready capture %s listed — release must never be inferred from 'failed'", failedHold)
	}
	if got[runningHold] {
		t.Errorf("running-run hold %s listed — a non-terminal run's custody must be retained", runningHold)
	}
	if got[releasedHold] {
		t.Errorf("already-released hold %s listed — only OPEN holds are candidates", releasedHold)
	}
	// (6) generation guard on the completed path.
	if !got[currentGen2Hold] {
		t.Errorf("current-generation hold %s not listed — the completed generation must be releasable", currentGen2Hold)
	}
	if got[orphanGen1Hold] {
		t.Errorf("orphaned generation-1 hold %s listed on a completed run — an older generation's uncaptured work must be RETAINED (generation guard)", orphanGen1Hold)
	}
}

func TestCountOpenCustodyHoldsForWorkerAndRepoLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	repo := pgtype.UUID{Bytes: fx.repoID, Valid: true}
	wID := fx.worker("holder", nil, false)
	liveW := pgtype.UUID{Bytes: wID, Valid: true}

	newRun := func(status string) uuid.UUID {
		id := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`,
			id, fx.userID, fx.repoID, fx.nextIID(), status, wID)
		return id
	}

	run1, run2 := newRun("failed"), newRun("failed")
	hold1 := insertCustodyHold(fx, run1, repo, liveW, "open")
	insertCustodyHold(fx, run2, repo, liveW, "open")

	if n, err := fx.q.CountOpenCustodyHoldsForWorker(fx.ctx, store.CountOpenCustodyHoldsForWorkerParams{WorkerID: wID, UserID: fx.userID}); err != nil {
		t.Fatalf("CountOpenCustodyHoldsForWorker: %v", err)
	} else if n != 2 {
		t.Fatalf("open holds for worker = %d, want 2", n)
	}
	// FIX 2 (owner scope): a FOREIGN owner counts 0 for the SAME worker — the guard filters on
	// user_id, so a non-owner never learns the worker holds custody (falls through to the 404).
	if n, err := fx.q.CountOpenCustodyHoldsForWorker(fx.ctx, store.CountOpenCustodyHoldsForWorkerParams{WorkerID: wID, UserID: uuid.New()}); err != nil {
		t.Fatalf("CountOpenCustodyHoldsForWorker(foreign owner): %v", err)
	} else if n != 0 {
		t.Fatalf("open holds for worker under a foreign owner = %d, want 0 (guard must be owner-scoped)", n)
	}
	if n, err := fx.q.CountOpenCustodyHoldsForRepo(fx.ctx, fx.repoID); err != nil {
		t.Fatalf("CountOpenCustodyHoldsForRepo: %v", err)
	} else if n != 2 {
		t.Fatalf("open holds for repo = %d, want 2", n)
	}

	// Releasing run1's custody drops both counts by one.
	if _, err := fx.q.ReleaseCustodyHold(fx.ctx, store.ReleaseCustodyHoldParams{ID: hold1}); err != nil {
		t.Fatalf("ReleaseCustodyHold: %v", err)
	}
	if n, err := fx.q.CountOpenCustodyHoldsForWorker(fx.ctx, store.CountOpenCustodyHoldsForWorkerParams{WorkerID: wID, UserID: fx.userID}); err != nil {
		t.Fatalf("CountOpenCustodyHoldsForWorker(after release): %v", err)
	} else if n != 1 {
		t.Fatalf("open holds for worker after release = %d, want 1", n)
	}
	if n, err := fx.q.CountOpenCustodyHoldsForRepo(fx.ctx, fx.repoID); err != nil {
		t.Fatalf("CountOpenCustodyHoldsForRepo(after release): %v", err)
	} else if n != 1 {
		t.Fatalf("open holds for repo after release = %d, want 1", n)
	}
}

func TestCustodyControllerAndOwnerListSignalsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	ctx := context.Background()

	// A standing hosted worker (for the controller list) and an open hold naming it.
	var wID uuid.UUID
	if err := fx.pool.QueryRow(ctx,
		`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, status)
		 VALUES ($1, 'custody-signal', $2, 'base', 'hosted', 'm', false, 'online') RETURNING id`,
		fx.userID, tokenHash()).Scan(&wID); err != nil {
		t.Fatalf("insert hosted worker: %v", err)
	}
	runID := uuid.New()
	mustExec(ctx, t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'failed', $5)`,
		runID, fx.userID, fx.repoID, fx.nextIID(), wID)
	holdID := insertCustodyHold(fx, runID, pgtype.UUID{Bytes: fx.repoID, Valid: true},
		pgtype.UUID{Bytes: wID, Valid: true}, "open")

	controllerCustodyHeld := func() bool {
		rows, err := fx.q.ListHostedWorkersForController(ctx, store.ListHostedWorkersForControllerParams{
			DiskPressureMinStreak: 2,
			HeartbeatCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		})
		if err != nil {
			t.Fatalf("ListHostedWorkersForController: %v", err)
		}
		for _, r := range rows {
			if r.ID == wID {
				return r.CustodyHeld
			}
		}
		t.Fatalf("worker %s absent from controller list", wID)
		return false
	}
	ownerRetaining := func() bool {
		rows, err := fx.q.ListWorkersByUser(ctx, fx.userID)
		if err != nil {
			t.Fatalf("ListWorkersByUser: %v", err)
		}
		for _, r := range rows {
			if r.ID == wID {
				return r.RetainingUnpublishedWork
			}
		}
		t.Fatalf("worker %s absent from owner list", wID)
		return false
	}

	if !controllerCustodyHeld() {
		t.Errorf("controller custody_held = false, want true (open hold on the worker)")
	}
	if !ownerRetaining() {
		t.Errorf("owner retaining_unpublished_work = false, want true (open hold on the worker)")
	}

	// Release custody → both signals clear.
	if _, err := fx.q.ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{ID: holdID}); err != nil {
		t.Fatalf("ReleaseCustodyHold: %v", err)
	}
	if holdIsOpen(fx, holdID) {
		t.Fatalf("hold %s still open after release", holdID)
	}
	if controllerCustodyHeld() {
		t.Errorf("controller custody_held = true after release, want false")
	}
	if ownerRetaining() {
		t.Errorf("owner retaining_unpublished_work = true after release, want false")
	}
}
