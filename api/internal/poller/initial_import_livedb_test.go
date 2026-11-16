package poller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The live-DB half of issue #4399: the poller owns a repo's first import and records
// it durably (waiting -> running -> succeeded|failed) on repos.initial_import_*.
// Everything here drives the real Engine against a real Postgres with a fake forge;
// the claim/finish SQL, the CHECK constraints and the generation guard only exist in
// the database.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package.

// importFake is the forge the poller's forgesvc builder returns. ListIssues serves the
// configured set for every fetch unless hook overrides it.
type importFake struct {
	forgetest.BaseFake
	mu     sync.Mutex
	issues []forge.Issue
	hook   func(ctx context.Context, project int64) ([]forge.Issue, error)
	calls  map[int64]int
}

func (f *importFake) ListIssues(ctx context.Context, project int64, _ forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.mu.Lock()
	hook, issues := f.hook, append([]forge.Issue(nil), f.issues...)
	f.calls[project]++
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, project)
	}
	return issues, nil
}

func (f *importFake) listCalls(project int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[project]
}

func (f *importFake) setHook(h func(ctx context.Context, project int64) ([]forge.Issue, error)) {
	f.mu.Lock()
	f.hook = h
	f.mu.Unlock()
}

func importIssues(iids ...int64) []forge.Issue {
	out := make([]forge.Issue, 0, len(iids))
	for _, iid := range iids {
		out = append(out, forge.Issue{
			IID: iid, Title: fmt.Sprintf("issue %d", iid), State: "opened", Labels: []string{"uzi"},
			WebURL: fmt.Sprintf("https://forge.test/g/r/-/issues/%d", iid), UpdatedAt: time.Now().UTC().Truncate(time.Second),
		})
	}
	return out
}

// onlyRepos narrows the poller's enabled set to this test's repos: the database is
// shared with other tests' enabled repos, whose connections this fake must not serve.
type onlyRepos struct {
	q   *store.Queries
	mu  sync.Mutex
	ids map[uuid.UUID]struct{}
}

func (o *onlyRepos) add(id uuid.UUID) {
	o.mu.Lock()
	o.ids[id] = struct{}{}
	o.mu.Unlock()
}

func (o *onlyRepos) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := o.q.ListEnabledReposWithConnections(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []store.ListEnabledReposWithConnectionsRow
	for _, r := range rows {
		if _, ok := o.ids[r.ID]; ok {
			out = append(out, r)
		}
	}
	return out, err
}

type importEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	svc    *forgesvc.Service
	fake   *importFake
	repos  *onlyRepos
	user   uuid.UUID
	conn   uuid.UUID
	nextID int64
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	e := &importEnv{
		t: t, ctx: ctx, pool: pool, q: q, user: uuid.New(), conn: uuid.New(),
		fake:   &importFake{calls: map[int64]int{}, issues: importIssues(1, 2, 3)},
		repos:  &onlyRepos{q: q, ids: map[uuid.UUID]struct{}{}},
		nextID: 1000,
	}
	e.svc = forgesvc.NewWithForgeBuilder(q, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
		return e.fake, nil
	})
	sealed, err := e.svc.EncryptToken("fresh fixture")
	if err != nil {
		t.Fatal(err)
	}
	e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", e.user, fmt.Sprintf("%s@import.test", e.user))
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.test','bot',1,$3)", e.conn, e.user, sealed)
	// Disable this test's repos on the way out so a later test's unfiltered poller
	// (or the API's) never sees them.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "UPDATE repos SET enabled=false WHERE connection_id IN (SELECT id FROM forge_connections WHERE user_id=$1)", e.user)
	})
	return e
}

