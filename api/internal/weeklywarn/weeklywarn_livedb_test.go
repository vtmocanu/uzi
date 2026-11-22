package weeklywarn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests need UZI_TEST_DATABASE_URL (e2e/run-store-it.sh provides a throwaway Postgres and
// sweeps this package for the LiveDB suffix). They prove the transaction ordering, the rollback and
// retry behaviour, the fences and the cascade; durability beyond the deployment's Postgres settings
// is not claimed.

var migrateOnce sync.Once

type published struct {
	row       store.Notification
	n         notifysvc.Notification
	committed bool // the claim (warned flag and notifications row) was visible to another connection
}

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	q    *store.Queries
	user uuid.UUID
	w    *Warner
	now  time.Time

	mu   sync.Mutex
	pubs []published
}

// pubSink is the Publisher: it records each publish and, from a second connection, whether the
// claim it announces was already committed.
type pubSink struct{ f *fixture }

func (p pubSink) PublishPersisted(ctx context.Context, row store.Notification, n notifysvc.Notification) {
	var committed bool
	err := p.f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notifications WHERE id = $1) AND EXISTS (SELECT 1 FROM weekly_limit_alerts WHERE user_id = $2 AND warned)`,
		row.ID, row.UserID).Scan(&committed)
	if err != nil {
		committed = false
	}
	p.f.mu.Lock()
	p.f.pubs = append(p.f.pubs, published{row: row, n: n, committed: committed})
	p.f.mu.Unlock()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	var merr error
	migrateOnce.Do(func() { merr = store.Migrate(ctx, dsn) })
	if merr != nil {
		t.Fatalf("migrate: %v", merr)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), user: uuid.New(), now: time.Now().UTC().Truncate(time.Second)}
	f.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user, fmt.Sprintf("weeklywarn-%s@e2e", f.user))
	f.w = New(Config{
		Pool: pool, Queries: f.q, Publisher: pubSink{f}, Blocks: fakeBlocks{},
		Runs:           func(context.Context, uuid.UUID) ([]store.Run, error) { return nil, nil },
		ClaudeInterval: time.Hour, CodexInterval: time.Hour, Now: func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) secret(kind, label string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with) VALUES ($1, $2, $3, $4, false, $5, 'master')`,
		id, f.user, kind, label+"-"+id.String()[:8], []byte("sealed"))
	return id
}

func (f *fixture) rev(secret uuid.UUID) int64 {
	f.t.Helper()
	row, err := f.q.GetSecretEnablement(f.ctx, store.GetSecretEnablementParams{ID: secret, UserID: f.user})
	if err != nil {
		f.t.Fatalf("GetSecretEnablement: %v", err)
	}
	return row.EnablementRev
}

func (f *fixture) claudeRun(secret uuid.UUID, status string) {
	f.t.Helper()
	f.exec(`INSERT INTO runs (user_id, kind, issue_title, issue_description, harness, status, anthropic_secret_id) VALUES ($1, 'chat', 't', 'd', 'claude', $2, $3)`,
		f.user, status, secret)
}

// at returns the instant k seconds past the fixture clock: observation instants are ordered by k.
func (f *fixture) at(k int) time.Time { return f.now.Add(time.Duration(k) * time.Second) }

// resetIn is a reset h hours past the fixture clock.
func (f *fixture) resetIn(h float64) time.Time {
	return f.now.Add(time.Duration(h * float64(time.Hour)))
}

func (f *fixture) claude(secret uuid.UUID, rev int64, pct int16, reset time.Time, k int) {
	f.t.Helper()
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, secret, rev, &pct, &reset, f.at(k))
}

func (f *fixture) claudeAt(secret uuid.UUID, rev int64, pct int16, reset, syncedAt time.Time) {
	f.t.Helper()
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, secret, rev, &pct, &reset, syncedAt)
}

func (f *fixture) credential(secret uuid.UUID, rev int64, pct int16, reset time.Time, k int) *credential {
	f.t.Helper()
	c := f.w.anthropicCredential(f.user, secret, rev, &pct, &reset, f.at(k))
	if c == nil {
		f.t.Fatal("reading unexpectedly rejected by the validity gate")
	}
	return c
}

