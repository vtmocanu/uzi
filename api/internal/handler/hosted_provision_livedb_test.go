package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of M2's tests (PRD #58). These exist because the provision
// transaction is unreachable from a fake store — Handler.pool/.q are concrete
// types — and, more importantly, because THE THING M2 MUST GET RIGHT IS INVISIBLE
// TO A FAKE. A fake store cannot exhibit a TOCTOU: it has no snapshot isolation, no
// concurrency semantics, and no advisory locks, so it would return a tidy green
// against a quota guard that lets four workers through on a real Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// hostedLiveDB spins the shared live-DB fixtures: a migrated pool, a Handler wired
// to it, and a seeded user. Every test derives its fixtures from a fresh uuid — the
// runner shares one database across the whole LiveDB set, and workers.token_hash is
// UNIQUE, so fixed literals would collide across tests.
func hostedLiveDB(t *testing.T, quota string) (*Handler, *pgxpool.Pool, *store.Queries, *secretbox.Box, uuid.UUID) {
	t.Helper()
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
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{
		pool:     pool,
		q:        q,
		box:      box,
		cfg:      config.Config{WorkerHostingEnabled: true},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{{Key: settings.KeyHostedWorkerQuota, Value: quota}}}, time.Minute),
	}

	userID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("hosted-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return h, pool, q, box, userID
}

func countHostedWorkers(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE user_id = $1 AND kind = 'hosted'`, userID).Scan(&n); err != nil {
		t.Fatalf("count hosted workers: %v", err)
	}
	return n
}

// The deterministic proof of BOTH lock properties: that the real code takes THAT
// lock with THAT key inside its transaction, AND that it counts after acquiring it.
// No timing, no goroutine race — the two orderings are distinguished by
// construction.
//
// It is worth being precise about why this test has to exist, because the obvious
// reading of the suite is that the race test below covers this. It does not, and
// the numbers say so:
//
//   - Missing lock: the race test detects it 19 times in 20 (the auditor ran it 20
//     times). That 1-in-20 false green is the whole reason a deterministic test was
//     mandated — an N-goroutine race is evidence, not proof.
//   - Lock present but taken AFTER the count: caught only ~83% of the time (red in 5
//     of 5 isolated sweeps, but green once in a full run-store-it.sh run, landing on
//     exactly 2 — one false green in six observations). This ordering is the subtler
//     defect and had the weaker guard, which is backwards.
//
// The mechanism: hold the lock for user u on a separate connection, so a correct
// provision MUST block. While it is blocked, insert u's full quota directly. Then
// release. Now the two orderings answer differently, and neither can get lucky:
//
//	correct (lock, then count) -> counts AFTER acquiring -> sees the quota -> refuses
//	broken  (count, then lock) -> already counted 0 before blocking -> inserts anyway
//
// So: no refusal means the count was taken outside the critical section, and a
// provision that never blocks at all means the lock is missing or mis-keyed. One
// test, both failure modes, deterministically. QuotaRaceLiveDB below is kept as the
// end-to-end sanity check on the real concurrent path, not as the proof.
func TestProvisionHostedWorkerLockIsHeldLiveDB(t *testing.T) {
	const quota = 2
	h, pool, _, _, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	// Hold the lock on our own connection, outside the handler.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HostedProvisionLockClass, hostedProvisionLockObjID(userID)); err != nil {
		t.Fatalf("take holder lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.provisionHostedWorker(ctx, userID, "blocked", "base", "m", false, quota)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("provisionHostedWorker completed (err=%v) while the per-user advisory lock was "+
			"held by another transaction — it is not taking pg_advisory_xact_lock("+
			"HostedProvisionLockClass, objid(user)) inside its tx, so the quota count is "+
			"unserialized and the guard is decorative", err)
	case <-time.After(500 * time.Millisecond):
		// Correct: it is waiting on the lock. A lock-after-count implementation also
		// waits here — it has just already taken its count, which is what the rest of
		// this test detects.
	}

	// Fill the user's quota from outside, while the provision sits blocked. Raw SQL
	// on the pool: it takes no advisory lock, so it cannot deadlock against the
	// holder, and it deliberately bypasses the handler (which would block too).
	for i := 0; i < quota; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size)
			 VALUES ($1, $2, $3, 'base', 'hosted', 'm')`,
			userID, fmt.Sprintf("preexisting-%d", i),
			append([]byte(fmt.Sprintf("lockorder-%d-", i)), userID[:]...)); err != nil {
			t.Fatalf("seed pre-existing hosted worker %d: %v", i, err)
		}
	}

	// Release. The blocked provision now acquires the lock and proceeds.
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder tx: %v", err)
	}
	select {
	case err := <-done:
		// THE assertion. Under READ COMMITTED each statement takes a fresh snapshot, so
		// a count issued after the lock is acquired sees the rows committed above —
		// which is exactly the property that makes the lock work at this isolation
		// level.
		if !errors.Is(err, errHostedQuotaExceeded) {
			t.Fatalf("provision returned %v, want errHostedQuotaExceeded — the user's quota was "+
				"filled while this provision sat blocked on the lock, so a provision that counts "+
				"AFTER acquiring the lock must see %d workers and refuse. Counting BEFORE the "+
				"lock (or outside it) reads the pre-block 0 and inserts anyway, which is this "+
				"failure exactly: the lock is taken but guards nothing.", err, quota)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provisionHostedWorker never completed after the lock was released")
	}
	if n := countHostedWorkers(ctx, t, pool, userID); n != quota {
		t.Errorf("hosted workers = %d, want %d — a provision that should have been refused "+
			"inserted one anyway", n, quota)
	}
}

