package store

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #4771 m1: the stored analytics documents, their invalidation triggers and the session that
// builds and publishes them. Tests share one database and run serially (no t.Parallel), so each one
// starts by emptying the docs table and cleans up only the users and repos it created.

const apSection = "ap-test-section"

type apEnv struct {
	ctx  context.Context
	pool *pgxpool.Pool
	q    *Queries
}

func apOpenEnv(t *testing.T) apEnv {
	t.Helper()
	ctx, pool, q := openRepoActivityLiveDB(t)
	raExec(ctx, t, pool, `DELETE FROM analytics_precomputed_docs`)
	e := apEnv{ctx: ctx, pool: pool, q: q}
	// A previous failed run may have left the lock held by a session that is still ending.
	s := e.acquire(t, 10*time.Second)
	e.closeOK(t, s)
	return e
}

func (e apEnv) counter(t *testing.T) int64 {
	t.Helper()
	n, err := e.q.ReadAnalyticsPrecomputeCounter(e.ctx)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return n
}

func (e apEnv) publications(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, `SELECT publications FROM analytics_precomputed_state WHERE id`).Scan(&n); err != nil {
		t.Fatalf("read publications: %v", err)
	}
	return n
}

func (e apEnv) docCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM analytics_precomputed_docs`).Scan(&n); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	return n
}

// acquire polls until a session opens or the wait ends.
func (e apEnv) acquire(t *testing.T, wait time.Duration) *AnalyticsPrecomputeSession {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
		if err != nil {
			t.Fatalf("open session: %v", err)
		}
		if ok {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the precompute lock was not acquirable within %v", wait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e apEnv) closeOK(t *testing.T, s *AnalyticsPrecomputeSession) {
	t.Helper()
	unlocked, err := s.Close(e.ctx)
	if err != nil || !unlocked {
		t.Fatalf("Close = (%v, %v), want (true, nil)", unlocked, err)
	}
}

// open returns a session closed at test end whatever state it is in.
func (e apEnv) open(t *testing.T) *AnalyticsPrecomputeSession {
	t.Helper()
	s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
	if err != nil || !ok {
		t.Fatalf("OpenAnalyticsPrecomputeSession = (%v, %v), want acquired", ok, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	return s
}

func apNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func apDocs(prefix string, keys ...string) []AnalyticsPrecomputedDoc {
	out := make([]AnalyticsPrecomputedDoc, 0, len(keys))
	for _, k := range keys {
		out = append(out, AnalyticsPrecomputedDoc{Key: k, Document: []byte(prefix + ":" + k)})
	}
	return out
}

// publish builds (reads the counter) and publishes docs on a fresh session, then closes it.
func (e apEnv) publish(t *testing.T, at time.Time, docs []AnalyticsPrecomputedDoc) int64 {
	t.Helper()
	s := e.open(t)
	var counter int64
	if err := s.RunBuild(e.ctx, func(_ context.Context, _ *AnalyticsSnapshot, c int64) error { counter = c; return nil }); err != nil {
		t.Fatalf("RunBuild: %v", err)
	}
	if err := s.Publish(e.ctx, apSection, counter, at, docs); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	e.closeOK(t, s)
	return counter
}

// assertSet requires the section to hold exactly docs, all generated at at, read in ONE statement.
func (e apEnv) assertSet(t *testing.T, label string, at time.Time, docs []AnalyticsPrecomputedDoc) {
	t.Helper()
	rows, err := e.q.ListAnalyticsPrecomputedDocs(e.ctx, apSection)
	if err != nil {
		t.Fatalf("%s: list: %v", label, err)
	}
	want := append([]AnalyticsPrecomputedDoc(nil), docs...)
	sort.Slice(want, func(i, j int) bool { return want[i].Key < want[j].Key })
	if len(rows) != len(want) {
		t.Fatalf("%s: %d docs, want %d (%v)", label, len(rows), len(want), rows)
	}
	for i, r := range rows {
		if r.VariantKey != want[i].Key || !bytes.Equal(r.Document, want[i].Document) || !r.GeneratedAt.Time.Equal(at) {
			t.Fatalf("%s: doc %d = (%q, %q, %v), want (%q, %q, %v)", label, i, r.VariantKey, r.Document, r.GeneratedAt.Time, want[i].Key, want[i].Document, at)
		}
	}
}

func apWait(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func apAsync(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

// waitLockWait polls until backend pid is waiting on a lock (the positive barrier that replaces
// inferring "blocked" from elapsed time), bounded at 10 seconds.
func (e apEnv) waitLockWait(t *testing.T, pid int32, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock'`, pid).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (backend %d) never waited on a lock", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func apSQLState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// seedRepo creates a user, a connection and n disabled repos, removed with the user at test end.
func (e apEnv) seedRepos(t *testing.T, n int) (user, conn uuid.UUID, repos []uuid.UUID) {
	t.Helper()
	user, conn = raSeedUserConn(e.ctx, t, e.pool, true)
	for i := 0; i < n; i++ {
		repos = append(repos, raSeedRepo(e.ctx, t, e.pool, conn, "ap/"+uuid.NewString(), false))
	}
	return user, conn, repos
}