type ledger struct {
	exists     bool
	observedAt time.Time
	warned     bool
	anchor     *time.Time
	pct        *float64
	bucket     *string
	multi      bool
}

func (f *fixture) ledgerWhere(col string, id uuid.UUID) ledger {
	f.t.Helper()
	var l ledger
	var anchor pgtype.Timestamptz
	var pct pgtype.Float8
	var bucket pgtype.Text
	err := f.pool.QueryRow(f.ctx, `SELECT observed_at, warned, anchor_reset, anchor_pct, anchor_bucket, anchor_multi FROM weekly_limit_alerts WHERE user_id = $1 AND `+col+` = $2`,
		f.user, id).Scan(&l.observedAt, &l.warned, &anchor, &pct, &bucket, &l.multi)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger{}
	}
	if err != nil {
		f.t.Fatalf("read ledger: %v", err)
	}
	l.exists = true
	if anchor.Valid {
		v := anchor.Time
		l.anchor = &v
	}
	if pct.Valid {
		v := pct.Float64
		l.pct = &v
	}
	if bucket.Valid {
		v := bucket.String
		l.bucket = &v
	}
	return l
}

func (f *fixture) ledger(secret uuid.UUID) ledger {
	return f.ledgerWhere("anthropic_secret_id", secret)
}

func (f *fixture) ledgerRows() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM weekly_limit_alerts WHERE user_id = $1`, f.user).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) notifications() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = $2`, f.user, notifysvc.KindWeeklyLimitWarning).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) published() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.pubs...)
}

// wantDMs asserts the number of committed notifications rows and publishes agree.
func (f *fixture) wantDMs(n int, why string) {
	f.t.Helper()
	if got := f.notifications(); got != n {
		f.t.Fatalf("%s: notifications rows = %d, want %d", why, got, n)
	}
	if got := len(f.published()); got != n {
		f.t.Fatalf("%s: publishes = %d, want %d", why, got, n)
	}
}

func (f *fixture) failNextPersist() {
	var once sync.Once
	f.w.persist = func(ctx context.Context, q notifysvc.NotificationInserter, n notifysvc.Notification) (store.Notification, error) {
		fail := false
		once.Do(func() { fail = true })
		if fail {
			return store.Notification{}, errors.New("injected notification insert failure")
		}
		return notifysvc.PersistTx(ctx, q, n)
	}
}

// anthropicFx is a fixture with one enabled Anthropic token and one running run on it.
func anthropicFx(t *testing.T) (*fixture, uuid.UUID, int64) {
	f := newFixture(t)
	s := f.secret(store.KindAnthropicToken, "tok")
	f.claudeRun(s, "running")
	return f, s, f.rev(s)
}

func TestWeeklyWarnFirstReadingSeedsAndWarnsOnceLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	f.claude(s, rev, 40, reset, 1)
	l := f.ledger(s)
	if !l.exists || l.warned || !l.observedAt.Equal(f.at(1)) || l.anchor == nil || !l.anchor.Equal(reset) {
		t.Fatalf("seed after the first reading = %+v", l)
	}
	f.wantDMs(0, "below the margin")
	f.claude(s, rev, 90, reset, 2)
	f.wantDMs(1, "first near reading")
	f.claude(s, rev, 95, reset, 3)
	f.claude(s, rev, 100, reset, 4)
	f.wantDMs(1, "same window")
	p := f.published()[0]
	if !p.committed {
		t.Fatal("published before the claim committed")
	}
	if p.n.Kind != notifysvc.KindWeeklyLimitWarning || p.n.UserID != f.user || p.n.Slack == nil || p.n.Slack.Credential == nil ||
		p.n.Slack.Credential.SecretID != s || p.n.Slack.Credential.EnablementRev != rev || p.n.Slack.DeliveryTag != "" {
		t.Fatalf("published notification = %+v", p.n)
	}
}