// THE headline test of this milestone. Decision 8 requires quota enforcement to be
// atomic with no TOCTOU; this is what tells us whether it is.
//
// The count is a snapshot read and is NOT sufficient by itself: under READ COMMITTED
// two concurrent provisions both read N-1, neither sees the other's uncommitted row,
// and both insert. (A guarded `INSERT … WHERE count < quota` has exactly the same
// hole — it was tried and dropped for reading as though it did not.) The advisory
// lock is what serializes them. Measured: with the lock removed, all 8 racers below
// succeed against a quota of 2.
//
// THIS TEST IS EVIDENCE, NOT PROOF, and an earlier version of this comment claimed
// otherwise. It is a real concurrent exercise of the real path, which is worth
// having — but a race that has to be lost to be observed can always fail to happen.
// Measured rather than reasoned, on both defects it is supposed to see: a missing
// lock slips through ~1 run in 20, and a lock taken after the count slips through
// ~1 in 6 (green once in six observations, landing on exactly 2). Neither number is
// zero, so neither defect may be left to this test alone.
//
// TestProvisionHostedWorkerLockIsHeldLiveDB above is the deterministic proof of both,
// by construction. This one stays because it is the only test that exercises genuine
// concurrency end-to-end, and because a 19-in-20 detector is a fine second opinion —
// it is simply not the guard.
func TestProvisionHostedWorkerQuotaRaceLiveDB(t *testing.T) {
	const quota = 2
	const racers = 8
	h, pool, _, _, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, to actually overlap the transactions
			_, err := h.provisionHostedWorker(ctx, userID, fmt.Sprintf("racer-%d", i), "base", "m", false, quota)
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, refused int
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errHostedQuotaExceeded):
			refused++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if ok != quota {
		t.Errorf("%d provisions succeeded, want exactly %d — the quota guard is not atomic", ok, quota)
	}
	if refused != racers-quota {
		t.Errorf("%d provisions refused, want %d", refused, racers-quota)
	}
	// The database is the real assertion: whatever the handlers each believed, the
	// user must not be over quota.
	if n := countHostedWorkers(ctx, t, pool, userID); n != quota {
		t.Errorf("user holds %d hosted workers, want %d", n, quota)
	}
}

