package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1030 M2 — hold resume affinity through an image ROLL, against a REAL Postgres.
//
// The trigger for run #1009: a run parked (limit_wait→queued, worker_id retained for
// affinity) while its owner worker was cordoned for an image roll (draining_since set,
// worker ROW + PVC survive) was released by ClaimRun's affinity leg the instant the owner
// went draining, and a live peer stole it cold. M2 rewrites the affinity NOT EXISTS so the
// pin HOLDS whenever the owner ROW exists AND (it is draining OR its heartbeat is fresh),
// and falls open ONLY when the row is GONE (teardown) or the owner is heartbeat-stale AND
// draining_since IS NULL (death/hang — ADR-628 D3a's protected case). It also threads
// @claimant_draining so a DRAINING worker reaches ClaimRun (the workersvc early return was
// removed) but is scoped to claim ONLY its own promoted run (PRD #422 D7: "a draining
// worker claims nothing new").
//
// Constants cited (do NOT change; from api/internal/config/config.go): WORKER_AFFINITY_
// CEILING 2h (the @affinity_cutoff ceiling), WORKER_HEARTBEAT_STALE 45s (@heartbeat_cutoff).
// The cases below drive a self-consistent MODEL of those cutoffs (@affinity_cutoff = now-30m,
// @heartbeat_cutoff = now-45s): the test proves ClaimRun reads whatever cutoffs it is passed,
// not that 30m/45s are the defaults. Each case has a positive control so none passes vacuously.
//
// Scope: cases (a)-(d) are all queued/limit_wait-shaped runs (status='queued' with worker_id
// retained, exactly what PromoteLimitWaitRuns leaves behind). A RUNNING run force-rolled
// mid-flight is handled by the pre-existing stale-worker sweeps, complementary to M2 and not
// exercised here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh
// provides one).

type rollAffinityFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
	iid    int64
}

func newRollAffinityFixture(t *testing.T) *rollAffinityFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	fx := &rollAffinityFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New()}
	connID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		fx.userID, fmt.Sprintf("roll-affinity-%s@e2e", fx.userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, fx.userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, fx.repoID, connID)
	return fx
}

func (fx *rollAffinityFixture) nextIID() int64 { fx.iid++; return fx.iid }

// worker inserts a cap-2, non-docker, online worker whose last_heartbeat_at is stamped
// heartbeatAge in the PAST and whose draining_since is set iff draining. heartbeatAge=0 +
// draining=false is a live, non-draining claim target; draining=true models a roll cordon.
func (fx *rollAffinityFixture) worker(name string, heartbeatAge time.Duration, draining bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, docker_enabled)
		 VALUES ($1, $2, $3, $4, 'online', now() - make_interval(secs => $5), 2, false)`,
		id, fx.userID, name, tokenHash(), heartbeatAge.Seconds())
	if draining {
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, id)
	}
	return id
}

// deleteWorker removes a worker ROW (models the teardown path: DeleteWorkerForUser /
// ReapEphemeralWorkers delete the row API-side before the controller's kube teardown).
func (fx *rollAffinityFixture) deleteWorker(id uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `DELETE FROM workers WHERE id = $1`, id)
}

// pinnedRun inserts a queued repo-bearing run owned by ownerID whose updated_at is stamped
// updatedAge in the PAST (0 = just-promoted, within the ceiling), returns its id.
func (fx *rollAffinityFixture) pinnedRun(ownerID uuid.UUID, updatedAge time.Duration) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, updated_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, now() - make_interval(secs => $6))`,
		id, fx.userID, fx.repoID, fx.nextIID(), ownerID, updatedAge.Seconds())
	return id
}

// unclaimedRun inserts a queued repo-bearing run with worker_id NULL (a new, never-claimed run).
func (fx *rollAffinityFixture) unclaimedRun() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', NULL)`,
		id, fx.userID, fx.repoID, fx.nextIID())
	return id
}

// claim runs ClaimRun as workerID with the test's modeled cutoffs (@affinity_cutoff = now-30m,
// @heartbeat_cutoff = now-45s = WORKER_HEARTBEAT_STALE) and the given @claimant_draining. A
// draining claimant passes claimantDraining=true; a live peer passes false.
func (fx *rollAffinityFixture) claim(workerID uuid.UUID, claimantDraining bool) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:         pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:           fx.userID,
		AffinityCutoff:   pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true},
		SpreadCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		ClaimantDraining: claimantDraining,
		IsDockerWorker:   false,
	})
}

// (a) Roll cordon holds the pin: owner W is cordoned for a roll (row present, draining_since
// set, heartbeat FRESH). The run must STAY pinned to W — a live peer P must NOT steal it —
// and W itself (draining claimant, ClaimantDraining=true) re-claims its own promoted run.
func TestClaimRunRollCordonHoldsPinLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 0, true)  // cordoned for a roll: heartbeat-fresh but draining
	p := fx.worker("P", 0, false) // a live, equally-eligible peer
	run := fx.pinnedRun(w, 0)     // just promoted, well within the 30m ceiling

	// A live peer (not draining) must be refused — the pin holds because W's row exists
	// and W is draining (roll), which the M2 NOT EXISTS treats as "can still resume".
	if _, err := fx.claim(p, false); err != pgx.ErrNoRows {
		t.Fatalf("a run whose owner is cordoned for a roll must NOT fall open to a live peer; got %v, want pgx.ErrNoRows", err)
	}
	// Positive control: W (draining claimant) re-claims its OWN promoted run — the whole
	// point of threading @claimant_draining while no longer short-circuiting in workersvc.
	c, err := fx.claim(w, true)
	if err != nil {
		t.Fatalf("the roll-cordoned owner W must re-claim its own pinned run (ClaimantDraining=true): %v", err)
	}
	if c.ID != run {
		t.Fatalf("W claimed %s, want the pinned run %s", c.ID, run)
	}
}