func TestWeeklyWarnTransactionOrderingAndRollbackLiveDB(t *testing.T) {
	t.Run("an injected notification-row failure rolls the claim back and the next near reading notifies", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		reset := f.resetIn(100)
		f.failNextPersist()
		f.claude(s, rev, 91, reset, 1)
		l := f.ledger(s)
		if !l.exists || l.warned || !l.observedAt.Equal(f.at(1)) {
			t.Fatalf("after the failed claim: %+v (the observation must be committed, warned false)", l)
		}
		f.wantDMs(0, "rolled back claim emits nothing")
		f.claude(s, rev, 92, reset, 2)
		f.wantDMs(1, "retry")
		if !f.ledger(s).warned {
			t.Fatal("retry did not claim")
		}
	})
	t.Run("a failed claim right after a refill, which then notifies", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		reset := f.resetIn(100)
		f.claude(s, rev, 90, reset, 1)
		f.wantDMs(1, "first warning")
		f.claude(s, rev, 0, reset, 2) // the refill rearms at the unchanged boundary
		if f.ledger(s).warned {
			t.Fatal("refill did not rearm")
		}
		f.failNextPersist()
		f.claude(s, rev, 90, reset, 3)
		f.wantDMs(1, "the failed claim after the refill emits nothing")
		if f.ledger(s).warned {
			t.Fatal("rollback left warned set")
		}
		f.claude(s, rev, 91, reset, 4)
		f.wantDMs(2, "the next near reading notifies")
	})
	t.Run("nothing is published before the commit and a rollback never publishes", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		f.failNextPersist()
		f.claude(s, rev, 95, f.resetIn(100), 1)
		if len(f.published()) != 0 {
			t.Fatal("published although the transaction rolled back")
		}
		f.claude(s, rev, 95, f.resetIn(100), 2)
		ps := f.published()
		if len(ps) != 1 || !ps[0].committed {
			t.Fatalf("publishes = %+v, want one, after the commit", ps)
		}
	})
	t.Run("a claim retried after an ambiguous commit writes no second row", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		c := f.credential(s, rev, 95, f.resetIn(100), 1)
		next, ok := f.w.observe(f.ctx, c)
		if !ok {
			t.Fatal("observation dropped")
		}
		f.w.claim(f.ctx, c, next)
		f.w.claim(f.ctx, c, next) // the caller could not tell whether the first commit landed
		f.wantDMs(1, "retried claim")
	})
}

func TestWeeklyWarnConcurrentObserversLiveDB(t *testing.T) {
	for i := 0; i < 5; i++ {
		f, s, rev := anthropicFx(t)
		reset := f.resetIn(100)
		var wg sync.WaitGroup
		for _, k := range []int{1, 2, 3} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.claude(s, rev, 93, reset, k)
			}()
		}
		wg.Wait()
		f.wantDMs(1, fmt.Sprintf("round %d: concurrent observers", i))
		if l := f.ledger(s); !l.warned {
			t.Fatalf("ledger = %+v", l)
		}
	}
}

func TestWeeklyWarnEqualAndOlderTimestampsLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	f.claude(s, rev, 40, reset, 5)
	// An equal instant is dropped: the second (near) reading must not claim.
	f.claude(s, rev, 99, reset, 5)
	if l := f.ledger(s); l.pct == nil || *l.pct != 40 || l.warned {
		t.Fatalf("an equal-instant reading was applied: %+v", l)
	}
	f.wantDMs(0, "equal timestamp")
	// An older instant (a reading B observed before A that arrives after A) is dropped too.
	f.claude(s, rev, 99, reset, 4)
	if l := f.ledger(s); *l.pct != 40 || !l.observedAt.Equal(f.at(5)) {
		t.Fatalf("an older reading was applied: %+v", l)
	}
	f.wantDMs(0, "older timestamp")
	f.claude(s, rev, 99, reset, 6)
	f.wantDMs(1, "a newer reading applies")
}