// The lock is keyed per user, not globally (that is the one refinement over
// registration's single-key lock). If the key ever collapses to a constant, this
// still passes — but if it were keyed wrongly such that users SHARE a quota, this
// fails. Cheap insurance on a subtle key derivation.
func TestProvisionHostedWorkerQuotaIsPerUserLiveDB(t *testing.T) {
	h, pool, _, _, userA := hostedLiveDB(t, "2")
	ctx := context.Background()

	userB := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userB, fmt.Sprintf("hosted-b-%s@e2e", userB)); err != nil {
		t.Fatalf("seed user B: %v", err)
	}

	// A fills their quota.
	for i := 0; i < 2; i++ {
		if _, err := h.provisionHostedWorker(ctx, userA, fmt.Sprintf("a-%d", i), "base", "m", false, 2); err != nil {
			t.Fatalf("provision for user A: %v", err)
		}
	}
	if _, err := h.provisionHostedWorker(ctx, userA, "a-over", "base", "m", false, 2); !errors.Is(err, errHostedQuotaExceeded) {
		t.Fatalf("user A over quota: err = %v, want errHostedQuotaExceeded", err)
	}
	// B is unaffected: the quota is per user.
	if _, err := h.provisionHostedWorker(ctx, userB, "b-0", "base", "m", false, 2); err != nil {
		t.Fatalf("provision for user B must be unaffected by user A's quota: %v", err)
	}
	if n := countHostedWorkers(ctx, t, pool, userB); n != 1 {
		t.Errorf("user B holds %d hosted workers, want 1", n)
	}
}

// The pinned auditor constraint, made mechanical: workers.token_hash and the parked
// sealed plaintext are written by the SAME transaction, so "the hash I proved is
// still current" ≡ "the parked ciphertext is the token I proved".
// MarkHostedWorkerTokenDelivered's `AND token_hash = @proved_token_hash` predicate
// rests entirely on that equivalence, and nothing else in the codebase would notice
// if it broke — a split would leave a worker holding a token_hash whose plaintext
// was never queued, and every other test would stay green.
//
// It is proved end-to-end through exported API only: Poll is the honest way to open
// the ciphertext (tokenAAD is unexported), and it is also the real path the
// controller takes.
func TestProvisionHostedWorkerCoWriteLiveDB(t *testing.T) {
	h, _, q, box, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	// docker=true here, so this test doubles as the end-to-end docker-flag proof
	// (PRD #83 M3): the boolean must survive provision -> row -> Poll -> wire, which
	// is the whole of what the api owes the controller for docker.
	wkr, err := h.provisionHostedWorker(ctx, userID, "co-write", "base", "l", true, 2)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !wkr.DockerEnabled.Valid || !wkr.DockerEnabled.Bool {
		t.Fatalf("docker_enabled = %+v on the provisioned row, want an explicit true", wkr.DockerEnabled)
	}

	resp, err := hostedsvc.New(q, box, time.Now, time.Hour).Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var found *hostedsvc.DesiredWorker
	for i := range resp.Workers {
		if resp.Workers[i].ID == wkr.ID.String() {
			found = &resp.Workers[i]
			break
		}
	}
	if found == nil {
		t.Fatal("the provisioned worker is absent from the controller's desired state")
	}
	if found.JoinToken == nil {
		t.Fatal("no join token parked for a freshly provisioned worker — the seal did not " +
			"land in the provision transaction, so this worker can never be delivered one")
	}
	// The equivalence itself.
	if !jointoken.Equal(jointoken.Hash(*found.JoinToken), wkr.TokenHash) {
		t.Fatal("the parked ciphertext is NOT the token whose hash is on the worker row — " +
			"token_hash and the sealed plaintext were not co-written, and " +
			"MarkHostedWorkerTokenDelivered's proved-hash predicate is now unsound")
	}
	// The desired state the controller renders from, while we are here.
	if found.Template != "base" || found.Size != "l" {
		t.Errorf("desired state = template %q size %q, want base/l", found.Template, found.Size)
	}
	// The docker flag reached the wire: a controller polling this sees Docker=true and
	// renders the privileged DinD sidecar. If this drops to false the sidecar silently
	// never renders and the worker's `docker` is inert.
	if !found.Docker {
		t.Error("desired state Docker=false for a worker provisioned with docker=true — " +
			"the flag did not survive to the controller poll, so the sidecar never renders")
	}
}

