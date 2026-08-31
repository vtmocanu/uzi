package schedsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Behavioral, end-to-end live-DB coverage for PRD #890 M4. Distinct from M1's
// TestVaultLockNoticeQueriesLiveDB, which proves the three queries EXECUTE and does a
// SERIAL claim → clear → reclaim: this drives the REAL VaultLockReconciler over the
// REAL notifysvc.Service and the REAL *store.Queries against live Postgres, covering
// what M1 did not —
//
//   - Real scoping (Success Criterion 2): a spread of users seeded so the eligibility
//     query itself decides who is notified — queued runs and active+due schedules count;
//     running/claimed runs and fired/disabled schedules do not; no-Slack and no-vault
//     users never qualify.
//   - The unlock → clear → re-notify episode cycle (SC5) end-to-end through Reconcile.
//   - The vlt.Unlocked fail-safe skip (SC3), proven to suppress a send WITHOUT burning
//     the mark (a later Reconcile, once relocked, still notifies).
//   - The atomic claim under CONCURRENCY (SC6) — the multi-replica dedup — in a separate
//     test below (M1 was serial).
//
// Assertions read the DURABLE, synchronous proof: notifysvc.Notify persists the inbox
// row inside the call (before the best-effort Slack enqueue), so a raw
// `SELECT count(*) FROM notifications WHERE user_id=$1 AND kind='vault_locked'` is the
// race-free source of truth. The captured Slack render is asserted too, guarded by a
// mutex per the Slacker contract.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.
func TestVaultLockReconcilerLiveDB(t *testing.T) {
	ctx, pool, q := openVaultLockLiveDB(t)
	s := &vaultLockSeeder{ctx: ctx, t: t, pool: pool}

	// A — eligible: vault + confirmed Slack + a queued run (lock-blockable work).
	userA := s.user(true)
	s.vault(userA)
	s.run(userA, s.repo(userA), "queued")

	// B — eligible: vault + Slack + an enabled, active, due schedule (no pending run).
	userB := s.user(true)
	s.vault(userB)
	s.schedule(userB, s.repo(userB), true, "active", true)

	// C — NOT eligible: vault + Slack, but the only run is already 'running' (the lock
	// does not block an already-claimed run).
	userC := s.user(true)
	s.vault(userC)
	s.run(userC, s.repo(userC), "running")

	// D — NOT eligible: vault + Slack, but the only run is 'claimed'.
	userD := s.user(true)
	s.vault(userD)
	s.run(userD, s.repo(userD), "claimed")

	// E — NOT eligible: vault + Slack, but the only schedule is 'fired' (will not fire).
	userE := s.user(true)
	s.vault(userE)
	s.schedule(userE, s.repo(userE), true, "fired", true)

	// F — NOT eligible: vault + Slack, but the only schedule is disabled.
	userF := s.user(true)
	s.vault(userF)
	s.schedule(userF, s.repo(userF), false, "active", true)

	// G — NOT eligible: vault + a queued run, but no confirmed Slack link.
	userG := s.user(false)
	s.vault(userG)
	s.run(userG, s.repo(userG), "queued")

	// H — NOT eligible: confirmed Slack + a queued run, but NO user_vaults row.
	userH := s.user(true)
	s.run(userH, s.repo(userH), "queued")

	// U — eligible work, but UNLOCKED on this pod: vault + Slack + queued run, with the
	// local vlt.Unlocked skip active. Must be suppressed WITHOUT burning the mark.
	userU := s.user(true)
	s.vault(userU)
	s.run(userU, s.repo(userU), "queued")

	slacker := &capturingSlacker{}
	notifier := notifysvc.New(q, slacker, 0, nil) // real Service over the live DB
	vlt := &fakeVault{unlockedSet: map[uuid.UUID]bool{userU: true}}
	set := &fakeSettings{publicBaseURL: "https://uzi.example"}
	rec := NewVaultLockReconciler(q, vlt, notifier, set, nil)

	// ── Reconcile #1: exactly {A, B} are notified. ─────────────────────────────
	rec.Reconcile(ctx)

	for _, u := range []struct {
		name string
		id   uuid.UUID
	}{{"A", userA}, {"B", userB}} {
		if got := countVaultLocked(ctx, t, pool, u.id); got != 1 {
			t.Fatalf("after Reconcile #1: user %s has %d vault_locked rows, want 1", u.name, got)
		}
	}
	for _, u := range []struct {
		name string
		id   uuid.UUID
	}{{"C", userC}, {"D", userD}, {"E", userE}, {"F", userF}, {"G", userG}, {"H", userH}, {"U", userU}} {
		if got := countVaultLocked(ctx, t, pool, u.id); got != 0 {
			t.Fatalf("after Reconcile #1: ineligible user %s has %d vault_locked rows, want 0", u.name, got)
		}
	}

	// The captured Slack render for A: fixed cause-neutral copy, 🔒 emoji, the deep link,
	// and a "queued run" Fact.
	assertVaultLockRender(t, slacker, userA, "queued run")
	// B's render carries a "scheduled job" Fact instead.
	assertVaultLockRender(t, slacker, userB, "scheduled job")

	// ── Reconcile #2: episode dedup — A and B are NOT re-notified. ─────────────
	rec.Reconcile(ctx)
	if got := countVaultLocked(ctx, t, pool, userA); got != 1 {
		t.Fatalf("after Reconcile #2: user A has %d rows, want 1 (the mark must exclude it)", got)
	}
	if got := countVaultLocked(ctx, t, pool, userB); got != 1 {
		t.Fatalf("after Reconcile #2: user B has %d rows, want 1 (the mark must exclude it)", got)
	}

	// ── Re-notify cycle (SC5): clear A's mark (the unlock re-arm), reconcile → A is
	// notified afresh; B stays at 1. ───────────────────────────────────────────
	if err := q.ClearVaultLockNotice(ctx, userA); err != nil {
		t.Fatalf("ClearVaultLockNotice(A): %v", err)
	}
	rec.Reconcile(ctx)
	if got := countVaultLocked(ctx, t, pool, userA); got != 2 {
		t.Fatalf("after clear + Reconcile #3: user A has %d rows, want 2 (re-notified)", got)
	}
	if got := countVaultLocked(ctx, t, pool, userB); got != 1 {
		t.Fatalf("after clear + Reconcile #3: user B has %d rows, want 1 (not re-armed)", got)
	}

	// ── Unlocked-skip proof (SC3): U was suppressed by the local vlt.Unlocked skip on
	// every prior tick WITHOUT burning its mark, so relocking it and reconciling now
	// sends. ───────────────────────────────────────────────────────────────────
	if got := countVaultLocked(ctx, t, pool, userU); got != 0 {
		t.Fatalf("before relock: user U has %d rows, want 0 (still unlocked-skipped)", got)
	}
	vlt.unlockedSet[userU] = false
	rec.Reconcile(ctx)
	if got := countVaultLocked(ctx, t, pool, userU); got != 1 {
		t.Fatalf("after relock + Reconcile #4: user U has %d rows, want 1 (the skip preserved the mark)", got)
	}
	// A/B unchanged by the U-relock tick.
	if got := countVaultLocked(ctx, t, pool, userA); got != 2 {
		t.Fatalf("after Reconcile #4: user A has %d rows, want 2 (unchanged)", got)
	}
	if got := countVaultLocked(ctx, t, pool, userB); got != 1 {
		t.Fatalf("after Reconcile #4: user B has %d rows, want 1 (unchanged)", got)
	}
	// The ineligible cohort stayed silent throughout.
	for _, u := range []struct {
		name string
		id   uuid.UUID
	}{{"C", userC}, {"D", userD}, {"E", userE}, {"F", userF}, {"G", userG}, {"H", userH}} {
		if got := countVaultLocked(ctx, t, pool, u.id); got != 0 {
			t.Fatalf("final: ineligible user %s has %d vault_locked rows, want 0", u.name, got)
		}
	}
}

