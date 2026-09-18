package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCustodyEpisodeFindQueriesLiveDB is the M6 live-DB proof for the two ADDITIVE find-owners
// reads the owner custody-episode reconciler (slacksvc.CustodyEpisodeReconciler) drives:
// ListOwnersOverCustodyLimit and ListOwnersWithClearedCustodyEpisode. It statically references
// both new methods so `deadcode -test ./...` sees them reachable, AND asserts their real behaviour
// against a real Postgres — the HAVING count admission predicate and the correlated open-hold
// count a fake store cannot exhibit — driving the whole episode cycle at the SQL level
// (crossing → claim → re-arm on drop → re-cross) alongside the frozen M1 Claim/Clear pair.
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and CI's test-api-store-it job
// run `-run 'LiveDB$'` over the five listed packages (store, handler, forgesvc, schedsvc,
// workersvc) only. slacksvc is not listed, so the reconciler's own lifecycle is proven by its
// unit tests (internal/slacksvc/custody_episode_test.go) and the queries are proven here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCustodyEpisodeFindQueriesLiveDB(t *testing.T) {
	ctx, pool, q := openCustodyEpisodeLiveDB(t)
	s := &custodyEpisodeSeeder{ctx: ctx, t: t, pool: pool}

	const limit = int32(8)

	userA := s.user() // 8 open holds → at the limit (over)
	holdsA := s.openHolds(userA, 8)
	userB := s.user() // 10 open holds → over
	s.openHolds(userB, 10)
	userC := s.user() // 3 open holds → under
	s.openHolds(userC, 3)
	s.user() // userD: 0 holds → never appears anywhere

	// ── ListOwnersOverCustodyLimit ─────────────────────────────────────────────
	over := s.listOver(limit)
	if !sameUUIDSet(over, []uuid.UUID{userA, userB}) {
		t.Fatalf("over limit(8) = %v, want {A,B}", over)
	}
	// A stricter limit of 9 drops A (exactly 8) and keeps B (10).
	if over9 := s.listOver(9); !sameUUIDSet(over9, []uuid.UUID{userB}) {
		t.Fatalf("over limit(9) = %v, want {B}", over9)
	}

	// Claim the episode notice for A (crossing) and C (a stale below-limit notice, e.g. left from
	// an earlier episode that already dropped). B is left unclaimed.
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, userA); err != nil || got != userA {
		t.Fatalf("claim A = (%v, %v), want (%v, nil)", got, err, userA)
	}
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, userC); err != nil || got != userC {
		t.Fatalf("claim C = (%v, %v), want (%v, nil)", got, err, userC)
	}

	// ── ListOwnersWithClearedCustodyEpisode ────────────────────────────────────
	// C has a notice and is BELOW the limit → its episode has closed (re-arm). A has a notice but
	// is still OVER → NOT cleared. B has no notice → never appears. userD has no notice.
	cleared := s.listCleared(limit)
	if !sameUUIDSet(cleared, []uuid.UUID{userC}) {
		t.Fatalf("cleared(8) = %v, want {C} (below-limit notice re-arms; A over-limit stays)", cleared)
	}

	// Release 6 of A's 8 holds so A drops to 2 open (below the limit). A's episode has now closed.
	s.release(holdsA[:6])
	if over := s.listOver(limit); !sameUUIDSet(over, []uuid.UUID{userB}) {
		t.Fatalf("after A drops: over(8) = %v, want {B}", over)
	}
	cleared = s.listCleared(limit)
	if !sameUUIDSet(cleared, []uuid.UUID{userA, userC}) {
		t.Fatalf("after A drops: cleared(8) = %v, want {A,C} (A dropped below the limit)", cleared)
	}

	// Re-arm A (the reconciler clears its notice), then re-cross A back over the limit. With the
	// notice cleared, a fresh claim WINS again — the later crossing notifies afresh.
	if err := q.ClearCustodyEpisodeNotice(ctx, userA); err != nil {
		t.Fatalf("clear A: %v", err)
	}
	s.openHolds(userA, 6) // A back to 8 open (2 remaining + 6 new)
	if over := s.listOver(limit); !sameUUIDSet(over, []uuid.UUID{userA, userB}) {
		t.Fatalf("after A re-crosses: over(8) = %v, want {A,B}", over)
	}
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, userA); err != nil || got != userA {
		t.Fatalf("re-claim A after re-arm = (%v, %v), want (%v, nil) (a re-armed episode notifies afresh)", got, err, userA)
	}
	// A second claim in the SAME (re-crossed) episode conflicts on the PK → pgx.ErrNoRows.
	if _, err := q.ClaimCustodyEpisodeNotice(ctx, userA); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim A = %v, want pgx.ErrNoRows (at-most-once per episode)", err)
	}
}