// Mutation it must catch: split Publish into two transactions (delete+commit, then insert+commit),
// or insert before the delete is part of the same transaction. At every pause point a whole-set
// single-statement reader must see set A complete; after commit, set B complete.
func TestAnalyticsPrecomputeAtomicReplaceLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	atA, atB := apNow().Add(-time.Hour), apNow()
	setA := apDocs("A", "a", "b", "c")
	setB := apDocs("B", "b", "c", "d", "e")
	e.publish(t, atA, setA)
	e.assertSet(t, "before", atA, setA)

	s := e.open(t)
	var counter int64
	if err := s.RunBuild(e.ctx, func(_ context.Context, _ *AnalyticsSnapshot, c int64) error { counter = c; return nil }); err != nil {
		t.Fatal(err)
	}
	paused, resume := make(chan string, 16), make(chan struct{})
	pause := func(label string) { paused <- label; <-resume }
	s.hooks.afterPublishDelete = func() { pause("delete") }
	s.hooks.afterPublishInsert = func(i int) { pause("insert") }
	done := apAsync(func() error { return s.Publish(e.ctx, apSection, counter, atB, setB) })

	for i := 0; i < 1+len(setB); i++ {
		select {
		case label := <-paused:
			e.assertSet(t, "pause "+label, atA, setA)
		case err := <-done:
			t.Fatalf("publish ended before pause %d: %v", i, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("pause %d never reached", i)
		}
		resume <- struct{}{}
	}
	if err := apWait(t, done, "publish"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	e.assertSet(t, "after", atB, setB)
}

// Mutation it must catch: drop the guard (publish without comparing the counter).
func TestAnalyticsPrecomputeCounterGuardLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	at := apNow()
	setA := apDocs("A", "a", "b")
	counter := e.publish(t, at, setA)
	pubs := e.publications(t)

	s := e.open(t)
	for _, stale := range []int64{counter + 1, counter - 1} {
		err := s.Publish(e.ctx, apSection, stale, apNow(), apDocs("B", "x"))
		if !errors.Is(err, ErrAnalyticsPrecomputedInvalidated) {
			t.Fatalf("Publish(counter %d) = %v, want ErrAnalyticsPrecomputedInvalidated", stale, err)
		}
		if !s.Idle() {
			t.Fatal("session must stay usable after a refused publish")
		}
	}
	e.assertSet(t, "unchanged", at, setA)
	if got := e.publications(t); got != pubs {
		t.Fatalf("publications = %d, want %d", got, pubs)
	}
	e.closeOK(t, s)
}

// Mutation it must catch: remove the trigger function's DELETE of the docs, the counter UPDATE (it
// must move once per deleted row), or the deferral (docs vanish before COMMIT).
func TestAnalyticsPrecomputeDeleteInvalidatesLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	repoCase := func(name string, rows int64, del func(user, conn uuid.UUID, repos []uuid.UUID) error) {
		t.Run(name, func(t *testing.T) {
			user, conn, repos := e.seedRepos(t, 2)
			at := apNow()
			setA := apDocs("A", "a", "b", "c")
			e.publish(t, at, setA)
			before := e.counter(t)
			if err := del(user, conn, repos); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if n := e.docCount(t); n != 0 {
				t.Fatalf("%d docs after the delete committed, want 0", n)
			}
			if got := e.counter(t); got != before+rows {
				t.Fatalf("counter = %d, want %d (one increment per deleted users or repos row)", got, before+rows)
			}
		})
	}
	repoCase("repo via DeleteRepoForUser", 1, func(user, _ uuid.UUID, repos []uuid.UUID) error {
		n, err := e.q.DeleteRepoForUser(e.ctx, DeleteRepoForUserParams{ID: repos[0], UserID: user})
		if err == nil && n != 1 {
			err = errors.New("repo not deleted")
		}
		return err
	})
	repoCase("connection cascading to two repos", 2, func(user, conn uuid.UUID, _ []uuid.UUID) error {
		n, err := e.q.DeleteForgeConnectionForUser(e.ctx, DeleteForgeConnectionForUserParams{ID: conn, UserID: user})
		if err == nil && n != 1 {
			err = errors.New("connection not deleted")
		}
		return err
	})
	repoCase("user cascading to connection and repos", 3, func(user, _ uuid.UUID, _ []uuid.UUID) error {
		_, err := e.pool.Exec(e.ctx, `DELETE FROM users WHERE id = $1`, user)
		return err
	})
	repoCase("two repos in one transaction", 2, func(_, _ uuid.UUID, repos []uuid.UUID) error {
		_, err := e.pool.Exec(e.ctx, `DELETE FROM repos WHERE id = ANY($1)`, repos)
		return err
	})
	t.Run("user without children", func(t *testing.T) {
		user := uuid.New()
		raExec(e.ctx, t, e.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, "ap-"+user.String()+"@e2e")
		e.publish(t, apNow(), apDocs("A", "a"))
		before := e.counter(t)
		raExec(e.ctx, t, e.pool, `DELETE FROM users WHERE id = $1`, user)
		if n := e.docCount(t); n != 0 || e.counter(t) != before+1 {
			t.Fatalf("docs = %d, counter = %d, want 0 and %d", n, e.counter(t), before+1)
		}
	})
	t.Run("deferred until commit", func(t *testing.T) {
		_, _, repos := e.seedRepos(t, 1)
		at := apNow()
		setA := apDocs("A", "a", "b")
		e.publish(t, at, setA)
		before := e.counter(t)
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, repos[0]); err != nil {
			t.Fatal(err)
		}
		var inTx int64
		if err := tx.QueryRow(e.ctx, `SELECT count(*) FROM analytics_precomputed_docs`).Scan(&inTx); err != nil || inTx != int64(len(setA)) {
			t.Fatalf("docs inside the deleting transaction = %d (%v), want %d: the trigger must be deferred", inTx, err, len(setA))
		}
		e.assertSet(t, "other session before commit", at, setA)
		if got := e.counter(t); got != before {
			t.Fatalf("counter moved to %d before commit", got)
		}
		if err := tx.Commit(e.ctx); err != nil {
			t.Fatal(err)
		}
		if e.docCount(t) != 0 || e.counter(t) != before+1 {
			t.Fatalf("after commit docs = %d, counter = %d", e.docCount(t), e.counter(t))
		}
	})
}