func TestWeeklyWarnClaimLosesToNewerObservationLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	c1 := f.credential(s, rev, 95, reset, 1)
	n1, ok := f.w.observe(f.ctx, c1)
	if !ok {
		t.Fatal("R1 dropped")
	}
	// A newer accepted observation lands before R1's claim.
	c2 := f.credential(s, rev, 96, reset, 2)
	n2, ok := f.w.observe(f.ctx, c2)
	if !ok {
		t.Fatal("R2 dropped")
	}
	f.w.claim(f.ctx, c1, n1)
	f.wantDMs(0, "the older claim lost")
	f.w.claim(f.ctx, c2, n2)
	f.wantDMs(1, "the newest claim wins")
}

func TestWeeklyWarnRejectedReadingDoesNotCancelAPendingClaimLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	c1 := f.credential(s, rev, 95, reset, 1)
	n1, ok := f.w.observe(f.ctx, c1)
	if !ok {
		t.Fatal("R1 dropped")
	}
	// R2 is stale (written more than three intervals before the warner's clock): rejected before any
	// transaction, so observed_at stays at R1's instant.
	pct := int16(99)
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, s, rev, &pct, &reset, f.now.Add(-4*time.Hour))
	if l := f.ledger(s); !l.observedAt.Equal(f.at(1)) || l.warned {
		t.Fatalf("a rejected reading touched the ledger: %+v", l)
	}
	f.w.claim(f.ctx, c1, n1)
	f.wantDMs(1, "R1's claim still commits exactly one DM")
}

func TestWeeklyWarnFreshCredentialRejectedReadingCreatesNoRowLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	pct := int16(97)
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, s, rev, &pct, &reset, f.now.Add(-4*time.Hour)) // stale
	pre := f.resetIn(-1)
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, s, rev, &pct, &pre, f.now.Add(-2*time.Hour)) // pre-reset
	f.w.ObserveAnthropicWeekly(f.ctx, f.user, s, rev, nil, &reset, f.at(1))                // absent SevenDayPct
	if f.ledgerRows() != 0 {
		t.Fatal("a rejected reading created a ledger row")
	}
	// A valid reading under a stale fence (the token was disabled) creates none either.
	f.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, s)
	f.claude(s, rev, 97, reset, 2)
	if f.ledgerRows() != 0 {
		t.Fatal("a stale-fence reading created a ledger row")
	}
	f.wantDMs(0, "rejected readings")
}

func TestWeeklyWarnStaleFenceRotationAndEnablementLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	f.claude(s, rev, 40, reset, 1)
	// Rotation / re-enable moves the enablement revision: a reading observed at the old one is dropped.
	f.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, s)
	f.claude(s, rev, 99, reset, 2)
	if l := f.ledger(s); *l.pct != 40 || !l.observedAt.Equal(f.at(1)) {
		t.Fatalf("a stale-revision reading was applied: %+v", l)
	}
	f.wantDMs(0, "stale revision")
	f.claude(s, rev+1, 99, reset, 3)
	f.wantDMs(1, "the current revision warns")
}

func TestWeeklyWarnFenceHeldAgainstSecretEnablementLiveDB(t *testing.T) {
	t.Run("an observer waits behind a pending enablement change and then sees it", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if err := store.LockSecretMutation(f.ctx, tx, f.user); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.claude(s, rev, 99, f.resetIn(100), 1)
		}()
		select {
		case <-done:
			t.Fatal("the observation did not wait for the exclusive secret-mutation lock")
		case <-time.After(300 * time.Millisecond):
		}
		if _, err := f.q.WithTx(tx).SetSecretEnablement(f.ctx, store.SetSecretEnablementParams{ID: s, UserID: f.user, Enabled: false}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the observation never resumed")
		}
		if f.ledgerRows() != 0 {
			t.Fatal("the observation committed against a credential that was disabled first")
		}
		f.wantDMs(0, "disabled credential")
	})
	t.Run("an enablement change waits for an in-flight claim", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		release := make(chan struct{})
		changed := make(chan struct{})
		var once sync.Once
		f.w.persist = func(ctx context.Context, q notifysvc.NotificationInserter, n notifysvc.Notification) (store.Notification, error) {
			once.Do(func() {
				// Mid-claim, the shared lock is held: a concurrent enablement change must queue behind it.
				go func() {
					defer close(changed)
					tx, err := f.pool.Begin(f.ctx)
					if err != nil {
						return
					}
					defer func() { _ = tx.Rollback(f.ctx) }()
					if store.LockSecretMutation(f.ctx, tx, f.user) != nil {
						return
					}
					_, _ = f.q.WithTx(tx).SetSecretEnablement(f.ctx, store.SetSecretEnablementParams{ID: s, UserID: f.user, Enabled: false})
					_ = tx.Commit(f.ctx)
				}()
				select {
				case <-changed:
					close(release)
				case <-time.After(300 * time.Millisecond):
				}
			})
			return notifysvc.PersistTx(ctx, q, n)
		}
		f.claude(s, rev, 99, f.resetIn(100), 1)
		select {
		case <-release:
			t.Fatal("the enablement change completed while the claim held the secret-mutation lock")
		default:
		}
		<-changed
		f.wantDMs(1, "the claim committed before the enablement change")
	})
}

