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

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
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

// The deterministic proof that the real code takes THAT lock with THAT key inside
// its transaction. Every other test in M2 passes whether or not the lock statement
// exists — including the race test below, which is timing-dependent in principle.
// This one is not: it works by contradiction. Hold the advisory lock for user u on a
// separate connection, and a correct provisionHostedWorker MUST block. If it
// completes anyway, the lock it takes is not this one — dropped, or keyed
// differently.
//
// WHAT THIS TEST DOES NOT CATCH, stated precisely because the obvious reading is
// wrong and a future reader will otherwise trust it too far: it does NOT catch the
// lock being taken AFTER the count. That ordering still blocks here (the provision
// counts, then waits on the lock, then proceeds), so this test passes while the
// quota is entirely unprotected — the count would have been taken outside the
// critical section. Verified by mutation, not reasoned: moving the lock below the
// count leaves this test green and turns QuotaRaceLiveDB red (8 of 8 racers pass a
// quota of 2).
//
// So the two tests divide the failure modes and NEITHER is redundant:
//   - this one: the lock is missing or mis-keyed (the race test can, in principle,
//     miss this by luck)
//   - QuotaRaceLiveDB: the lock is present but in the wrong place (this test cannot
//     see that at all)
//
// Delete either and one real defect ships silently.
func TestProvisionHostedWorkerLockIsHeldLiveDB(t *testing.T) {
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
		_, err := h.provisionHostedWorker(ctx, userID, "blocked", "base", "m", 2)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("provisionHostedWorker completed (err=%v) while the per-user advisory lock was "+
			"held by another transaction — it is not taking pg_advisory_xact_lock("+
			"HostedProvisionLockClass, objid(user)) inside its tx, so the quota count is "+
			"unserialized and the guard is decorative", err)
	case <-time.After(500 * time.Millisecond):
		// Correct: it is waiting on the lock.
	}

	// Release, and it must proceed.
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder tx: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("provision after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provisionHostedWorker never completed after the lock was released")
	}
	if n := countHostedWorkers(ctx, t, pool, userID); n != 1 {
		t.Errorf("hosted workers = %d, want 1", n)
	}
}

// THE headline test of this milestone. Decision 8 requires quota enforcement to be
// atomic with no TOCTOU; this is what tells us whether it is.
//
// The count is a snapshot read and is NOT sufficient by itself: under READ COMMITTED
// two concurrent provisions both read N-1, neither sees the other's uncommitted row,
// and both insert. (A guarded `INSERT … WHERE count < quota` has exactly the same
// hole — it was tried and dropped for reading as though it did not.) The advisory
// lock is what serializes them. Measured, twice: with the lock removed, all 8 racers
// below succeed against a quota of 2; with the lock merely moved BELOW the count,
// the same thing happens, which is the defect only this test can see.
//
// Alone it could in principle pass by luck, which is why
// TestProvisionHostedWorkerLockIsHeldLiveDB above exists — see its comment for how
// the two split the failure modes.
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
			_, err := h.provisionHostedWorker(ctx, userID, fmt.Sprintf("racer-%d", i), "base", "m", quota)
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
		if _, err := h.provisionHostedWorker(ctx, userA, fmt.Sprintf("a-%d", i), "base", "m", 2); err != nil {
			t.Fatalf("provision for user A: %v", err)
		}
	}
	if _, err := h.provisionHostedWorker(ctx, userA, "a-over", "base", "m", 2); !errors.Is(err, errHostedQuotaExceeded) {
		t.Fatalf("user A over quota: err = %v, want errHostedQuotaExceeded", err)
	}
	// B is unaffected: the quota is per user.
	if _, err := h.provisionHostedWorker(ctx, userB, "b-0", "base", "m", 2); err != nil {
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

	wkr, err := h.provisionHostedWorker(ctx, userID, "co-write", "base", "l", 2)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	resp, err := hostedsvc.New(q, box).Poll(ctx)
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
}

// Nothing is sealed when the guard refuses. A quota-refused provision must leave no
// trace at all — the rollback covers both writes, which is a property of them being
// one transaction.
func TestProvisionHostedWorkerRefusalSealsNothingLiveDB(t *testing.T) {
	h, pool, _, _, userID := hostedLiveDB(t, "0")
	ctx := context.Background()

	if _, err := h.provisionHostedWorker(ctx, userID, "refused", "base", "m", 0); !errors.Is(err, errHostedQuotaExceeded) {
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

	wkr, err := h.provisionHostedWorker(ctx, userID, "doomed", "base", "m", 2)
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
	resp, err := hostedsvc.New(q, box).Poll(ctx)
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
	if _, err := h.provisionHostedWorker(ctx, userID, "replacement", "base", "m", 2); err != nil {
		t.Fatalf("reprovision after delete: %v", err)
	}
}

// Decision 11's refusal covers hosted workers too, and a refused delete must not
// destroy the pending token. The rule is kind-blind, so this asserts nothing new
// about the code — it pins the decision against a future hosted-specific delete
// path that forgets the refusal, which is a named M2 acceptance criterion.
func TestDeleteHostedWorkerRefusedWhileBusyLiveDB(t *testing.T) {
	h, pool, q, box, userID := hostedLiveDB(t, "2")
	ctx := context.Background()

	wkr, err := h.provisionHostedWorker(ctx, userID, "busy", "base", "m", 2)
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