// Mutation it must catch: an invalidation guard keyed on the custom setting
// uzi.analytics_precomputed_invalidated. A caller that sets it to its own txid must not be able to
// suppress the invalidation.
func TestAnalyticsPrecomputeInvalidationIgnoresCallerSettingLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 1)
	e.publish(t, apNow(), apDocs("A", "a", "b"))
	before := e.counter(t)

	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(e.ctx, `SELECT set_config('uzi.analytics_precomputed_invalidated', txid_current()::text, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, repos[0]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.docCount(t); n != 0 {
		t.Fatalf("%d docs left after a delete whose caller preset the setting, want 0", n)
	}
	if got := e.counter(t); got != before+1 {
		t.Fatalf("counter = %d, want %d", got, before+1)
	}
}

// Mutation it must catch: a trigger that fires for a statement that deleted nothing (a statement
// level trigger instead of FOR EACH ROW), a missing TRUNCATE trigger, or the wrong function.
func TestAnalyticsPrecomputeZeroRowAndTruncateLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	user, conn, _ := e.seedRepos(t, 0)
	at := apNow()
	setA := apDocs("A", "a", "b")
	e.publish(t, at, setA)
	before := e.counter(t)

	raExec(e.ctx, t, e.pool, `DELETE FROM repos WHERE id = $1`, uuid.New())
	raExec(e.ctx, t, e.pool, `DELETE FROM users WHERE id = $1`, uuid.New())
	if n, err := e.q.DeleteForgeConnectionForUser(e.ctx, DeleteForgeConnectionForUserParams{ID: conn, UserID: user}); err != nil || n != 1 {
		t.Fatalf("delete connection = (%d, %v)", n, err)
	}
	e.assertSet(t, "after zero-row deletes", at, setA)
	if got := e.counter(t); got != before {
		t.Fatalf("counter = %d, want unchanged %d", got, before)
	}

	// The triggers exist, call the invalidation function and have the documented shape.
	rows, err := e.pool.Query(e.ctx, `
		SELECT t.tgname, c.relname, p.proname,
		       (t.tgtype & 32) <> 0 AS truncate_trigger,
		       (t.tgtype & 1) <> 0 AS row_trigger,
		       t.tgdeferrable AND t.tginitdeferred AS deferred
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE p.proname = 'analytics_precomputed_invalidate' AND NOT t.tgisinternal`)
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		table, fn               string
		truncate, row, deferred bool
	}
	got := map[string]shape{}
	for rows.Next() {
		var name string
		var s shape
		if err := rows.Scan(&name, &s.table, &s.fn, &s.truncate, &s.row, &s.deferred); err != nil {
			t.Fatal(err)
		}
		got[name] = s
	}
	rows.Close()
	want := map[string]shape{
		"analytics_precomputed_invalidate_users":          {"users", "analytics_precomputed_invalidate", false, true, true},
		"analytics_precomputed_invalidate_repos":          {"repos", "analytics_precomputed_invalidate", false, true, true},
		"analytics_precomputed_invalidate_users_truncate": {"users", "analytics_precomputed_invalidate", true, false, false},
		"analytics_precomputed_invalidate_repos_truncate": {"repos", "analytics_precomputed_invalidate", true, false, false},
	}
	if len(got) != len(want) {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("trigger %s = %+v, want %+v", k, got[k], w)
		}
	}

	// A real TRUNCATE, rolled back so no other test's rows are lost.
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(e.ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `TRUNCATE repos CASCADE`); err != nil {
		t.Fatal(err)
	}
	var docs, counter int64
	if err := tx.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM analytics_precomputed_docs), (SELECT invalidation FROM analytics_precomputed_state WHERE id)`).Scan(&docs, &counter); err != nil {
		t.Fatal(err)
	}
	if docs != 0 || counter != before+1 {
		t.Fatalf("inside TRUNCATE transaction docs = %d, counter = %d, want 0 and %d", docs, counter, before+1)
	}
	if err := tx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.assertSet(t, "after rolled-back truncate", at, setA)
}

