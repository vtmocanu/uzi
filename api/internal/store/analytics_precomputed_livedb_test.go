package store

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
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

// apGo runs fn on its own goroutine and registers a cleanup that calls cancel (which must cancel
// the context fn uses, and release any hook fn blocks in) and then waits for fn to return. Cleanups
// run in reverse order, so register the cleanup of every connection, transaction or session fn uses
// before calling apGo: a test that fails mid-way then never releases a resource a worker still holds.
func apGo(t *testing.T, cancel context.CancelFunc, fn func() error) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ch <- fn()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			t.Errorf("background worker did not exit after cancellation")
		}
	})
	return ch
}

// waitLockWait polls until backend pid is waiting on a lock that backend blocker holds (the
// positive barrier that replaces inferring "blocked" from elapsed time), bounded at 10 seconds.
func (e apEnv) waitLockWait(t *testing.T, pid, blocker int64, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock' AND $2::int = ANY(pg_blocking_pids($1::int))`, pid, blocker).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (backend %d) never waited on a lock held by backend %d", what, pid, blocker)
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
	ctx, cancel := context.WithCancel(e.ctx)
	paused, resume := make(chan string, 16), make(chan struct{})
	pause := func(label string) {
		select {
		case paused <- label:
		case <-ctx.Done():
			return
		}
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	s.hooks.afterPublishDelete = func() { pause("delete") }
	s.hooks.afterPublishInsert = func(i int) { pause("insert") }
	done := apGo(t, cancel, func() error { return s.Publish(ctx, apSection, counter, atB, setB) })

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
// must move exactly once per deleting transaction, whatever the number of rows), or the deferral
// (docs vanish before COMMIT).
func TestAnalyticsPrecomputeDeleteInvalidatesLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	repoCase := func(name string, del func(user, conn uuid.UUID, repos []uuid.UUID) error) {
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
			if got := e.counter(t); got != before+1 {
				t.Fatalf("counter = %d, want %d (exactly one increment per deleting transaction)", got, before+1)
			}
		})
	}
	repoCase("repo via DeleteRepoForUser", func(user, _ uuid.UUID, repos []uuid.UUID) error {
		n, err := e.q.DeleteRepoForUser(e.ctx, DeleteRepoForUserParams{ID: repos[0], UserID: user})
		if err == nil && n != 1 {
			err = errors.New("repo not deleted")
		}
		return err
	})
	repoCase("connection cascading to two repos", func(user, conn uuid.UUID, _ []uuid.UUID) error {
		n, err := e.q.DeleteForgeConnectionForUser(e.ctx, DeleteForgeConnectionForUserParams{ID: conn, UserID: user})
		if err == nil && n != 1 {
			err = errors.New("connection not deleted")
		}
		return err
	})
	repoCase("user cascading to connection and repos", func(user, _ uuid.UUID, _ []uuid.UUID) error {
		_, err := e.pool.Exec(e.ctx, `DELETE FROM users WHERE id = $1`, user)
		return err
	})
	repoCase("two repos in one transaction", func(_, _ uuid.UUID, repos []uuid.UUID) error {
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

// Mutation it must catch: an invalidation guard keyed on a session setting (the earlier design used
// uzi.analytics_precomputed_invalidated). A caller that sets it to its own txid must not be able to
// suppress the invalidation. The once-per-transaction guard now lives on the state row, so this
// passes trivially; it stays as a regression guard against reintroducing a session-keyed guard.
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

// Mutation it must catch: the once-per-transaction guard removed (the counter then moves once per
// firing) or keyed so a rolled-back firing's marker survives its savepoint.
func TestAnalyticsPrecomputeCounterOncePerTransactionLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 4)
	e.publish(t, apNow(), apDocs("A", "a"))
	before := e.counter(t)

	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	inTx := func() int64 {
		t.Helper()
		var n int64
		if err := tx.QueryRow(e.ctx, `SELECT invalidation FROM analytics_precomputed_state WHERE id`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A rolled-back firing leaves no marker behind: the delete after the savepoint still advances.
	run(`SAVEPOINT sp`)
	run(`DELETE FROM repos WHERE id = $1`, repos[0])
	run(`SET CONSTRAINTS ALL IMMEDIATE`)
	if got := inTx(); got != before+1 {
		t.Fatalf("inside the savepoint counter = %d, want %d", got, before+1)
	}
	run(`ROLLBACK TO SAVEPOINT sp`)
	run(`SET CONSTRAINTS ALL DEFERRED`)
	if got := inTx(); got != before {
		t.Fatalf("after ROLLBACK TO SAVEPOINT counter = %d, want %d", got, before)
	}
	// Several deletes in one transaction (some firing immediately, some at commit) move it once.
	run(`DELETE FROM repos WHERE id = $1`, repos[1])
	run(`SET CONSTRAINTS ALL IMMEDIATE`)
	run(`DELETE FROM repos WHERE id = $1`, repos[2])
	run(`DELETE FROM repos WHERE id = $1`, repos[3])
	run(`SET CONSTRAINTS ALL DEFERRED`)
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.counter(t); got != before+1 {
		t.Fatalf("counter = %d, want exactly %d for one deleting transaction", got, before+1)
	}
	if n := e.docCount(t); n != 0 {
		t.Fatalf("%d docs left, want 0", n)
	}
}

// Mutation it must catch: a trigger that updates the state row once per deleted row (the per-row
// counter update whose COMMIT cost grows quadratically). The load-bearing check is that two
// thousand repos advance the counter exactly once; the time bound is only a generous sanity limit.
func TestAnalyticsPrecomputeBulkDeleteLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, conn := raSeedUserConn(e.ctx, t, e.pool, true)
	const n = 2000
	raProjectSeq++
	base := raProjectSeq
	raProjectSeq += n
	raExec(e.ctx, t, e.pool, `
		INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		SELECT gen_random_uuid(), $1, $2 + g, 'ap-bulk/' || g || '-' || gen_random_uuid()::text, 'https://forge.e2e/x', 'main', false
		FROM generate_series(1, $3::int) g`, conn, base, n)
	e.publish(t, apNow(), apDocs("A", "a"))
	before := e.counter(t)
	start := time.Now()
	tag, err := e.pool.Exec(e.ctx, `DELETE FROM repos WHERE connection_id = $1`, conn)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != n {
		t.Fatalf("deleted %d repos, want %d", tag.RowsAffected(), n)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("deleting %d repos took %v", n, d)
	}
	if got := e.counter(t); got != before+1 {
		t.Fatalf("counter = %d, want %d (one increment for the whole transaction)", got, before+1)
	}
	if c := e.docCount(t); c != 0 {
		t.Fatalf("%d docs left, want 0", c)
	}
}

// Mutation it must catch: the function without its pinned search_path. A session that creates TEMP
// tables named like the state and docs tables must not redirect the trigger's writes to them.
func TestAnalyticsPrecomputeInvalidationIgnoresTempShadowLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	_, _, repos := e.seedRepos(t, 1)
	e.publish(t, apNow(), apDocs("A", "a", "b"))
	before := e.counter(t)

	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The explicit DROP below removes the temp tables before the connection returns to the pool
	// (pgxpool does not reset sessions).
	defer func() {
		_, _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS pg_temp.analytics_precomputed_state, pg_temp.analytics_precomputed_docs`)
	}()
	exec(`CREATE TEMP TABLE analytics_precomputed_state (id boolean, invalidation bigint, publications bigint, failure_count int, last_failure_at timestamptz, invalidated_xact xid8)`)
	exec(`INSERT INTO pg_temp.analytics_precomputed_state VALUES (true, 0, 0, 0, NULL, NULL)`)
	exec(`CREATE TEMP TABLE analytics_precomputed_docs (section text, variant_key text, generated_at timestamptz, invalidation bigint, document bytea)`)
	exec(`BEGIN`)
	exec(`DELETE FROM repos WHERE id = $1`, repos[0])
	exec(`COMMIT`)

	if n := e.docCount(t); n != 0 {
		t.Fatalf("%d real docs left after a delete from a session shadowing the tables, want 0", n)
	}
	if got := e.counter(t); got != before+1 {
		t.Fatalf("real counter = %d, want %d", got, before+1)
	}
	var tempCounter int64
	if err := conn.QueryRow(e.ctx, `SELECT invalidation FROM pg_temp.analytics_precomputed_state`).Scan(&tempCounter); err != nil || tempCounter != 0 {
		t.Fatalf("temp shadow counter = %d (%v), want it untouched", tempCounter, err)
	}
}

// Mutation it must catch: a function body that reads a session setting, or a function left on the
// caller's search_path.
func TestAnalyticsPrecomputeFunctionCatalogShapeLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	var src string
	var config []string
	if err := e.pool.QueryRow(e.ctx, `SELECT prosrc, COALESCE(proconfig, '{}') FROM pg_proc WHERE proname = 'analytics_precomputed_invalidate' AND pronamespace = current_schema()::regnamespace`).Scan(&src, &config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "current_setting(") {
		t.Fatal("the invalidation function must not read a session setting")
	}
	found := false
	for _, c := range config {
		if strings.HasPrefix(c, "search_path=") {
			found = true
			if !strings.HasSuffix(c, "pg_temp") {
				t.Fatalf("function search_path = %q, want pg_temp last", c)
			}
		}
	}
	if !found {
		t.Fatalf("function proconfig = %v, want a pinned search_path", config)
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
	pubCtx, pubCancel := context.WithCancel(e.ctx)
	paused, resume := make(chan struct{}, 4), make(chan struct{})
	s.hooks.afterPublishInsert = func(i int) {
		if i == 0 {
			paused <- struct{}{}
			select {
			case <-resume:
			case <-pubCtx.Done():
			}
		}
	}
	pub := apGo(t, pubCancel, func() error {
		return s.Publish(pubCtx, apSection, counter, apNow(), apDocs("B", "b", "c"))
	})
	select {
	case <-paused:
	case err := <-pub:
		t.Fatalf("publish ended before its pause: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("publish never paused")
	}

	// The publisher holds the state row. The deleter commits: its deferred trigger waits on it.
	pubPID := int64(s.conn.PgConn().PID())
	dc, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dc.Release) // registered before the delete worker, so it runs after the worker exits
	var delPID int32
	if err := dc.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&delPID); err != nil {
		t.Fatal(err)
	}
	delCtx, delCancel := context.WithCancel(e.ctx)
	del := apGo(t, delCancel, func() error {
		_, err := dc.Exec(delCtx, `DELETE FROM repos WHERE id = $1`, repos[0])
		return err
	})
	e.waitLockWait(t, int64(delPID), pubPID, "the delete commit")
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

	// The rollbacks are cleanups registered before the workers below, so they run after the
	// workers have been cancelled and joined (a transaction is used by one goroutine at a time).
	a, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Rollback(context.Background()) })
	b, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Rollback(context.Background()) })

	var errs []error
	var mu sync.Mutex
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	var aPID, bPID int32
	if err := a.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&aPID); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&bPID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(e.ctx, `DELETE FROM repos WHERE id = $1`, r1); err != nil {
		t.Fatalf("A delete r1: %v", err)
	}
	bCtx, bCancel := context.WithCancel(e.ctx)
	bDel := apGo(t, bCancel, func() error { _, err := b.Exec(bCtx, `DELETE FROM repos WHERE id = $1`, r2); return err })
	// B's delete takes only the r2 row lock (its trigger is deferred), so it returns on its own.
	bDelErr := apWait(t, bDel, "B delete r2")
	aCtx, aCancel := context.WithCancel(e.ctx)
	aDel := apGo(t, aCancel, func() error { _, err := a.Exec(aCtx, `DELETE FROM repos WHERE id = $1`, r2); return err })
	// Barrier: A is waiting on B's r2 row lock before B commits.
	e.waitLockWait(t, int64(aPID), int64(bPID), "A delete r2")
	// B's delete must have returned before B's commit starts (a transaction is used by one
	// goroutine at a time). Without the deferral it is stuck here until the deadlock resolves.
	record := func(name string, err error) {
		if err != nil {
			note(err)
			t.Errorf("%s: %v (SQLSTATE %s)", name, err, apSQLState(err))
		}
	}
	record("B delete r2", bDelErr)
	bCommitCtx, bCommitCancel := context.WithCancel(e.ctx)
	bCommit := apGo(t, bCommitCancel, func() error { return b.Commit(bCommitCtx) })
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
// fail the open), or a failure of them that is treated as fatal. The hook must have run, else the
// test would not be exercising the failing TCP statement at all.
func TestAnalyticsPrecomputeTCPSettingsBestEffortLiveDB(t *testing.T) {
	e := apOpenEnv(t)
	calls := 0
	analyticsPrecomputeTCPSettingsFault = func() error { calls++; return errors.New("injected tcp failure") }
	s, ok, err := OpenAnalyticsPrecomputeSession(e.ctx, e.pool)
	analyticsPrecomputeTCPSettingsFault = nil
	if err != nil || !ok {
		t.Fatalf("open with a failing TCP statement = (%v, %v), want acquired", ok, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	if calls != 1 {
		t.Fatalf("the TCP settings fault hook ran %d times, want 1", calls)
	}
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
		faulted := false
		err := s.RunBuild(e.ctx, func(ctx context.Context, snap *AnalyticsSnapshot, _ int64) error {
			// Capture the socket now: Close clears s.conn, and the callback must never read it.
			raw := s.conn.PgConn().Conn()
			ran := make(chan struct{})
			timer := time.AfterFunc(200*time.Millisecond, func() {
				defer close(ran)
				_ = raw.Close()
			})
			_, err := snap.tx.Exec(ctx, `SELECT pg_sleep(30)`)
			if !timer.Stop() {
				<-ran // the fault fired: wait for its callback before the session is closed
				faulted = true
			}
			return err
		})
		if !faulted {
			t.Fatalf("the statement ended (%v) before the injected socket fault ran", err)
		}
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