// (b) Draining AND heartbeat-stale still holds the pin: the ~2 min pod-swap edge exceeds the
// 45s @heartbeat_cutoff, so a heartbeat-gated rule would wrongly fall open mid-swap. M2 holds
// on draining REGARDLESS of heartbeat, so the pin survives; W re-claims its own run.
func TestClaimRunDrainingStaleHoldsPinLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, true) // draining AND heartbeat 2m old (past 45s cutoff)
	p := fx.worker("P", 0, false)            // live peer
	run := fx.pinnedRun(w, 0)                // within the 30m ceiling

	if _, err := fx.claim(p, false); err != pgx.ErrNoRows {
		t.Fatalf("a draining+heartbeat-stale owner (pod-swap edge) must still hold the pin; got %v, want pgx.ErrNoRows", err)
	}
	c, err := fx.claim(w, true)
	if err != nil {
		t.Fatalf("owner W must re-claim its own run across the draining+stale pod-swap edge: %v", err)
	}
	if c.ID != run {
		t.Fatalf("W claimed %s, want %s", c.ID, run)
	}
}

// (c) Teardown / death falls open immediately. Two sub-cases, both to a LIVE peer:
//   - the owner is KILLED: heartbeat-stale AND draining_since IS NULL (ADR-628 D3a's
//     protected death/hang case) — the M2 NOT EXISTS finds no still-resuming owner;
//   - the owner ROW is DELETED (teardown): the EXISTS matches no row at all.
func TestClaimRunTeardownFallsOpenLiveDB(t *testing.T) {
	// c1: killed owner (heartbeat-stale, NOT draining).
	t.Run("killed_owner_not_draining", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		w := fx.worker("W", 2*time.Minute, false) // heartbeat 2m old, draining_since NULL: dead
		p := fx.worker("P", 0, false)             // live peer
		run := fx.pinnedRun(w, 0)                 // within the ceiling: only death can release it

		c, err := fx.claim(p, false)
		if err != nil {
			t.Fatalf("a run whose owner is killed (heartbeat-stale, not draining) must fall open to a peer: %v", err)
		}
		if c.ID != run {
			t.Fatalf("peer P claimed %s, want %s", c.ID, run)
		}
	})

	// c2: owner row deleted (teardown).
	t.Run("owner_row_deleted", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		w := fx.worker("W", 0, false) // will be deleted
		p := fx.worker("P", 0, false) // live peer
		run := fx.pinnedRun(w, 0)     // within the ceiling
		fx.deleteWorker(w)            // teardown: the DB row is gone before the kube teardown

		c, err := fx.claim(p, false)
		if err != nil {
			t.Fatalf("a run whose owner row is deleted (teardown) must fall open to a peer: %v", err)
		}
		if c.ID != run {
			t.Fatalf("peer P claimed %s, want %s", c.ID, run)
		}
	})
}

// (d) A draining worker claims NOTHING NEW (PRD #422 D7, the @claimant_draining scoping):
//   - a new/unclaimed run (worker_id IS NULL) must not be claimed by a draining worker;
//   - a run owned by a DIFFERENT worker must not be claimed by a draining worker.
//
// Positive controls confirm the runs are otherwise claimable by a non-draining worker.
func TestClaimRunDrainingClaimantClaimsNothingNewLiveDB(t *testing.T) {
	// d1: unclaimed run — a draining worker must not grab it; a non-draining peer can.
	t.Run("unclaimed_run", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		d := fx.worker("D", 0, true)  // a draining worker polling for work
		p := fx.worker("P", 0, false) // a live, non-draining peer
		run := fx.unclaimedRun()

		if _, err := fx.claim(d, true); err != pgx.ErrNoRows {
			t.Fatalf("a draining worker must NOT claim a new/unclaimed run; got %v, want pgx.ErrNoRows", err)
		}
		// Positive control: the run is genuinely claimable — a non-draining peer takes it.
		c, err := fx.claim(p, false)
		if err != nil {
			t.Fatalf("the unclaimed run must be claimable by a non-draining worker: %v", err)
		}
		if c.ID != run {
			t.Fatalf("peer P claimed %s, want %s", c.ID, run)
		}
	})

	// d2: run owned by a DIFFERENT worker — a draining worker must not steal it via any
	// fallen-open leg; the @claimant_draining clause scopes it to its own runs only.
	t.Run("run_owned_by_other_worker", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		d := fx.worker("D", 0, true)        // draining claimant
		other := fx.worker("O", 0, false)   // a live, non-draining owner
		p := fx.worker("P", 0, false)       // live peer for the positive control
		fx.pinnedRun(other, 31*time.Minute) // past the 30m ceiling: the affinity leg has fallen open...

		// ...but @claimant_draining still forbids the DRAINING worker D from taking it.
		if _, err := fx.claim(d, true); err != pgx.ErrNoRows {
			t.Fatalf("a draining worker must NOT claim a fallen-open run owned by another worker; got %v, want pgx.ErrNoRows", err)
		}
		// Positive control: a non-draining peer P legitimately claims the fallen-open run.
		if _, err := fx.claim(p, false); err != nil {
			t.Fatalf("the fallen-open run must be claimable by a non-draining peer: %v", err)
		}
	})
}