// Mutation it must catch: in the trigger function, DELETE the docs before the UPDATE of the state
// row (or mark the function STABLE): the DELETE then runs on a snapshot taken before a publisher's
// commit and leaves the freshly published set behind.
func TestAnalyticsPrecomputeReadCommittedInterleaveLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 1)
	e.publish(t, apNow().Add(-time.Hour), apDocs("A", "a", "b"))

	s := e.open(t)
	var counter int64
	if err := s.RunBuild(e.ctx, func(_ context.Context, _ *AnalyticsSnapshot, c int64) error { counter = c; return nil }); err != nil {
		t.Fatal(err)
	}
	paused, resume := make(chan struct{}, 4), make(chan struct{})
	s.hooks.afterPublishInsert = func(i int) {
		if i == 0 {
			paused <- struct{}{}
			<-resume
		}
	}
	pub := apAsync(func() error { return s.Publish(e.ctx, apSection, counter, apNow(), apDocs("B", "b", "c")) })
	select {
	case <-paused:
	case err := <-pub:
		t.Fatalf("publish ended before its pause: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("publish never paused")
	}

	// The publisher holds the state row. The deleter commits: its deferred trigger waits on it.
	dc, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Release()
	var delPID int32
	if err := dc.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&delPID); err != nil {
		t.Fatal(err)
	}
	del := apAsync(func() error {
		_, err := dc.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, repos[0])
		return err
	})
	e.waitLockWait(t, delPID, "the delete commit")
	select {
	case derr := <-del:
		t.Fatalf("the delete commit finished while the publisher held the state row: %v", derr)
	default:
	}
	resume <- struct{}{}
	if err := apWait(t, pub, "publish"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := apWait(t, del, "delete"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := e.docCount(t); n != 0 {
		t.Fatalf("%d docs left behind by the publisher that committed while the delete waited", n)
	}
	if got := e.counter(t); got != counter+1 {
		t.Fatalf("counter = %d, want %d", got, counter+1)
	}
}

// Mutation it must catch: a publisher that takes the state row with SELECT ... FOR UPDATE (or no
// lock) instead of the guarded UPDATE. The row version would not change, so a REPEATABLE READ
// deleter would commit and leave the published set behind.
func TestAnalyticsPrecomputeRepeatableReadInterleaveLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 1)

	d, err := e.pool.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Rollback(context.Background()) }()
	if _, err := d.Exec(e.ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, repos[0]); err != nil {
		t.Fatal(err)
	}
	e.publish(t, apNow(), apDocs("A", "a", "b"))

	err = d.Commit(e.ctx)
	if apSQLState(err) != "40001" {
		t.Fatalf("deleter COMMIT = %v, want SQLSTATE 40001", err)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM repos WHERE id = $1`, repos[0]).Scan(&n); err != nil || n != 1 {
		t.Fatalf("repo count = %d (%v), want the repo still present", n, err)
	}
	if got := e.docCount(t); got != 2 {
		t.Fatalf("docs = %d, want the published set intact", got)
	}
}

// Mutation it must catch: make the row triggers non-deferred (plain AFTER DELETE FOR EACH ROW):
// the first delete then holds the state row while waiting on a source row and the cycle closes.
func TestAnalyticsPrecomputeNoDeadlockLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 2)
	r1, r2 := repos[0], repos[1]
	e.publish(t, apNow(), apDocs("A", "a"))

	a, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Rollback(context.Background()) }()
	b, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Rollback(context.Background()) }()

	var errs []error
	var mu sync.Mutex
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	var aPID int32
	if err := a.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&aPID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, r1); err != nil {
		t.Fatalf("A delete r1: %v", err)
	}
	bDel := apAsync(func() error { _, err := b.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, r2); return err })
	// B's delete takes only the r2 row lock (its trigger is deferred), so it returns on its own.
	bDelErr := apWait(t, bDel, "B delete r2")
	aDel := apAsync(func() error { _, err := a.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, r2); return err })
	// Barrier: A is waiting on B's r2 row lock before B commits.
	e.waitLockWait(t, aPID, "A delete r2")
	// B's delete must have returned before B's commit starts (a transaction is used by one
	// goroutine at a time). Without the deferral it is stuck here until the deadlock resolves.
	record := func(name string, err error) {
		if err != nil {
			note(err)
			t.Errorf("%s: %v (SQLSTATE %s)", name, err, apSQLState(err))
		}
	}
	record("B delete r2", bDelErr)
	bCommit := apAsync(func() error { return b.Commit(e.ctx) })
	record("B commit", apWait(t, bCommit, "B commit"))
	record("A delete r2", apWait(t, aDel, "A delete r2"))
	if err := a.Commit(e.ctx); err != nil {
		t.Errorf("A commit: %v (SQLSTATE %s)", err, apSQLState(err))
	}
	for _, err := range errs {
		if apSQLState(err) == "40P01" {
			t.Fatalf("deadlock detected: %v", err)
		}
	}
}

// Mutation it must catch: Publish that does not compare the counter the build read.
func TestAnalyticsPrecomputeBuildRaceLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 1)
	e.publish(t, apNow().Add(-time.Hour), apDocs("A", "a", "b"))

	s := e.open(t)
	var built int64
	err := s.RunBuild(e.ctx, func(ctx context.Context, snap *AnalyticsSnapshot, c int64) error {
		built = c
		// A delete commits from another connection while the build snapshot is open.
		_, derr := e.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, repos[0])
		return derr
	})
	if err != nil {
		t.Fatalf("RunBuild: %v", err)
	}
	err = s.Publish(e.ctx, apSection, built, apNow(), apDocs("B", "x", "y"))
	if !errors.Is(err, ErrAnalyticsPrecomputedInvalidated) {
		t.Fatalf("Publish = %v, want ErrAnalyticsPrecomputedInvalidated", err)
	}
	if n := e.docCount(t); n != 0 {
		t.Fatalf("%d docs after a refused publish, want 0", n)
	}
	if got := e.counter(t); got != built+1 {
		t.Fatalf("counter = %d, want %d", got, built+1)
	}
	e.closeOK(t, s)
}

// Mutation it must catch: unlocking or closing nothing on an early exit, or a second opener that
// does not use pg_try_advisory_lock (it would block or report acquired).
func TestAnalyticsPrecomputeTryLockAndEarlyExitsLiveDB(t *testing.T) {
	e := apOpenEnv(t)

	t.Run("second opener is refused until the first closes", func(t *testing.T) {
		first := e.open(t)
		s2, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
		if s2 != nil || ok || err != nil {
			t.Fatalf("second open = (%v, %v, %v), want (nil, false, nil)", s2, ok, err)
		}
		e.closeOK(t, first)
		if unlocked, err := first.Close(e.ctx); unlocked || err != nil {
			t.Fatalf("second Close = (%v, %v), want (false, nil)", unlocked, err)
		}
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
	t.Run("skip without a transaction", func(t *testing.T) {
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
	t.Run("build fn error", func(t *testing.T) {
		s := e.open(t)
		boom := errors.New("boom")
		if err := s.RunBuild(e.ctx, func(context.Context, *AnalyticsSnapshot, int64) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("RunBuild = %v, want boom", err)
		}
		if !s.Idle() {
			t.Fatal("session must be idle after an fn error")
		}
		e.closeOK(t, s)
	})
	t.Run("publish error keeps the previous set", func(t *testing.T) {
		at := apNow()
		setA := apDocs("A", "a", "b")
		counter := e.publish(t, at, setA)
		s := e.open(t)
		dup := []AnalyticsPrecomputedDoc{{Key: "x", Document: []byte("1")}, {Key: "x", Document: []byte("2")}}
		err := s.Publish(e.ctx, apSection, counter, apNow(), dup)
		if err == nil || errors.Is(err, ErrAnalyticsPrecomputePublishUnresolved) || errors.Is(err, ErrAnalyticsPrecomputedInvalidated) {
			t.Fatalf("Publish = %v, want a plain pre-commit error", err)
		}
		e.assertSet(t, "after failed publish", at, setA)
		e.closeOK(t, s)
	})
	t.Run("invalidated publish", func(t *testing.T) {
		s := e.open(t)
		if err := s.Publish(e.ctx, apSection, e.counter(t)+7, apNow(), apDocs("B", "x")); !errors.Is(err, ErrAnalyticsPrecomputedInvalidated) {
			t.Fatalf("Publish = %v", err)
		}
		e.closeOK(t, s)
	})
	t.Run("context cancelled during the build", func(t *testing.T) {
		s := e.open(t)
		ctx, cancel := context.WithCancel(e.ctx)
		time.AfterFunc(200*time.Millisecond, cancel)
		err := s.RunBuild(ctx, func(ctx context.Context, snap *AnalyticsSnapshot, _ int64) error {
			_, err := snap.tx.Exec(ctx, `SELECT pg_sleep(30)`)
			return err
		})
		if err == nil {
			t.Fatal("RunBuild returned nil after cancellation")
		}
		_, _ = s.Close(e.ctx)
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
	t.Run("fault after the try lock", func(t *testing.T) {
		boom := errors.New("injected")
		analyticsPrecomputeAfterTryLock = func() error { return boom }
		s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
		analyticsPrecomputeAfterTryLock = nil
		if s != nil || ok || !errors.Is(err, boom) {
			t.Fatalf("open = (%v, %v, %v), want the injected error", s, ok, err)
		}
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
	t.Run("rollback fault", func(t *testing.T) {
		s := e.open(t)
		counter := e.counter(t)
		boom := errors.New("rollback boom")
		s.hooks.rollbackFault = func() error { return boom }
		if err := s.RunBuild(e.ctx, func(context.Context, *AnalyticsSnapshot, int64) error { return nil }); !errors.Is(err, boom) {
			t.Fatalf("RunBuild = %v, want the rollback error returned", err)
		}
		if s.Idle() {
			t.Fatal("session must be broken after a failed rollback")
		}
		before := e.docCount(t)
		if err := s.Publish(e.ctx, apSection, counter, apNow(), apDocs("B", "x")); err == nil {
			t.Fatal("Publish on a broken session succeeded")
		}
		if e.docCount(t) != before {
			t.Fatal("a broken session published")
		}
		if unlocked, _ := s.Close(e.ctx); unlocked {
			t.Fatal("a broken session must not claim a proven unlock")
		}
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
}

// Mutation it must catch: TCP settings folded into the required statement (a refusal would then
// fail the open), or a failure of them that is treated as fatal.
func TestAnalyticsPrecomputeTCPSettingsBestEffortLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	analyticsPrecomputeTCPSettingsFault = func() error { return errors.New("injected tcp failure") }
	s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
	analyticsPrecomputeTCPSettingsFault = nil
	if err != nil || !ok {
		t.Fatalf("open with a failing TCP statement = (%v, %v), want acquired", ok, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	var got string
	if err := s.conn.QueryRow(e.ctx, "SHOW idle_session_timeout").Scan(&got); err != nil || got != "1min" {
		t.Fatalf("required settings: idle_session_timeout = %q (%v)", got, err)
	}
	if s2, ok2, err2 := OpenAnalyticsPrecomputeSession(e.ctx, e.pool); s2 != nil || ok2 || err2 != nil {
		t.Fatalf("second open = (%v, %v, %v): the first session must hold the lock", s2, ok2, err2)
	}
	e.closeOK(t, s)
}

// Mutation it must catch: Close that unlocks (or gives up) without ending an open transaction, as
// after a panic in a RunBuild fn.
func TestAnalyticsPrecomputeCloseRollsBackOpenTxLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	s := e.open(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fn did not panic")
			}
		}()
		_ = s.RunBuild(e.ctx, func(context.Context, *AnalyticsSnapshot, int64) error { panic("boom") })
	}()
	if s.Idle() {
		t.Fatal("session must still hold the open build transaction after the panic")
	}
	e.closeOK(t, s)
	e.closeOK(t, e.acquire(t, 5*time.Second))
}

// Mutation it must catch: session settings that are not applied (no client_connection_check_interval
// or idle timeouts), so a lost client leaves the lock held, or a lock that outlives its connection.
func TestAnalyticsPrecomputeProcessLossLiveDB(t *testing.T) {
	e := apOpenEnv(t)

	t.Run("session settings", func(t *testing.T) {
		s := e.open(t)
		for name, want := range map[string]string{
			"statement_timeout":                   "10min",
			"lock_timeout":                        "10s",
			"idle_session_timeout":                "1min",
			"idle_in_transaction_session_timeout": "1min",
			"client_connection_check_interval":    "1s",
			"tcp_keepalives_idle":                 "30",
			"tcp_keepalives_interval":             "10",
			"tcp_keepalives_count":                "3",
			"tcp_user_timeout":                    "60000",
		} {
			var got string
			if err := s.conn.QueryRow(e.ctx, "SHOW "+name).Scan(&got); err != nil || got != want {
				t.Errorf("SHOW %s = %q (%v), want %q", name, got, err, want)
			}
		}
		e.closeOK(t, s)
	})
	killSocket := func(s *AnalyticsPrecomputeSession) { _ = s.conn.PgConn().Conn().Close() }

	t.Run("idle in transaction", func(t *testing.T) {
		at := apNow()
		setA := apDocs("A", "a", "b")
		e.publish(t, at, setA)
		s := e.open(t)
		_ = s.RunBuild(e.ctx, func(context.Context, *AnalyticsSnapshot, int64) error {
			killSocket(s)
			return nil
		})
		_, _ = s.Close(e.ctx)
		e.closeOK(t, e.acquire(t, 5*time.Second))
		e.assertSet(t, "after loss", at, setA)
	})
	t.Run("during a paused publish", func(t *testing.T) {
		at := apNow()
		setA := apDocs("A", "a", "b")
		counter := e.publish(t, at, setA)
		s := e.open(t)
		s.hooks.afterPublishDelete = func() { killSocket(s) }
		err := s.Publish(e.ctx, apSection, counter, apNow(), apDocs("B", "c"))
		if err == nil {
			t.Fatal("Publish succeeded on a killed connection")
		}
		_, _ = s.Close(e.ctx)
		e.closeOK(t, e.acquire(t, 5*time.Second))
		e.assertSet(t, "after loss", at, setA)
	})
	t.Run("while a statement is active", func(t *testing.T) {
		at := apNow()
		setA := apDocs("A", "a", "b")
		e.publish(t, at, setA)
		s := e.open(t)
		err := s.RunBuild(e.ctx, func(ctx context.Context, snap *AnalyticsSnapshot, _ int64) error {
			time.AfterFunc(200*time.Millisecond, func() { killSocket(s) })
			_, err := snap.tx.Exec(ctx, `SELECT pg_sleep(30)`)
			return err
		})
		if err == nil {
			t.Fatal("RunBuild succeeded on a killed connection")
		}
		_, _ = s.Close(e.ctx)
		e.closeOK(t, e.acquire(t, 5*time.Second))
		e.assertSet(t, "after loss", at, setA)
	})
	t.Run("after commit before unlock", func(t *testing.T) {
		counter := e.publish(t, apNow().Add(-time.Hour), apDocs("A", "a", "b"))
		s := e.open(t)
		atB := apNow()
		setB := apDocs("B", "c", "d")
		s.hooks.afterPublishCommit = func() error { killSocket(s); return nil }
		if err := s.Publish(e.ctx, apSection, counter, atB, setB); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if unlocked, _ := s.Close(e.ctx); unlocked {
			t.Fatal("Close claimed a proven unlock on a killed connection")
		}
		e.closeOK(t, e.acquire(t, 5*time.Second))
		e.assertSet(t, "after commit", atB, setB)
	})
	t.Run("lost commit acknowledgement is unresolved", func(t *testing.T) {
		counter := e.publish(t, apNow().Add(-time.Hour), apDocs("A", "a"))
		s := e.open(t)
		atB := apNow()
		setB := apDocs("B", "c")
		s.hooks.afterPublishCommit = func() error { return errors.New("ack lost") }
		err := s.Publish(e.ctx, apSection, counter, atB, setB)
		if !errors.Is(err, ErrAnalyticsPrecomputePublishUnresolved) {
			t.Fatalf("Publish = %v, want ErrAnalyticsPrecomputePublishUnresolved", err)
		}
		if s.Idle() {
			t.Fatal("session must be closed after an unresolved publish")
		}
		e.assertSet(t, "the commit did happen", atB, setB)
		_, _ = s.Close(e.ctx)
		e.closeOK(t, e.acquire(t, 5*time.Second))
	})
}

// Mutation it must catch: a missing state row that is tolerated (the trigger's IF NOT FOUND raise
// removed, or Publish and RunBuild treating no row as counter zero).
func TestAnalyticsPrecomputeMissingStateRowLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 2)

	// The trigger raises, so a delete cannot commit without bumping the counter.
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(e.ctx, `DELETE FROM analytics_precomputed_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, repos[0]); err != nil {
		t.Fatalf("deferred trigger must not fire before commit: %v", err)
	}
	if err := tx.Commit(e.ctx); err == nil {
		t.Fatal("COMMIT succeeded without a state row")
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM analytics_precomputed_state) * 10 + (SELECT count(*) FROM repos WHERE id = $1)`, repos[0]).Scan(&n); err != nil || n != 11 {
		t.Fatalf("state rows and repo = %d (%v), want both still present (11)", n, err)
	}

	// Committed absence: every state consumer errors. The row is restored with its values.
	var inval, pubs int64
	var fails int32
	var lastFail pgtype.Timestamptz
	if err := e.pool.QueryRow(e.ctx, `SELECT invalidation, publications, failure_count, last_failure_at FROM analytics_precomputed_state WHERE id`).Scan(&inval, &pubs, &fails, &lastFail); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM analytics_precomputed_state`)
		_, err := e.pool.Exec(context.Background(),
			`INSERT INTO analytics_precomputed_state (invalidation, publications, failure_count, last_failure_at) VALUES ($1, $2, $3, $4)`, inval, pubs, fails, lastFail)
		if err != nil {
			t.Errorf("restoring the state row: %v", err)
		}
	})
	raExec(e.ctx, t, e.pool, `DELETE FROM analytics_precomputed_state`)

	s := e.open(t)
	if err := s.RunBuild(e.ctx, func(context.Context, *AnalyticsSnapshot, int64) error { return nil }); err == nil {
		t.Fatal("RunBuild succeeded without a state row")
	}
	if !s.Idle() {
		t.Fatal("session must stay usable after the build error")
	}
	if err := s.Publish(e.ctx, apSection, 0, apNow(), apDocs("B", "x")); err == nil || errors.Is(err, ErrAnalyticsPrecomputedInvalidated) {
		t.Fatalf("Publish = %v, want a missing-row error", err)
	}
	if e.docCount(t) != 0 {
		t.Fatal("published without a state row")
	}
	if err := RecordAnalyticsPrecomputeFailure(e.ctx, e.pool, apNow()); err == nil {
		t.Fatal("RecordAnalyticsPrecomputeFailure succeeded without a state row")
	}
	e.closeOK(t, s)
}

