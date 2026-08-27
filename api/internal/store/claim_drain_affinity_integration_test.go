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

// Drain-aware resume affinity (PRD #628 M1 / ADR-628 D3a) against a REAL Postgres.
// ClaimRun's affinity leg no longer pins a promoted run to its prior worker W for a
// fixed 2-minute window. It now pins ONLY while W is a live, non-draining claim target
// (the NOT EXISTS worker-liveness leg, reusing the same @heartbeat_cutoff the fleet
// spread uses), and a generous ceiling (@affinity_cutoff = now-WORKER_AFFINITY_CEILING,
// 30m) bounds the one live-but-wedged pathology a pure liveness test would strand.
//
// The four cases below drive the real params: @affinity_cutoff = now-30m (the ceiling)
// and @heartbeat_cutoff = now-45s (WORKER_HEARTBEAT_STALE), exactly as service.go wires
// them. Each has a positive control — a worker actually claims the run (or W reclaims
// its own) — so none passes vacuously.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store e2e
// runner e2e/run-store-it.sh provides one).

// drainAffinityFixture is a per-test live-DB harness: one user + forge_connection + repo,
// plus helpers to seed workers whose heartbeat and draining_since the test controls, seed
// a queued run pinned to a chosen worker with a chosen updated_at age, and claim as a
// given worker with the real M1 cutoffs.
type drainAffinityFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
	iid    int64
}

func newDrainAffinityFixture(t *testing.T) *drainAffinityFixture {
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

	fx := &drainAffinityFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New()}
	connID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		fx.userID, fmt.Sprintf("drain-affinity-%s@e2e", fx.userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, fx.userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, fx.repoID, connID)
	return fx
}

func (fx *drainAffinityFixture) nextIID() int64 { fx.iid++; return fx.iid }

// worker inserts a cap-2, non-docker, online worker whose last_heartbeat_at is stamped
// heartbeatAge in the PAST and whose draining_since is set iff draining. A fresh worker
// (heartbeatAge = 0, draining = false) is a live, non-draining claim target.
func (fx *drainAffinityFixture) worker(name string, heartbeatAge time.Duration, draining bool) uuid.UUID {
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

// pinnedRun inserts a queued repo-bearing run owned by ownerID (worker_id = ownerID) whose
// updated_at is stamped updatedAge in the PAST, and returns its id. updatedAge = 0 models a
// just-promoted run (within the ceiling); a value past the 30m ceiling models the wedged case.
func (fx *drainAffinityFixture) pinnedRun(ownerID uuid.UUID, updatedAge time.Duration) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, updated_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, now() - make_interval(secs => $6))`,
		id, fx.userID, fx.repoID, fx.nextIID(), ownerID, updatedAge.Seconds())
	return id
}

// claim runs ClaimRun as workerID with the real M1 cutoffs: @affinity_cutoff = now-30m
// (the WORKER_AFFINITY_CEILING) and @heartbeat_cutoff = now-45s (WORKER_HEARTBEAT_STALE).
func (fx *drainAffinityFixture) claim(workerID uuid.UUID) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:        pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:          fx.userID,
		AffinityCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true},
		SpreadCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:  false,
	})
}

// (a) Pinned while live: a queued run owned by W (updated_at recent, within the ceiling)
// where W is heartbeat-fresh and non-draining. A different eligible peer P must NOT get it
// (the liveness leg keeps it pinned); W itself DOES reclaim it (resume affinity).
func TestClaimRunPinnedToLiveOwnerLiveDB(t *testing.T) {
	fx := newDrainAffinityFixture(t)
	w := fx.worker("W", 0, false) // live, non-draining
	p := fx.worker("P", 0, false) // an equally-eligible, live peer
	run := fx.pinnedRun(w, 0)     // just promoted: updated_at = now(), well within the 30m ceiling

	// Peer P is blocked purely by the affinity leg: worker_id != P, W is a live non-draining
	// claim target (NOT EXISTS is false), and updated_at is not past the ceiling.
	if _, err := fx.claim(p); err != pgx.ErrNoRows {
		t.Fatalf("a run pinned to a live, non-draining owner must NOT fall open to a peer; got %v, want pgx.ErrNoRows", err)
	}
	// Positive control: W reclaims its own run via resume affinity (worker_id = @worker_id).
	c, err := fx.claim(w)
	if err != nil {
		t.Fatalf("the live owner W must reclaim its own pinned run: %v", err)
	}
	if c.ID != run {
		t.Fatalf("W claimed %s, want the pinned run %s", c.ID, run)
	}
}

// (b) Falls open when the owner's heartbeat is stale: same run (updated_at recent, within
// the ceiling) but W's last_heartbeat_at is older than @heartbeat_cutoff. The liveness leg
// (not the ceiling) releases it, so peer P claims — proving heartbeat staleness alone frees
// the pin even though the run is well within the 30m ceiling.
func TestClaimRunFallsOpenWhenOwnerHeartbeatStaleLiveDB(t *testing.T) {
	fx := newDrainAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false) // heartbeat 2m old: past the 45s @heartbeat_cutoff
	p := fx.worker("P", 0, false)             // live, eligible peer
	run := fx.pinnedRun(w, 0)                 // updated_at recent: the ceiling has NOT released it

	c, err := fx.claim(p)
	if err != nil {
		t.Fatalf("a run whose owner is heartbeat-stale must fall open to a peer (liveness leg, not the ceiling): %v", err)
	}
	if c.ID != run {
		t.Fatalf("peer P claimed %s, want %s", c.ID, run)
	}
}

// (c) Falls open when the owner is draining: W is heartbeat-fresh but draining_since is set,
// so it will never resume the run. The liveness leg releases the pin (a draining worker is
// excluded from the NOT EXISTS), so peer P claims.
func TestClaimRunFallsOpenWhenOwnerDrainingLiveDB(t *testing.T) {
	fx := newDrainAffinityFixture(t)
	w := fx.worker("W", 0, true) // heartbeat-fresh but draining
	p := fx.worker("P", 0, false)
	run := fx.pinnedRun(w, 0) // updated_at recent: only the draining state can release it

	c, err := fx.claim(p)
	if err != nil {
		t.Fatalf("a run whose owner is draining must fall open to a peer (draining owner excluded from the liveness leg): %v", err)
	}
	if c.ID != run {
		t.Fatalf("peer P claimed %s, want %s", c.ID, run)
	}
}

// (d) Ceiling releases a live-but-stuck owner: W is a live, non-draining claim target, so the
// liveness leg keeps it pinned — but the run's updated_at is older than @affinity_cutoff (past
// the 30m ceiling), so the ceiling OR-arm frees it and peer P claims. This is the bounded
// live-but-wedged case the ceiling exists to cover.
func TestClaimRunCeilingReleasesWedgedLiveOwnerLiveDB(t *testing.T) {
	fx := newDrainAffinityFixture(t)
	w := fx.worker("W", 0, false)          // live, non-draining: the liveness leg would keep it pinned
	p := fx.worker("P", 0, false)          // live, eligible peer
	run := fx.pinnedRun(w, 31*time.Minute) // updated_at past the 30m ceiling

	c, err := fx.claim(p)
	if err != nil {
		t.Fatalf("a run past the affinity ceiling must fall open to a peer even when its owner is live: %v", err)
	}
	if c.ID != run {
		t.Fatalf("peer P claimed %s, want %s", c.ID, run)
	}
}