// TestVaultLockClaimConcurrentLiveDB proves the atomic RETURNING claim (SC6): N
// goroutines racing ClaimVaultLockNotice for one marked-NULL user yield EXACTLY ONE
// winner and N-1 pgx.ErrNoRows — the exactly-once dedup that makes N booting api
// replicas send one DM per user. Run under -race.
func TestVaultLockClaimConcurrentLiveDB(t *testing.T) {
	ctx, pool, q := openVaultLockLiveDB(t)
	s := &vaultLockSeeder{ctx: ctx, t: t, pool: pool}
	userID := s.user(true)
	s.vault(userID) // lock_notified_at defaults NULL → exactly one claim can win

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{}) // barrier: release all goroutines at once
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
			id, err := q.ClaimVaultLockNotice(ctx, userID)
			results[i] = res{id: id, err: err} // each goroutine writes its own slot
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

// openVaultLockLiveDB mirrors the skip/migrate/pool idiom of the M1 store live-DB test.
func openVaultLockLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
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

// capturingSlacker records every PublishNotification under a mutex. notifysvc's real
// slacksvc Slacker publishes on its own goroutine, so the mutex guards against the race
// even though this fake is invoked synchronously inside Notify; the durable assertions
// query the persisted inbox rows.
type capturingSlacker struct {
	mu     sync.Mutex
	byUser map[uuid.UUID][]notifysvc.SlackRender
}

func (s *capturingSlacker) PublishNotification(userID uuid.UUID, r notifysvc.SlackRender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byUser == nil {
		s.byUser = map[uuid.UUID][]notifysvc.SlackRender{}
	}
	s.byUser[userID] = append(s.byUser[userID], r)
}

func (s *capturingSlacker) renders(id uuid.UUID) []notifysvc.SlackRender {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notifysvc.SlackRender(nil), s.byUser[id]...)
}