// Mutation it must catch: GetAnalyticsPrecomputedDoc ignoring the variant key (or the section).
func TestAnalyticsPrecomputeKeyedReadLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	at := apNow()
	docs := []AnalyticsPrecomputedDoc{
		{Key: "k1", Document: []byte(`{"a":1}`)},
		{Key: "k2", Document: []byte{0x00, 0xff, 0x7f, 0x0a}},
		{Key: "k3", Document: []byte(`{"c":"three"}`)},
	}
	e.publish(t, at, docs)
	for _, d := range docs {
		row, err := e.q.GetAnalyticsPrecomputedDoc(e.ctx, GetAnalyticsPrecomputedDocParams{Section: apSection, VariantKey: d.Key})
		if err != nil || !bytes.Equal(row.Document, d.Document) || !row.GeneratedAt.Time.Equal(at) {
			t.Fatalf("Get %s = (%q, %v, %v), want exactly %q", d.Key, row.Document, row.GeneratedAt.Time, err, d.Document)
		}
	}
	if _, err := e.q.GetAnalyticsPrecomputedDoc(e.ctx, GetAnalyticsPrecomputedDocParams{Section: "other", VariantKey: "k1"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Get in another section = %v, want no rows", err)
	}
	e.assertSet(t, "list", at, docs)
}

