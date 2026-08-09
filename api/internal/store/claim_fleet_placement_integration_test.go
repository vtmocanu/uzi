package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Fleet-aware ClaimRun (PRD #216 M2) against a REAL Postgres — the placement
// behaviour the fake-store unit tests cannot cover because they never run the SQL:
// the spread clause (a busy worker DEFERS a queued fresh run to a strictly-less-loaded,
// live, eligible peer with a free slot), its bypasses (resume affinity, a run older
// than @spread_cutoff, a NULL claimer cap, a 0 claimer-active-count), the peer
// eligibility sharing the claimant's fn_worker_can_claim expression (00113), and the
// FOR UPDATE SKIP LOCKED no-double-claim guarantee under concurrency.
//
// "Strictly less loaded" is the integer cross-multiply peer.active*my.cap <
// my.active*peer.cap (no float ties). Active = runs with status IN
// ('claimed','running','awaiting_approval','awaiting_input') AND kind <> 'chat'.
//
// Every test here is skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; the store e2e runner (e2e/run-store-it.sh) provides one.

// fleetFixture is a per-test live-DB harness: one user + forge_connection + repo, plus
// helpers to seed workers (cap/docker/heartbeat under the test's control), hold N active
// runs on a worker, create fresh queued runs, and claim as a given worker.
type fleetFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
	iid    int64
}

func newFleetFixture(t *testing.T) *fleetFixture {
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

	fx := &fleetFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New()}
	connID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		fx.userID, fmt.Sprintf("fleet-%s@e2e", fx.userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, fx.userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, fx.repoID, connID)
	return fx
}

// worker inserts an online worker under the fixture's user. A nil cap seeds a NULL
// max_concurrent_runs (never a deferral target); docker toggles docker_enabled so the
// peer-eligibility path can be exercised.
func (fx *fleetFixture) worker(name string, cap *int32, docker bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, docker_enabled)
		 VALUES ($1, $2, $3, $4, 'online', now(), $5, $6)`,
		id, fx.userID, name, tokenHash(), cap, docker)
	return id
}

func (fx *fleetFixture) nextIID() int64 { fx.iid++; return fx.iid }

// holdActive makes workerID hold n ACTIVE (status='running') repo-bearing runs.
func (fx *fleetFixture) holdActive(workerID uuid.UUID, n int) {
	fx.t.Helper()
	for i := 0; i < n; i++ {
		id := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5)`,
			id, fx.userID, fx.repoID, fx.nextIID(), workerID)
	}
}

// queuedRun inserts a fresh (updated_at = now()) queued, unowned repo-bearing run and
// returns its id.
func (fx *fleetFixture) queuedRun() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', NULL)`,
		id, fx.userID, fx.repoID, fx.nextIID())
	return id
}

func (fx *fleetFixture) claim(workerID uuid.UUID, isDocker bool, allow []uuid.UUID) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allow,
	})
}

