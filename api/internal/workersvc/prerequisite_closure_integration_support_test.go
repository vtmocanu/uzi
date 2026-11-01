package workersvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These adapters observe executor-owned real transactions only. Recovery API
// transactions may run concurrently. active stays true through Commit/Rollback.
type m3Transactions struct {
	f                 *prerequisiteExecutorLiveDB
	active            atomic.Bool
	committed         atomic.Bool
	orderViolation    atomic.Bool
	count             atomic.Int32
	s2PID             atomic.Int32
	afterMarkerCommit func()
}
type m3TxQueries struct {
	PrerequisiteStore
	owner     *m3Transactions
	second    bool
	runLocked bool
	marked    bool
}

func (q *m3TxQueries) LockPrerequisiteRun(ctx context.Context, id uuid.UUID) (store.Run, error) {
	r, e := q.PrerequisiteStore.LockPrerequisiteRun(ctx, id)
	if e == nil {
		q.runLocked = true
	}
	return r, e
}
func (q *m3TxQueries) LockPrerequisiteHold(ctx context.Context, id uuid.UUID) (store.RecoveryCustodyHold, error) {
	if q.second && !q.runLocked {
		q.owner.orderViolation.Store(true)
		return store.RecoveryCustodyHold{}, errors.New("S2 submitted hold before acquiring run")
	}
	return q.PrerequisiteStore.LockPrerequisiteHold(ctx, id)
}
func (q *m3TxQueries) MarkPrerequisiteInvoked(ctx context.Context, a store.MarkPrerequisiteInvokedParams) (store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.MarkPrerequisiteInvoked(ctx, a)
	q.marked = e == nil
	return r, e
}
func (o *m3Transactions) transaction(ctx context.Context, fn func(PrerequisiteStore) error) error {
	tx, e := o.f.env.pool.Begin(ctx)
	if e != nil {
		return e
	}
	o.active.Store(true)
	q := &m3TxQueries{PrerequisiteStore: store.New(tx), owner: o, second: o.count.Add(1) == 2}
	if q.second {
		o.s2PID.Store(int32(tx.Conn().PgConn().PID())) //nolint:gosec // PostgreSQL PID fits int32.
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(c)
		o.active.Store(false)
	}()
	if e = fn(q); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	if q.marked {
		o.committed.Store(true)
		if o.afterMarkerCommit != nil {
			o.afterMarkerCommit()
		}
	}
	return nil
}

type m3Builder struct {
	build func(string) (forge.Forge, error)
}

func (b *m3Builder) ForgeForConnection(_ string, baseURL string, _ []byte) (forge.Forge, error) {
	return b.build(baseURL)
}

// Only seeded fixture objects supply metadata. Keep pointers so explicit remote
// rebinding before execution is reflected, while preserving custom ancestry stubs.
func (f *prerequisiteExecutorLiveDB) fixtureForge(t *testing.T, baseURL string, driver forge.Forge) (forge.Forge, error) {
	t.Helper()
	original, ok := driver.(*prerequisiteTestForge)
	if !ok {
		return nil, fmt.Errorf("unsupported LiveDB fixture forge %T", driver)
	}
	clone := *original
	clone.getProject = func(ctx context.Context, id int64) (forge.Project, error) {
		f.fixturesMu.RLock()
		defer f.fixturesMu.RUnlock()
		var selected *prerequisiteMemory
		for _, m := range f.fixtures {
			if m.conn.BaseUrl == baseURL && m.repo.ForgeProjectID == id {
				if selected != nil {
					return forge.Project{}, fmt.Errorf("ambiguous fixture project %s/%d", baseURL, id)
				}
				selected = m
			}
		}
		if selected == nil {
			return forge.Project{}, fmt.Errorf("unknown fixture project %s/%d", baseURL, id)
		}
		if selected.inTX.Load() {
			t.Error("project lookup in transaction")
		}
		u, err := url.Parse(selected.repo.WebUrl)
		if err != nil {
			return forge.Project{}, err
		}
		return forge.Project{ForgeProjectID: id, PathWithNamespace: strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), WebURL: selected.repo.WebUrl}, ctx.Err()
	}
	return &clone, nil
}