// Mutation it must catch: Publish that does not reset the failure status, or a status read that
// ignores the requested keys.
func TestAnalyticsPrecomputeStatusLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	at := apNow().Add(-time.Minute)
	e.publish(t, at, apDocs("A", "a", "b", "c"))

	failAt := apNow()
	if err := RecordAnalyticsPrecomputeFailure(e.ctx, e.pool, failAt); err != nil {
		t.Fatal(err)
	}
	if err := RecordAnalyticsPrecomputeFailure(e.ctx, e.pool, failAt); err != nil {
		t.Fatal(err)
	}
	st, err := e.q.AnalyticsPrecomputeStatus(e.ctx, AnalyticsPrecomputeStatusParams{Section: apSection, Keys: []string{"a", "c", "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Present != 2 || !st.OldestGeneratedAt.Time.Equal(at) || st.FailureCount < 2 || !st.LastFailureAt.Valid || !st.LastFailureAt.Time.Equal(failAt) {
		t.Fatalf("status = %+v, want 2 present at %v and the failures recorded at %v", st, at, failAt)
	}
	e.publish(t, apNow(), apDocs("B", "a"))
	st, err = e.q.AnalyticsPrecomputeStatus(e.ctx, AnalyticsPrecomputeStatusParams{Section: apSection, Keys: []string{"a", "b"}})
	if err != nil || st.Present != 1 || st.FailureCount != 0 || st.LastFailureAt.Valid {
		t.Fatalf("status after publish = %+v (%v), want 1 present and the failures cleared", st, err)
	}
}

// Mutation it must catch: ReadAnalyticsPrecomputeStatus or the session's Status ignoring the keys.
func TestAnalyticsPrecomputeStatusHelpersLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	at := apNow().Add(-time.Minute)
	e.publish(t, at, apDocs("A", "a", "b"))
	failAt := apNow()
	if err := RecordAnalyticsPrecomputeFailure(e.ctx, e.pool, failAt); err != nil {
		t.Fatal(err)
	}
	want := AnalyticsPrecomputeState{Present: 1, OldestGeneratedAt: at, FailureCount: 1, LastFailureAt: failAt}
	got, err := ReadAnalyticsPrecomputeStatus(e.ctx, e.pool, apSection, []string{"a", "zzz"})
	if err != nil || got.Present != want.Present || !got.OldestGeneratedAt.Equal(at) || got.FailureCount != 1 || !got.LastFailureAt.Equal(failAt) {
		t.Fatalf("pool status = %+v (%v), want %+v", got, err, want)
	}
	s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
	if err != nil || !ok {
		t.Fatalf("open = (%v, %v)", ok, err)
	}
	defer func() { _, _ = s.Close(e.ctx) }()
	got, err = s.Status(e.ctx, apSection, []string{"none"})
	if err != nil || got.Present != 0 || !got.OldestGeneratedAt.IsZero() || got.FailureCount != 1 {
		t.Fatalf("session status = %+v (%v), want no keys present and the shared failure count", got, err)
	}
}