// assertVaultLockRender checks a user's most recent captured Slack render carries the
// fixed cause-neutral copy, the 🔒 emoji, the deep link, and a Fact containing wantFact.
func assertVaultLockRender(t *testing.T, slacker *capturingSlacker, id uuid.UUID, wantFact string) {
	t.Helper()
	rs := slacker.renders(id)
	if len(rs) == 0 {
		t.Fatalf("no Slack render captured for user %s", id)
	}
	r := rs[len(rs)-1]
	if r.Title != vaultLockTitle {
		t.Errorf("render Title = %q, want %q", r.Title, vaultLockTitle)
	}
	if r.Body != vaultLockBody {
		t.Errorf("render Body = %q, want %q", r.Body, vaultLockBody)
	}
	if r.Emoji != "🔒" {
		t.Errorf("render Emoji = %q, want the lock emoji", r.Emoji)
	}
	if r.Link != "https://uzi.example" {
		t.Errorf("render Link = %q, want the deep link", r.Link)
	}
	var found bool
	for _, f := range r.Facts {
		if strings.Contains(f, wantFact) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("render Facts = %v, want one containing %q", r.Facts, wantFact)
	}
}

func countVaultLocked(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = 'vault_locked'`,
		userID).Scan(&n); err != nil {
		t.Fatalf("count vault_locked notifications for %s: %v", userID, err)
	}
	return n
}

// vaultLockSeeder builds the eligibility fixtures. n is a monotonic counter that keeps
// every forge connection / repo distinct — repos has UNIQUE(connection_id,
// forge_project_id), so each user gets its own connection AND a globally distinct
// forge_project_id.
type vaultLockSeeder struct {
	ctx  context.Context
	t    *testing.T
	pool *pgxpool.Pool
	n    int
}

func (s *vaultLockSeeder) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatalf("seed exec failed: %v\nSQL: %s", err, sql)
	}
}

// user inserts a user. When slackOK, it is a confirmed, opted-in Slack delivery target
// (slack_notify=true, slack_resolved_id set, slack_link_confirmed_at=now()); otherwise
// slack_notify=false, so it fails the Slack-deliverable gate.
func (s *vaultLockSeeder) user(slackOK bool) uuid.UUID {
	s.t.Helper()
	id := uuid.New()
	if slackOK {
		s.exec(`INSERT INTO users (id, email, password_hash, slack_notify, slack_resolved_id, slack_link_confirmed_at)
		         VALUES ($1, $2, 'x', true, $3, now())`,
			id, fmt.Sprintf("vlt-%s@e2e", id), "U"+id.String()[:8])
	} else {
		s.exec(`INSERT INTO users (id, email, password_hash, slack_notify)
		         VALUES ($1, $2, 'x', false)`,
			id, fmt.Sprintf("vlt-%s@e2e", id))
	}
	return id
}

func (s *vaultLockSeeder) vault(userID uuid.UUID) {
	s.exec(`INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek) VALUES ($1, $2, $3)`,
		userID, []byte("saltsaltsaltsalt"), []byte("wrapped-dek-bytes"))
}

// repo creates a forge connection + repo owned by userID and returns the repo id, with a
// globally distinct forge_project_id.
func (s *vaultLockSeeder) repo(userID uuid.UUID) uuid.UUID {
	s.t.Helper()
	s.n++
	connID, repoID := uuid.New(), uuid.New()
	fpid := int64(9000 + s.n)
	s.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	         VALUES ($1, $2, 'gitlab', $3, $4, $5, $6)`,
		connID, userID, fmt.Sprintf("https://forge-%d.e2e", s.n), fmt.Sprintf("bot-%d", s.n), fpid, []byte{0x1})
	s.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
	         VALUES ($1, $2, $3, $4, $5, true)`,
		repoID, connID, fpid, fmt.Sprintf("g/vlt-%d", s.n), fmt.Sprintf("https://forge.e2e/g/vlt-%d", s.n))
	return repoID
}

func (s *vaultLockSeeder) run(userID, repoID uuid.UUID, status string) {
	s.t.Helper()
	s.n++
	s.exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
	         VALUES ($1, $2, $3, $4, 't', 'd', $5)`,
		uuid.New(), userID, repoID, int64(4000+s.n), status)
}

// schedule inserts a target='prompt', timing='once' run_schedule. due sets next_fire_at
// to now() (else NULL). enabled + status control the fire-eligibility predicate.
func (s *vaultLockSeeder) schedule(userID, repoID uuid.UUID, enabled bool, status string, due bool) {
	s.t.Helper()
	s.n++
	var nextFire any
	if due {
		nextFire = time.Now()
	}
	s.exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, run_at, next_fire_at, enabled, status)
	         VALUES ($1, $2, $3, 'prompt', 'p', 'once', now(), $4, $5, $6)`,
		uuid.New(), userID, repoID, nextFire, enabled, status)
}