func (e *importEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// addRepo inserts an enabled repo on the test connection. state "" is the legacy NULL
// state; a failed state needs its class (the pairing CHECK).
func (e *importEnv) addRepo(state string, gen int64, class string) (uuid.UUID, int64) {
	e.t.Helper()
	e.nextID++
	return e.addRepoOn(e.conn, state, gen, class, e.nextID)
}

func (e *importEnv) addRepoOn(conn uuid.UUID, state string, gen int64, class string, project int64) (uuid.UUID, int64) {
	e.t.Helper()
	id := uuid.New()
	var st, cl, at any
	if state != "" {
		st = state
		at = time.Now().Add(-time.Hour)
	}
	if class != "" {
		cl = class
	}
	e.exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled,
	          initial_import_state,initial_import_generation,initial_import_error_class,initial_import_updated_at)
	        VALUES($1,$2,$3,$4,'https://forge.test/g/r',true,$5,$6,$7,$8)`,
		id, conn, project, id.String(), st, gen, cl, at)
	e.repos.add(id)
	return id, project
}

type importRow struct {
	state, class string // "" is NULL
	gen          int64
	updatedAt    time.Time // zero is NULL
}

func (e *importEnv) row(id uuid.UUID) importRow {
	e.t.Helper()
	var st, cl *string
	var at *time.Time
	var gen int64
	if err := e.pool.QueryRow(e.ctx, `SELECT initial_import_state, initial_import_generation, initial_import_error_class, initial_import_updated_at FROM repos WHERE id=$1`, id).Scan(&st, &gen, &cl, &at); err != nil {
		e.t.Fatal(err)
	}
	r := importRow{gen: gen}
	if st != nil {
		r.state = *st
	}
	if cl != nil {
		r.class = *cl
	}
	if at != nil {
		r.updatedAt = *at
	}
	return r
}

func (e *importEnv) cachedIIDs(id uuid.UUID) []int64 {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT forge_issue_iid FROM issues WHERE repo_id=$1`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var iid int64
		if err := rows.Scan(&iid); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, iid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (e *importEnv) engine(interval, forgeTimeout time.Duration) *Engine {
	en := New(e.svc, e.repos, interval, 10)
	en.SetForgeTimeout(forgeTimeout)
	en.SetImportStateStore(e.q)
	return en
}

func (e *importEnv) waitRow(id uuid.UUID, state string) importRow {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := e.row(id)
		if r.state == state {
			return r
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("repo %s state = %q, want %q within 15s", id, r.state, state)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func wantIIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cached issue iids = %v, want %v", got, want)
	}
}

// A legacy repo (NULL state) is never claimed: the poller still syncs it as before but
// the row never gains an import status.
func TestInitialImportLegacyRepoNeverClaimedLiveDB(t *testing.T) {
	e := newImportEnv(t)
	id, _ := e.addRepo("", 0, "")

	e.engine(time.Minute, 0).PollOnce(e.ctx)

	if r := e.row(id); r.state != "" || r.class != "" || r.gen != 0 || !r.updatedAt.IsZero() {
		t.Fatalf("legacy row after a poll = %+v, want untouched NULL state", r)
	}
	wantIIDs(t, e.cachedIIDs(id), 1, 2, 3) // the ordinary sync still ran
}

// waiting -> running -> succeeded, with the running state observable mid-import and a
// NULL class on success.
func TestInitialImportWaitingToSucceededLiveDB(t *testing.T) {
	e := newImportEnv(t)
	id, _ := e.addRepo("waiting", 4, "")

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.fake.setHook(func(ctx context.Context, _ int64) ([]forge.Issue, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return importIssues(1, 2, 3), nil
	})

	done := make(chan struct{})
	go func() { defer close(done); e.engine(time.Minute, 0).PollOnce(e.ctx) }()
	<-entered
	if r := e.row(id); r.state != "running" || r.class != "" || r.gen != 4 {
		t.Fatalf("mid-import row = %+v, want running at generation 4", r)
	}
	close(release)
	<-done

	r := e.row(id)
	if r.state != "succeeded" || r.class != "" || r.gen != 4 {
		t.Fatalf("final row = %+v, want succeeded, NULL class, generation unchanged", r)
	}
	wantIIDs(t, e.cachedIIDs(id), 1, 2, 3)
}

// A deadline during the import records failed/timeout even though the tick context is
// already dead: the settle write is detached from it.
func TestInitialImportTickDeadlineRecordsTimeoutLiveDB(t *testing.T) {
	e := newImportEnv(t)
	id, _ := e.addRepo("waiting", 1, "")
	e.fake.setHook(func(ctx context.Context, _ int64) ([]forge.Issue, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	e.engine(200*time.Millisecond, 0).PollOnce(e.ctx)

	if r := e.row(id); r.state != "failed" || r.class != "timeout" {
		t.Fatalf("row after a deadline = %+v, want failed/timeout", r)
	}
	if got := e.cachedIIDs(id); len(got) != 0 {
		t.Fatalf("a failed import cached %v, want nothing", got)
	}
}

// Classified forge failures keep their class; an unclassified one is other; a panic in
// the forge still leaves a settled row; and a failed row retries to succeeded on the
// next pass (failed -> running -> succeeded passes every CHECK).
func TestInitialImportFailureClassesAndRetryLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hook  func(context.Context, int64) ([]forge.Issue, error)
		class string
	}{
		{"server error", func(context.Context, int64) ([]forge.Issue, error) {
			return nil, classedErr{forge.ErrorClassServerError}
		}, "server_error"},
		{"auth", func(context.Context, int64) ([]forge.Issue, error) {
			return nil, classedErr{forge.ErrorClassAuth}
		}, "auth"},
		{"rate limited", func(context.Context, int64) ([]forge.Issue, error) {
			return nil, classedErr{forge.ErrorClassRateLimited}
		}, "rate_limited"},
		{"unclassified", func(context.Context, int64) ([]forge.Issue, error) {
			return nil, fmt.Errorf("boom")
		}, "other"},
		{"panic", func(context.Context, int64) ([]forge.Issue, error) {
			panic("hostile forge response")
		}, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			id, _ := e.addRepo("waiting", 2, "")
			e.fake.setHook(tc.hook)
			en := e.engine(time.Minute, 0)

			en.PollOnce(e.ctx)
			if r := e.row(id); r.state != "failed" || r.class != tc.class || r.gen != 2 {
				t.Fatalf("row after failure = %+v, want failed/%s at generation 2", r, tc.class)
			}

			// The next regular pass retries a failed import and succeeds.
			e.fake.setHook(nil)
			en.PollOnce(e.ctx)
			if r := e.row(id); r.state != "succeeded" || r.class != "" || r.gen != 2 {
				t.Fatalf("row after retry = %+v, want succeeded with NULL class", r)
			}
			wantIIDs(t, e.cachedIIDs(id), 1, 2, 3)
		})
	}
}