func TestWeeklyWarnCascadeOnSecretDeleteLiveDB(t *testing.T) {
	f, s, rev := anthropicFx(t)
	reset := f.resetIn(100)
	f.claude(s, rev, 92, reset, 1)
	f.wantDMs(1, "first token")
	if !f.ledger(s).exists {
		t.Fatal("no ledger row")
	}
	// A claim whose row was cascaded away skips cleanly.
	c := f.credential(s, rev, 93, reset, 2)
	n, ok := f.w.observe(f.ctx, c)
	if !ok {
		t.Fatal("observation dropped")
	}
	f.exec(`DELETE FROM runs WHERE user_id = $1`, f.user)
	f.exec(`DELETE FROM user_secrets WHERE id = $1`, s)
	if f.ledgerRows() != 0 {
		t.Fatal("deleting the token did not cascade the ledger row")
	}
	f.w.claim(f.ctx, c, n)
	f.wantDMs(1, "a claim against a deleted row skips")

	// Deletion creates a NEW ledger: a replacement token can warn again within the same week.
	s2 := f.secret(store.KindAnthropicToken, "replacement")
	f.claudeRun(s2, "running")
	f.claude(s2, f.rev(s2), 92, reset, 3)
	f.wantDMs(2, "a new ledger warns again in the same week")
	if f.ledger(s2).anchor == nil || !f.ledger(s2).anchor.Equal(reset) {
		t.Fatalf("new ledger = %+v", f.ledger(s2))
	}
}

func TestWeeklyWarnGatesLiveDB(t *testing.T) {
	t.Run("margin 0 suppresses even an at-limit reading but keeps observing", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		f.exec(`UPDATE users SET weekly_warning_margin_pts = 0 WHERE id = $1`, f.user)
		f.claude(s, rev, 100, f.resetIn(100), 1)
		f.wantDMs(0, "margin 0")
		if l := f.ledger(s); !l.exists || l.warned || !l.observedAt.Equal(f.at(1)) {
			t.Fatalf("margin 0 must still record the observation: %+v", l)
		}
	})
	t.Run("no non-terminal run sends nothing and leaves the period unwarned", func(t *testing.T) {
		f := newFixture(t)
		s := f.secret(store.KindAnthropicToken, "idle")
		f.claudeRun(s, "completed")
		rev := f.rev(s)
		f.claude(s, rev, 97, f.resetIn(100), 1)
		f.wantDMs(0, "no run")
		if f.ledger(s).warned {
			t.Fatal("warned without a run")
		}
		f.claudeRun(s, "queued")
		f.claude(s, rev, 97, f.resetIn(100), 2)
		f.wantDMs(1, "a run appeared")
	})
	t.Run("the reserve threshold is the effective limit", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		f.exec(`UPDATE users SET weekly_reserve_pct = 70 WHERE id = $1`, f.user)
		f.claude(s, rev, 50, f.resetIn(100), 1) // below 70 - 15
		f.wantDMs(0, "below limit - margin")
		f.claude(s, rev, 56, f.resetIn(100), 2)
		f.wantDMs(1, "within the margin of the reserve")
	})
	t.Run("a lagging post-boundary reading does not rearm; the later real reset rearms once", func(t *testing.T) {
		f, s, rev := anthropicFx(t)
		base := f.now
		reset := base.Add(time.Hour)
		f.claudeAt(s, rev, 92, reset, base)
		f.wantDMs(1, "warn")
		f.now = base.Add(2 * time.Hour) // the boundary has passed
		f.claudeAt(s, rev, 92, reset, f.now.Add(-time.Minute))
		f.wantDMs(1, "lagging reading at 92 percent")
		f.claudeAt(s, rev, 93, f.now.Add(168*time.Hour), f.now)
		f.wantDMs(2, "the real new reset")
		f.claudeAt(s, rev, 94, f.now.Add(168*time.Hour), f.now.Add(time.Second))
		f.wantDMs(2, "once per period")
	})
}