func m3Context(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func m3Wait(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type m3Result[T any] struct {
	value T
	err   error
}

func m3Start[T any](t *testing.T, ctx context.Context, fn func(context.Context) (T, error)) <-chan m3Result[T] {
	t.Helper()
	child, cancel := context.WithCancel(ctx)
	result := make(chan m3Result[T], 1)
	done := make(chan struct{})
	go func() { defer close(done); v, e := fn(child); result <- m3Result[T]{v, e} }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned operation did not join after cancellation")
		}
	})
	return result
}
func m3Join[T any](t *testing.T, ctx context.Context, ch <-chan m3Result[T]) T {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}
func m3Sweep(t *testing.T, ctx context.Context, s *Service) {
	t.Helper()
	if _, e := s.SweepPrerequisiteClosures(ctx); e != nil {
		t.Fatal(e)
	}
}
func m3Fresh(f *prerequisiteExecutorLiveDB) *Service {
	s := New(f.env.q, f.s.box, testParams())
	s.SetTxBeginner(f.env.pool)
	s.SetClock(f.s.now)
	s.SetForgeBaseURLAllowed(func(string) bool { return true })
	s.SetForges(f.s.forges)
	return s
}
func m3Read(t *testing.T, f *prerequisiteExecutorLiveDB, id uuid.UUID) store.RecoveryPrerequisiteClosure {
	t.Helper()
	r, e := f.env.q.GetPrerequisiteClosure(f.env.ctx, store.GetPrerequisiteClosureParams{CaptureID: id, UserID: f.m.row.UserID})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// This loopback broker records actual command packets. The two cancellation
// barriers run outside mu, and use the HTTP request's own cancellation.
type m3Remote struct {
	mu                 sync.Mutex
	tip                string
	defaultTip         string
	mode               string
	requests           int
	posts              atomic.Int32
	root               context.Context
	canceledBeforeRoot atomic.Bool
	commands           [][3]string
	tx                 *m3Transactions
	ioInTX             atomic.Bool
	sendBeforeCommit   atomic.Bool
	block              string
	entered            chan struct{}
	exited             chan struct{}
	canceled           chan struct{}
	abort              chan struct{}
	once               sync.Once
	ref                string
}

func m3RemoteFor(t *testing.T, f *prerequisiteExecutorLiveDB, tx *m3Transactions) *m3Remote {
	t.Helper()
	r := &m3Remote{defaultTip: prerequisiteTestTip, ref: f.m.row.PinRef, tx: tx, abort: make(chan struct{}), canceled: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(func() { close(r.abort); server.Close() })
	if tx != nil {
		f.h.transaction = &tx.active
	}
	f.env.exec("UPDATE forge_connections SET base_url=$1 WHERE id=$2", server.URL, f.m.conn.ID)
	f.env.exec("UPDATE repos SET web_url=$1 WHERE id=$2", server.URL+"/repo", f.m.row.RepoID)
	f.m.conn.BaseUrl = server.URL
	f.m.repo.WebUrl = server.URL + "/repo"
	return r
}
func (r *m3Remote) serve(w http.ResponseWriter, req *http.Request) {
	if r.tx != nil && r.tx.active.Load() {
		r.ioInTX.Store(true)
	}
	r.mu.Lock()
	r.requests++
	isPost := req.Method == http.MethodPost
	if isPost {
		r.posts.Add(1)
		if r.tx != nil && !r.tx.committed.Load() {
			r.sendBeforeCommit.Store(true)
		}
	}
	shouldBlock := r.block == "send" && isPost || r.block == "readback" && !isPost && len(r.commands) > 0
	entered, exited := r.entered, r.exited
	r.mu.Unlock()
	if shouldBlock {
		defer close(exited)
		// Parse the actual create command before the cancellation barrier.
		if isPost {
			scan := pktline.NewScanner(req.Body)
			if !scan.Scan() {
				http.Error(w, "missing command", 400)
				return
			}
			fields := strings.Fields(strings.SplitN(string(scan.Bytes()), "\x00", 2)[0])
			if len(fields) != 3 {
				http.Error(w, "bad command", 400)
				return
			}
			r.mu.Lock()
			r.commands = append(r.commands, [3]string{fields[0], fields[1], fields[2]})
			r.mu.Unlock()
			_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
		}
		r.once.Do(func() { close(entered) })
		select {
		case <-req.Context().Done():
			if r.root == nil || !errors.Is(r.root.Err(), context.Canceled) {
				r.canceledBeforeRoot.Store(true)
			}
			close(r.canceled)
		case <-r.abort:
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	enc := pktline.NewEncoder(w)
	if isPost {
		scan := pktline.NewScanner(req.Body)
		if !scan.Scan() {
			http.Error(w, "missing command", 400)
			return
		}
		fields := strings.Fields(strings.SplitN(string(scan.Bytes()), "\x00", 2)[0])
		if len(fields) != 3 {
			http.Error(w, "bad command", 400)
			return
		}
		command := [3]string{fields[0], fields[1], fields[2]}
		r.commands = append(r.commands, command)
		if !strings.HasPrefix(r.mode, "refuse") && r.mode != "ambiguous-absent" {
			r.tip = command[1]
			if r.tip == strings.Repeat("0", 40) {
				r.tip = ""
			}
		}
		if strings.HasPrefix(r.mode, "ambiguous") {
			http.Error(w, "ambiguous result", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_ = enc.EncodeString("unpack ok\n")
		status := "ok " + r.ref
		if strings.HasPrefix(r.mode, "refuse") {
			status = "ng " + r.ref + " synthetic policy refusal"
		}
		_ = enc.EncodeString(status + "\n")
		_ = enc.Flush()
		return
	}
	if (r.mode == "ambiguous-readback" || r.mode == "refuse-readback") && len(r.commands) > 0 ||
		r.mode == "ambiguous-cleanup-readback" && len(r.commands) > 1 {
		http.Error(w, "readback unavailable", http.StatusServiceUnavailable)
		return
	}
	svc := req.URL.Query().Get("service")
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	_ = enc.EncodeString("# service=" + svc + "\n")
	_ = enc.Flush()
	_ = enc.EncodeString(r.defaultTip + " refs/heads/main\x00report-status delete-refs\n")
	if r.tip != "" {
		_ = enc.EncodeString(r.tip + " " + r.ref + "\n")
	}
	_ = enc.Flush()
}
func (r *m3Remote) snapshot() (int, [][3]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, append([][3]string(nil), r.commands...)
}
func m3AssertFrozen(t *testing.T, before, after store.RecoveryPrerequisiteClosure) {
	t.Helper()
	if before.ID != after.ID || before.CaptureID != after.CaptureID || before.HoldID != after.HoldID ||
		before.RunID != after.RunID || before.UserID != after.UserID || before.RepoID != after.RepoID ||
		before.InvokedAt != after.InvokedAt || before.InvokedDeadline != after.InvokedDeadline ||
		before.Checksum != after.Checksum || !slices.Equal(before.PrerequisiteShas, after.PrerequisiteShas) ||
		before.ConnectionID != after.ConnectionID || before.ForgeKind != after.ForgeKind ||
		before.NormalizedBaseUrl != after.NormalizedBaseUrl || before.ProjectID != after.ProjectID ||
		before.DefaultBranch != after.DefaultBranch || before.PinTip != after.PinTip ||
		before.PinRef != after.PinRef || before.LiveRunID != after.LiveRunID || before.Attempts != after.Attempts {
		t.Fatal("frozen binding, live pointer, or create generation changed")
	}
}

// Connect before releasing the builder: S2 keeps its real 500ms deadline.
func m3Observer(t *testing.T, ctx context.Context, f *prerequisiteExecutorLiveDB) *pgx.Conn {
	t.Helper()
	c, e := pgx.ConnectConfig(ctx, f.env.pool.Config().ConnConfig.Copy())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.Close(cctx)
	})
	return c
}
func m3LockWait(t *testing.T, ctx context.Context, c *pgx.Conn, blocker int32, query string) int32 {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var pid int32
		e := c.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database()
   AND wait_event_type='Lock' AND strpos(query,$2)>0
   AND cardinality(pg_blocking_pids(pid))>0
   AND ($1::int=0 OR $1=ANY(pg_blocking_pids(pid))) LIMIT 1`, blocker, query).Scan(&pid)
		if e == nil {
			return pid
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			t.Fatal(e)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("missing PostgreSQL wait %q behind %d: %v", query, blocker, ctx.Err())
		}
	}
}