// activeCount reads the same active-run definition the spread clause uses.
func (fx *fleetFixture) activeCount(workerID uuid.UUID) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM runs WHERE worker_id = $1
		   AND status IN ('claimed','running','awaiting_approval','awaiting_input') AND kind <> 'chat'`,
		workerID).Scan(&n); err != nil {
		fx.t.Fatalf("activeCount: %v", err)
	}
	return n
}

func capOf(n int32) *int32 { return &n }

// (a1) Steady-state spread: two idle cap-2 workers, two fresh queued runs. A claims one,
// then DEFERS the second to still-idle B (B is strictly less loaded), and B claims it.
// The two claims are distinct and cover both runs.
func TestClaimRunSpreadsAcrossIdlePeersLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	b := fx.worker("B", capOf(2), false)
	r1, r2 := fx.queuedRun(), fx.queuedRun()

	c1, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("A's first claim must succeed (both workers idle): %v", err)
	}
	// A now holds 1; idle peer B (0 active) is strictly less loaded (0*2 < 1*2) and has a
	// free slot, so A must DEFER rather than pile a second run on itself.
	if _, err := fx.claim(a, false, nil); err != pgx.ErrNoRows {
		t.Fatalf("A holding 1 must defer to idle peer B; got %v, want pgx.ErrNoRows", err)
	}
	c2, err := fx.claim(b, false, nil)
	if err != nil {
		t.Fatalf("B must claim the deferred run: %v", err)
	}
	if c1.ID == c2.ID {
		t.Fatalf("A and B claimed the same run %s", c1.ID)
	}
	got := map[uuid.UUID]bool{c1.ID: true, c2.ID: true}
	if !got[r1] || !got[r2] {
		t.Fatalf("claims %s,%s do not cover both runs %s,%s", c1.ID, c2.ID, r1, r2)
	}
}

// (a2) D8 no-livelock: a busy cap-4 worker (2 active) does NOT defer to a peer with no
// free slot. B is cap 1 holding 1 (1 < 1 is false), so A claims the run itself.
func TestClaimRunHeterogeneousNoLivelockLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(4), false)
	b := fx.worker("B", capOf(1), false)
	fx.holdActive(a, 2)
	fx.holdActive(b, 1)
	run := fx.queuedRun()

	c, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("A must claim: peer B has no free slot (1<1 false), so A does not defer; got %v", err)
	}
	if c.ID != run {
		t.Fatalf("claimed %s, want %s", c.ID, run)
	}
}

// (a3) D7: a peer with a NULL cap is never a deferral target, so a busy worker still
// claims.
func TestClaimRunNullCapPeerStillClaimsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	fx.worker("B", nil, false) // NULL max_concurrent_runs
	fx.holdActive(a, 1)
	run := fx.queuedRun()

	c, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("A must claim: a NULL-cap peer is never a deferral target; got %v", err)
	}
	if c.ID != run {
		t.Fatalf("claimed %s, want %s", c.ID, run)
	}
}

// (a4) D7: a single-worker fleet always claims (no peers to defer to), guaranteeing
// claimability.
func TestClaimRunSingleWorkerFleetClaimsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	fx.holdActive(a, 1)
	run := fx.queuedRun()

	c, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("a single-worker fleet must claim its own queued run; got %v", err)
	}
	if c.ID != run {
		t.Fatalf("claimed %s, want %s", c.ID, run)
	}
}

// (a5) D4: a run older than @spread_cutoff BYPASSES the spread. A busy worker that would
// normally defer to an idle peer claims the OLD run itself, so the spread can never make
// a run unclaimable.
func TestClaimRunPastGraceExemptLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	fx.worker("B", capOf(2), false) // idle peer A would normally defer to
	fx.holdActive(a, 1)
	run := fx.queuedRun()
	// Backdate updated_at past @spread_cutoff (now-9s) so the spread is bypassed.
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET updated_at = now() - interval '60 seconds' WHERE id = $1`, run)

	c, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("a run older than @spread_cutoff must bypass the spread and be claimed; got %v", err)
	}
	if c.ID != run {
		t.Fatalf("claimed %s, want %s", c.ID, run)
	}
}

// (a6) A claimer holding 0 active runs never defers (its active count is 0, so the
// cross-multiply RHS is 0 and no peer can be strictly less loaded).
func TestClaimRunIdleClaimerNeverDefersLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	fx.worker("B", capOf(2), false) // an equally-idle peer
	run := fx.queuedRun()

	c, err := fx.claim(a, false, nil)
	if err != nil {
		t.Fatalf("an idle claimer (0 active) never defers; got %v", err)
	}
	if c.ID != run {
		t.Fatalf("claimed %s, want %s", c.ID, run)
	}
}