// ---- Codex ----

type codexFx struct {
	*fixture
	account uuid.UUID
	alias   uuid.UUID
	gen     int64
	crev    int64
	sig     string
}

func newCodexFx(t *testing.T) *codexFx {
	f := newFixture(t)
	acc, err := f.q.InsertCodexProviderAccount(f.ctx, store.InsertCodexProviderAccountParams{
		UserID: f.user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	cf := &codexFx{fixture: f, account: acc.ID, gen: acc.Generation, crev: acc.CredentialRevision}
	cf.alias = cf.linkAlias()
	cf.sig = cf.currentSig()
	f.exec(`INSERT INTO runs (user_id, kind, issue_title, issue_description, harness, status, codex_secret_id) VALUES ($1, 'chat', 't', 'd', 'codex', 'running', $2)`,
		f.user, cf.alias)
	return cf
}

func (c *codexFx) linkAlias() uuid.UUID {
	c.t.Helper()
	alias := c.secret(store.KindCodexAuth, "codex")
	if _, err := c.q.InsertCodexCredentialState(c.ctx, store.InsertCodexCredentialStateParams{UserSecretID: alias, UserID: c.user, Status: "staging"}); err != nil {
		c.t.Fatalf("credential state: %v", err)
	}
	if n, err := c.q.LinkCodexCredentialState(c.ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: alias, UserID: c.user, ProviderAccountID: pgtype.UUID{Bytes: c.account, Valid: true},
	}); err != nil || n != 1 {
		c.t.Fatalf("link = (%d, %v)", n, err)
	}
	return alias
}

func (c *codexFx) currentSig() string {
	c.t.Helper()
	rows, err := c.q.ListLinkedCodexAccountsForUser(c.ctx, c.user)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ProviderAccountID == c.account {
			return r.EnablementSig
		}
	}
	c.t.Fatal("account not listed")
	return ""
}

func (c *codexFx) fence() notifysvc.CodexAccountFence {
	return notifysvc.CodexAccountFence{OwnerID: c.user, AccountID: c.account, Generation: c.gen, CredentialRevision: c.crev, EnablementSig: c.sig}
}

type cwin struct {
	bucket string
	pct    float64
	reset  time.Time
}

func buckets(ws ...cwin) []apitypes.CodexRateLimitBucketDTO {
	var out []apitypes.CodexRateLimitBucketDTO
	for _, w := range ws {
		out = append(out, apitypes.CodexRateLimitBucketDTO{ID: w.bucket, Primary: weeklyWindow(fp(w.pct), w.reset.Unix())})
	}
	return out
}

func (c *codexFx) observe(k int, fence notifysvc.CodexAccountFence, ws ...cwin) {
	c.t.Helper()
	c.w.ObserveCodexWeekly(c.ctx, fence, buckets(ws...), c.at(k))
}

func (c *codexFx) ledger() ledger { return c.ledgerWhere("codex_account_id", c.account) }