// Nothing is sealed when the quota refuses: no worker, no parked ciphertext.
//
// It does NOT prove the rollback, and an earlier version of this comment claimed it
// did. The refusal returns BEFORE jointoken.Generate(), so no token is ever minted
// and there is nothing to roll back — this test would pass with the rollback
// entirely broken. What it actually pins is better than rollback coverage: that the
// refusal happens before anything is created at all. Refusing before minting beats
// minting and unwinding, because it cannot half-fail.
//
// The genuine both-writes case (the insert succeeds and SealJoinToken then fails) is
// untested and stays that way: reaching it needs a fault-injection seam in the tx
// body, and that seam is a worse thing to own than the gap. The gap is small on
// purpose — an un-rolled-back insert would leave a token_hash with no parked
// ciphertext, so the worker is simply never delivered a token, reads offline, and is
// recovered by delete + reprovision. Not a disclosure, and self-announcing.
func TestProvisionHostedWorkerRefusalSealsNothingLiveDB(t *testing.T) {
	h, pool, _, _, userID := hostedLiveDB(t, "0")
	ctx := context.Background()

	if _, err := h.provisionHostedWorker(ctx, userID, "refused", "base", "m", false, 0); !errors.Is(err, errHostedQuotaExceeded) {
		t.Fatalf("err = %v, want errHostedQuotaExceeded", err)
	}
	if n := countHostedWorkers(ctx, t, pool, userID); n != 0 {
		t.Errorf("user holds %d hosted workers after a refusal, want 0", n)
	}
	var tokens int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM hosted_worker_tokens t JOIN workers w ON w.id = t.worker_id WHERE w.user_id = $1`,
		userID).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 0 {
		t.Errorf("%d sealed tokens parked after a refused provision, want 0", tokens)
	}
}

// Decision 11 for a hosted worker, through the real (kind-blind) delete path: no
// sealed plaintext outlives the worker it belongs to. The FK cascade is what
// guarantees it, which is worth pinning — a future migration that recreated this
// table without ON DELETE CASCADE would strand ciphertext for a worker that no
// longer exists, and nothing else would notice.
func TestDeleteHostedWorkerCascadesPendingTokenLiveDB(t *testing.T) {
	h, pool, q, box, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	wkr, err := h.provisionHostedWorker(ctx, userID, "doomed", "base", "m", false, 2)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosted_worker_tokens WHERE worker_id = $1`, wkr.ID).Scan(&before); err != nil {
		t.Fatalf("count token row: %v", err)
	}
	if before != 1 {
		t.Fatalf("token rows before delete = %d, want 1", before)
	}

	svc := workersvc.New(q, box, workersvc.Params{})
	if err := svc.DeleteWorker(ctx, userID, wkr.ID); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosted_worker_tokens WHERE worker_id = $1`, wkr.ID).Scan(&after); err != nil {
		t.Fatalf("count token row: %v", err)
	}
	if after != 0 {
		t.Error("the worker's sealed join token outlived the worker — the FK cascade is gone")
	}
	// And it leaves the controller's desired state. The poll returns the fleet as a
	// SET, so a vanished row IS the teardown cue (Decision 9/11) — there is no delete
	// signal on the wire, which is exactly why this has to be asserted rather than
	// assumed.
	resp, err := hostedsvc.New(q, box, time.Now, time.Hour).Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, dw := range resp.Workers {
		if dw.ID == wkr.ID.String() {
			t.Error("a deleted hosted worker is still in the controller's desired state — " +
				"the controller would keep reconciling its Deployment forever")
		}
	}
	// And it frees quota: delete + reprovision is v1's recovery for a stranded
	// worker (there is no rotation endpoint), so it has to actually work.
	if _, err := h.provisionHostedWorker(ctx, userID, "replacement", "base", "m", false, 2); err != nil {
		t.Fatalf("reprovision after delete: %v", err)
	}
}

// TestProvisionHostedWorkerBindModeLiveDB pins PRD #1140 M1 for the HOSTED provision
// path against real Postgres: a freshly provisioned hosted worker defaults to
// anthropic_bind_mode 'auto' ONLY when its owner has a non-empty auto-select pool
// (≥1 auto_eligible anthropic_token), and 'default' otherwise. There is no label input
// on this route, so the derivation is two-way (auto or default) — no pinned case.
//
// It mirrors TestEphemeralProvisionBindModeLiveDB (ephemeral_provisioner_livedb_test.go)
// for the hosted-provision lane: the handler reads UserHasAutoEligibleAnthropicToken
// under the same qtx it inserts with and writes the derived mode into
// CreateHostedWorker's INSERT (D3), so an empty pool never ships an auto worker whose
// run would then park in pool_wait. The assertion reads wkr.AnthropicBindMode, the
// effective mode the provision response carries.
func TestProvisionHostedWorkerBindModeLiveDB(t *testing.T) {
	insertToken := func(q *store.Queries, ctx context.Context, userID uuid.UUID, label string, first bool) uuid.UUID {
		t.Helper()
		row, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: userID, Kind: store.KindAnthropicToken, Label: label, WantDefault: first,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert token %s: %v", label, err)
		}
		return row.ID
	}

	// An owner with ≥1 auto_eligible token (a born-eligible first token) → non-empty
	// pool → auto.
	t.Run("pooled_token_auto", func(t *testing.T) {
		h, _, q, _, userID := hostedLiveDB(t, "5")
		ctx := context.Background()
		insertToken(q, ctx, userID, "default", true) // first token, born auto_eligible (#804)

		wkr, err := h.provisionHostedWorker(ctx, userID, "pooled", "base", "m", false, 5)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if wkr.AnthropicBindMode != workersvc.BindModeAuto {
			t.Errorf("anthropic_bind_mode = %q, want %q (owner has ≥1 auto_eligible token)", wkr.AnthropicBindMode, workersvc.BindModeAuto)
		}
	})

	// An owner whose SOLE token was opted out of the pool → empty pool → default, so the
	// provisioned worker's run is never parked in pool_wait.
	t.Run("empty_pool_default", func(t *testing.T) {
		h, _, q, _, userID := hostedLiveDB(t, "5")
		ctx := context.Background()
		tok := insertToken(q, ctx, userID, "default", true) // born eligible, then opted out below
		if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
			ID: tok, UserID: userID, Kind: store.KindAnthropicToken, AutoEligible: false,
		}); err != nil {
			t.Fatalf("opt token out: %v", err)
		}

		wkr, err := h.provisionHostedWorker(ctx, userID, "empty", "base", "m", false, 5)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if wkr.AnthropicBindMode != workersvc.BindModeDefault {
			t.Errorf("anthropic_bind_mode = %q, want %q (empty auto-select pool must not select auto)", wkr.AnthropicBindMode, workersvc.BindModeDefault)
		}
	})

	// A single-token owner, eligible purely via the born-eligible first-token insert (no
	// toggle) → auto. The headline #804 case: single-token users get auto for free.
	t.Run("single_born_eligible_auto", func(t *testing.T) {
		h, _, q, _, userID := hostedLiveDB(t, "5")
		ctx := context.Background()
		insertToken(q, ctx, userID, "default", true) // sole token, born auto_eligible via insert

		wkr, err := h.provisionHostedWorker(ctx, userID, "single", "base", "m", false, 5)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if wkr.AnthropicBindMode != workersvc.BindModeAuto {
			t.Errorf("anthropic_bind_mode = %q, want %q (single born-eligible token)", wkr.AnthropicBindMode, workersvc.BindModeAuto)
		}
	})
}

// Decision 11's refusal covers hosted workers too, and a refused delete must not
// destroy the pending token. The rule is kind-blind, so this asserts nothing new
// about the code — it pins the decision against a future hosted-specific delete
// path that forgets the refusal, which is a named M2 acceptance criterion.
func TestDeleteHostedWorkerRefusedWhileBusyLiveDB(t *testing.T) {
	h, pool, q, box, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	wkr, err := h.provisionHostedWorker(ctx, userID, "busy", "base", "m", false, 2)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	connID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running', $4)`, runID, userID, repoID, wkr.ID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	svc := workersvc.New(q, box, workersvc.Params{})
	if err := svc.DeleteWorker(ctx, userID, wkr.ID); !errors.Is(err, workersvc.ErrWorkerHasActiveRuns) {
		t.Fatalf("err = %v, want ErrWorkerHasActiveRuns (a hosted worker holding a non-terminal run must not be deleted)", err)
	}
	if n := countHostedWorkers(ctx, t, pool, userID); n != 1 {
		t.Errorf("hosted workers after a refused delete = %d, want 1", n)
	}
}