// A forge client that cannot be built (undecryptable token) is recorded as failed,
// not left waiting or running.
func TestInitialImportForgeClientFailureRecordsFailedLiveDB(t *testing.T) {
	e := newImportEnv(t)
	badConn := uuid.New()
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge-bad.test','bot',1,$3)", badConn, e.user, []byte("not a sealed token"))
	id, project := e.addRepoOn(badConn, "waiting", 1, "", 9001)

	e.engine(time.Minute, 0).PollOnce(e.ctx)

	if r := e.row(id); r.state != "failed" || r.class != "other" {
		t.Fatalf("row = %+v, want failed/other", r)
	}
	if n := e.fake.listCalls(project); n != 0 {
		t.Fatalf("forge listed %d times for a repo whose client never built", n)
	}
}

// A generation that moves while the import is in flight (disable then re-enable): the
// in-flight import's finish writes zero rows, the newer waiting import is untouched,
// and the next pass imports it at its own generation.
func TestInitialImportStaleFinishWritesNothingLiveDB(t *testing.T) {
	e := newImportEnv(t)
	id, _ := e.addRepo("waiting", 1, "")

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.fake.setHook(func(ctx context.Context, _ int64) ([]forge.Issue, error) {
		once.Do(func() { close(entered) })
		<-release
		return importIssues(1, 2, 3), nil
	})
	en := e.engine(time.Minute, 0)
	done := make(chan struct{})
	go func() { defer close(done); en.PollOnce(e.ctx) }()
	<-entered
	e.exec(`UPDATE repos SET initial_import_generation = initial_import_generation + 1, initial_import_state='waiting',
	          initial_import_error_class=NULL, initial_import_updated_at=now() WHERE id=$1`, id)
	close(release)
	<-done

	if r := e.row(id); r.state != "waiting" || r.gen != 2 {
		t.Fatalf("row after a stale finish = %+v, want the newer waiting import at generation 2 untouched", r)
	}

	e.fake.setHook(nil)
	en.PollOnce(e.ctx)
	if r := e.row(id); r.state != "succeeded" || r.gen != 2 {
		t.Fatalf("row after the next pass = %+v, want succeeded at generation 2", r)
	}
}