func TestWeeklyWarnCodexWindowsLiveDB(t *testing.T) {
	cf := newCodexFx(t)
	reset := cf.resetIn(100)
	cf.observe(1, cf.fence(), cwin{"codex", 40, reset})
	l := cf.ledger()
	if !l.exists || l.bucket == nil || *l.bucket != "codex" || l.anchor == nil || !l.anchor.Equal(reset.Truncate(time.Second)) {
		t.Fatalf("Codex seed = %+v", l)
	}
	cf.observe(2, cf.fence(), cwin{"codex", 91, reset})
	cf.wantDMs(1, "first near reading")
	p := cf.published()[0]
	if p.n.Slack == nil || p.n.Slack.DeliveryTag != notifysvc.DeliveryTagCodexWeeklyWarning || p.n.Slack.CodexAccount == nil ||
		*p.n.Slack.CodexAccount != cf.fence() || p.n.Slack.Credential != nil || !p.committed {
		t.Fatalf("Codex notification = %+v", p)
	}
	// An unchanged-boundary refill rearms; the next near reading warns again.
	cf.observe(3, cf.fence(), cwin{"codex", 0, reset})
	cf.observe(4, cf.fence(), cwin{"codex", 90, reset})
	cf.wantDMs(2, "refill")
	// A second alias of the same account shares the ledger row.
	cf.linkAlias()
	cf.sig = cf.currentSig()
	cf.observe(5, cf.fence(), cwin{"codex", 93, reset})
	if cf.ledgerRows() != 1 {
		t.Fatalf("ledger rows = %d, want one per account", cf.ledgerRows())
	}
	cf.wantDMs(2, "aliases share one ledger")
}

func TestWeeklyWarnCodexRelinkKeepsRowAndGenerationFenceLiveDB(t *testing.T) {
	cf := newCodexFx(t)
	reset := cf.resetIn(100)
	old := cf.fence()
	cf.observe(1, old, cwin{"codex", 93, reset})
	cf.wantDMs(1, "before the relink")
	before := cf.ledger()

	// Relink: the alias is replaced by a fresh one on the same provider account (deleting an alias
	// removes only the alias), and the refresh/promote path bumps the account generation.
	cf.exec(`DELETE FROM runs WHERE user_id = $1`, cf.user)
	cf.exec(`DELETE FROM user_secrets WHERE id = $1`, cf.alias)
	cf.alias = cf.linkAlias()
	cf.exec(`UPDATE codex_provider_account SET generation = generation + 1 WHERE id = $1`, cf.account)
	cf.exec(`INSERT INTO runs (user_id, kind, issue_title, issue_description, harness, status, codex_secret_id) VALUES ($1, 'chat', 't', 'd', 'codex', 'running', $2)`, cf.user, cf.alias)
	cf.gen++
	cf.sig = cf.currentSig()

	after := cf.ledger()
	if !after.exists || !after.warned || after.observedAt != before.observedAt {
		t.Fatalf("a relink must keep the ledger row and warned: before %+v after %+v", before, after)
	}
	// A reading taken before the relink carries the old generation: dropped.
	cf.observe(2, old, cwin{"codex", 99, reset})
	if l := cf.ledger(); !l.observedAt.Equal(before.observedAt) {
		t.Fatalf("a pre-relink reading was applied: %+v", l)
	}
	// A reading under the new generation matches the same boundary: still warned, no second DM.
	cf.observe(3, cf.fence(), cwin{"codex", 99, reset})
	cf.wantDMs(1, "relink does not re-warn")
	if l := cf.ledger(); !l.warned || !l.observedAt.Equal(cf.at(3)) {
		t.Fatalf("post-relink observation = %+v", l)
	}
}

func TestWeeklyWarnCodexCascadeOnAccountDeleteLiveDB(t *testing.T) {
	cf := newCodexFx(t)
	cf.observe(1, cf.fence(), cwin{"codex", 40, cf.resetIn(100)})
	if !cf.ledger().exists {
		t.Fatal("no ledger row")
	}
	cf.exec(`DELETE FROM runs WHERE user_id = $1`, cf.user)
	cf.exec(`DELETE FROM user_secrets WHERE id = $1`, cf.alias)
	cf.exec(`DELETE FROM codex_provider_account WHERE id = $1`, cf.account)
	if cf.ledger().exists {
		t.Fatal("deleting the account did not cascade the ledger row")
	}
}