// TestCustodyEpisodeClaimConcurrentLiveDB proves the atomic RETURNING claim under concurrency: N
// goroutines racing ClaimCustodyEpisodeNotice for one owner with no prior notice yield EXACTLY
// ONE winner and N-1 pgx.ErrNoRows — the exactly-once dedup that makes N booting api pods send one
// custody-episode DM per owner. Run under -race.
func TestCustodyEpisodeClaimConcurrentLiveDB(t *testing.T) {
	ctx, pool, q := openCustodyEpisodeLiveDB(t)
	s := &custodyEpisodeSeeder{ctx: ctx, t: t, pool: pool}
	// A user with no custody_episode_notices row: exactly one claim can win.
	userID := s.user()

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	type res struct {
		id  uuid.UUID
		err error
	}
	results := make([]res, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, err := q.ClaimCustodyEpisodeNotice(ctx, userID)
			results[i] = res{id: id, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var winners, noRows, others int
	for _, r := range results {
		switch {
		case r.err == nil && r.id == userID:
			winners++
		case errors.Is(r.err, pgx.ErrNoRows):
			noRows++
		default:
			others++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (atomic RETURNING claim across concurrent replicas)", winners)
	}
	if noRows != n-1 {
		t.Fatalf("pgx.ErrNoRows results = %d, want %d (every loser sees no row)", noRows, n-1)
	}
	if others != 0 {
		t.Fatalf("got %d results that were neither the winner nor pgx.ErrNoRows", others)
	}
}

// ── live-DB harness + seeding helpers ──────────────────────────────────────────

func openCustodyEpisodeLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, store.New(pool)
}

// custodyEpisodeSeeder builds users and open custody holds directly. run_id / original_worker_id
// are PLAIN uuid columns on recovery_custody_holds (not FKs, D3), so a hold can be seeded without
// a runs/workers row — the find queries filter only on user_id and state.
type custodyEpisodeSeeder struct {
	ctx  context.Context
	t    *testing.T
	pool *pgxpool.Pool
	n    int
}

func (s *custodyEpisodeSeeder) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatalf("seed exec failed: %v\nSQL: %s", err, sql)
	}
}

func (s *custodyEpisodeSeeder) user() uuid.UUID {
	s.t.Helper()
	id := uuid.New()
	s.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		id, fmt.Sprintf("custody-%s@e2e", id))
	return id
}

// openHolds inserts n state='open' custody holds for userID and returns their ids.
func (s *custodyEpisodeSeeder) openHolds(userID uuid.UUID, n int) []uuid.UUID {
	s.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		s.n++
		id := uuid.New()
		s.exec(`INSERT INTO recovery_custody_holds
		          (id, user_id, run_id, generation, state, original_worker_id, original_worker_identity)
		        VALUES ($1, $2, $3, $4, 'open', $5, $6)`,
			id, userID, uuid.New(), int64(s.n), uuid.New(), fmt.Sprintf("w-%d", s.n))
		ids = append(ids, id)
	}
	return ids
}

// release flips the named holds to state='released'.
func (s *custodyEpisodeSeeder) release(ids []uuid.UUID) {
	s.t.Helper()
	s.exec(`UPDATE recovery_custody_holds SET state = 'released' WHERE id = ANY($1)`, ids)
}

func (s *custodyEpisodeSeeder) listOver(limit int32) []uuid.UUID {
	s.t.Helper()
	got, err := store.New(s.pool).ListOwnersOverCustodyLimit(s.ctx, limit)
	if err != nil {
		s.t.Fatalf("ListOwnersOverCustodyLimit(%d): %v", limit, err)
	}
	return got
}

func (s *custodyEpisodeSeeder) listCleared(limit int32) []uuid.UUID {
	s.t.Helper()
	got, err := store.New(s.pool).ListOwnersWithClearedCustodyEpisode(s.ctx, limit)
	if err != nil {
		s.t.Fatalf("ListOwnersWithClearedCustodyEpisode(%d): %v", limit, err)
	}
	return got
}

// sameUUIDSet reports whether got and want contain the same uuids (order-independent, no dupes).
func sameUUIDSet(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[uuid.UUID]int, len(want))
	for _, w := range want {
		m[w]++
	}
	for _, g := range got {
		m[g]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