// Crash recovery: a fresh Engine (empty in-memory state, as after a restart) finishes
// imports left waiting or running, at the same generation, and the full sync evicts
// cache rows the forge no longer has.
func TestInitialImportRecoversAfterRestartLiveDB(t *testing.T) {
	e := newImportEnv(t)
	waiting, _ := e.addRepo("waiting", 3, "")
	running, _ := e.addRepo("running", 5, "")
	partial, _ := e.addRepo("running", 8, "")
	for _, iid := range []int64{1, 99} { // 1 is on the forge, 99 was deleted forge-side
		e.exec(`INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at)
		        VALUES($1,$2,'old','opened','["uzi"]','https://forge.test/x',now(),now())`, partial, iid)
	}
	before := map[uuid.UUID]importRow{waiting: e.row(waiting), running: e.row(running), partial: e.row(partial)}

	fresh := e.engine(time.Minute, 0)
	fresh.PollOnce(e.ctx)

	for id, b := range before {
		r := e.row(id)
		if r.state != "succeeded" || r.class != "" || r.gen != b.gen {
			t.Errorf("repo %s row = %+v, want succeeded at generation %d", id, r, b.gen)
		}
		if !r.updatedAt.After(b.updatedAt) {
			t.Errorf("repo %s updated_at %v did not advance past %v", id, r.updatedAt, b.updatedAt)
		}
		wantIIDs(t, e.cachedIIDs(id), 1, 2, 3)
	}
}

// RequestImport reaches the import through Run's wake case, with the ticker far away.
func TestInitialImportRunWakeImportsRequestedRepoLiveDB(t *testing.T) {
	e := newImportEnv(t)
	id, _ := e.addRepo("waiting", 1, "")
	en := e.engine(time.Hour, 0)

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); en.Run(ctx) }()
	defer func() { cancel(); <-done }()

	en.RequestImport(id)
	r := e.waitRow(id, "succeeded")
	if r.gen != 1 {
		t.Fatalf("row = %+v, want generation 1", r)
	}
	wantIIDs(t, e.cachedIIDs(id), 1, 2, 3)
}

// The wake pass syncs only repos that were requested AND still owe an import, and it
// never prunes: another enabled repo's in-memory state survives it.
func TestInitialImportWakePassIsScopedAndKeepsOtherStateLiveDB(t *testing.T) {
	e := newImportEnv(t)
	requested, requestedProject := e.addRepo("waiting", 1, "")
	other, otherProject := e.addRepo("waiting", 1, "")                // enabled, owed, NOT requested
	requestedDone, doneProject := e.addRepo("succeeded", 1, "")       // requested but nothing owed
	requestedLegacy, legacyProject := e.addRepo("", 0, "")            // requested but legacy
	requestedFailed, failedProject := e.addRepo("failed", 1, "other") // requested; failed waits for a tick

	en := e.engine(time.Hour, 0)
	// Give the unrequested repo in-memory state, as a prior tick would have.
	en.states[other] = &repoState{pollCount: 7}

	for _, id := range []uuid.UUID{requested, requestedDone, requestedLegacy, requestedFailed} {
		en.RequestImport(id)
	}
	en.importRequested(e.ctx)

	if r := e.row(requested); r.state != "succeeded" {
		t.Fatalf("requested repo row = %+v, want succeeded", r)
	}
	if r := e.row(other); r.state != "waiting" {
		t.Fatalf("unrequested repo row = %+v, want still waiting", r)
	}
	if st := en.states[other]; st == nil || st.pollCount != 7 {
		t.Fatalf("unrequested repo's state = %+v, want it intact (a wake pass must not prune)", st)
	}
	if en.states[requested] == nil {
		t.Fatal("requested repo has no state entry after its pass")
	}
	if n := e.fake.listCalls(requestedProject); n == 0 {
		t.Fatal("requested repo never reached the forge")
	}
	for name, project := range map[string]int64{"unrequested": otherProject, "succeeded": doneProject, "legacy": legacyProject, "failed": failedProject} {
		if n := e.fake.listCalls(project); n != 0 {
			t.Errorf("%s repo listed %d times by the wake pass, want 0", name, n)
		}
	}
	if r := e.row(requestedFailed); r.state != "failed" {
		t.Fatalf("failed repo row = %+v, want left for the regular tick", r)
	}
	// The request set was drained: a second wake does nothing.
	before := e.fake.listCalls(requestedProject)
	en.importRequested(e.ctx)
	if e.fake.listCalls(requestedProject) != before {
		t.Fatal("a second wake re-synced a repo that was no longer requested")
	}
}