// (b7) D5 differential eligibility: the peer clause must use the SAME fn_worker_can_claim
// as the claimant, keyed on the SAME @docker_repo_allowlist. A non-docker worker defers a
// repo-R run to a strictly-less-loaded DOCKER peer ONLY when R is on the allowlist (the
// peer is then eligible); when R is off the allowlist the docker peer is not a valid
// target and the claimant takes the run itself — never a wrong deferral into an
// unclaimable state.
func TestClaimRunDockerPeerEligibilityLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false) // non-docker claimant
	b := fx.worker("B", capOf(2), true)  // docker peer
	fx.holdActive(a, 1)                  // A busy, would defer to a less-loaded eligible peer

	// Allowlisted: docker peer B IS eligible for repo R and strictly less loaded, so A defers.
	run1 := fx.queuedRun()
	if _, err := fx.claim(a, false, []uuid.UUID{fx.repoID}); err != pgx.ErrNoRows {
		t.Fatalf("A must defer to the docker peer eligible for the allowlisted repo; got %v, want pgx.ErrNoRows", err)
	}
	// B then claims it (docker worker, repo on allowlist) — the deferral had a real taker.
	cB, err := fx.claim(b, true, []uuid.UUID{fx.repoID})
	if err != nil {
		t.Fatalf("docker peer B must claim the allowlisted run: %v", err)
	}
	if cB.ID != run1 {
		t.Fatalf("docker peer claimed %s, want %s", cB.ID, run1)
	}

	// Non-allowlisted: with an empty allowlist the docker peer is NOT eligible for R, so it
	// is not a deferral target and A claims the run itself.
	run2 := fx.queuedRun()
	c, err := fx.claim(a, false, []uuid.UUID{})
	if err != nil {
		t.Fatalf("A must claim when the docker peer is NOT eligible for the (non-allowlisted) repo; got %v", err)
	}
	if c.ID != run2 {
		t.Fatalf("claimed %s, want %s", c.ID, run2)
	}
}

// (c9) Concurrency: 3 cap-2 workers race for 5 fresh queued runs. FOR UPDATE SKIP LOCKED
// guarantees no run is claimed twice. A worker may stop early (it deferred and got
// ErrNoRows while runs remain), so after the race a single-threaded settling sweep drains
// the rest: all 5 end claimed, none double-claimed, and no worker exceeds its cap.
func TestClaimRunConcurrentNoDoubleClaimLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	workers := []uuid.UUID{
		fx.worker("A", capOf(2), false),
		fx.worker("B", capOf(2), false),
		fx.worker("C", capOf(2), false),
	}
	runIDs := map[uuid.UUID]bool{}
	for i := 0; i < 5; i++ {
		runIDs[fx.queuedRun()] = true
	}

	var mu sync.Mutex
	claimedBy := map[uuid.UUID]uuid.UUID{} // runID -> workerID
	record := func(w, r uuid.UUID) {
		mu.Lock()
		defer mu.Unlock()
		if prev, ok := claimedBy[r]; ok {
			t.Errorf("run %s double-claimed by %s and %s (FOR UPDATE SKIP LOCKED must prevent this)", r, prev, w)
		}
		claimedBy[r] = w
	}

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w uuid.UUID) {
			defer wg.Done()
			for {
				r, err := fx.claim(w, false, nil)
				if err == pgx.ErrNoRows {
					return
				}
				if err != nil {
					t.Errorf("concurrent claim by %s: %v", w, err)
					return
				}
				record(w, r.ID)
			}
		}(w)
	}
	wg.Wait()

	// Settling sweep: the spread can leave runs queued when every worker deferred, so drain
	// single-threaded until a full pass over all workers claims nothing more. The
	// minimum-loaded worker never defers, so this converges while any run is claimable.
	for {
		progress := false
		for _, w := range workers {
			for {
				r, err := fx.claim(w, false, nil)
				if err == pgx.ErrNoRows {
					break
				}
				if err != nil {
					t.Fatalf("settling claim by %s: %v", w, err)
				}
				record(w, r.ID)
				progress = true
			}
		}
		if !progress {
			break
		}
	}

	if len(claimedBy) != len(runIDs) {
		t.Fatalf("want all %d runs claimed, got %d", len(runIDs), len(claimedBy))
	}
	for r := range runIDs {
		if _, ok := claimedBy[r]; !ok {
			t.Fatalf("run %s left unclaimable after a full settling sweep", r)
		}
	}
	for _, w := range workers {
		if n := fx.activeCount(w); n > 2 {
			t.Fatalf("worker %s holds %d active runs, exceeding its cap of 2", w, n)
		}
	}
}
