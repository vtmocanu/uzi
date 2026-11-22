package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPMCreateOutcome pins how pmCreateOutcome decides database ownership after a
// CREATE DATABASE attempt.
func TestPMCreateOutcome(t *testing.T) {
	pg := func(unlocalized string) error {
		return &pgconn.PgError{Severity: "ERREUR", SeverityUnlocalized: unlocalized, Code: "42P04"}
	}
	exec := func(err error) error { return &pmAdminErr{Stage: pmStageExec, Err: err} }
	cases := []struct {
		name                string
		err                 error
		wantCreated, wantAm bool
	}{
		{"nil", nil, true, false},
		{"connect stage", &pmAdminErr{Stage: pmStageConnect, Err: errors.New("refused")}, false, false},
		{"marker stage", &pmAdminErr{Stage: pmStageMarker, Err: errors.New("no marker")}, false, false},
		{"exec ERROR", exec(pg("ERROR")), false, false},
		{"exec FATAL", exec(pg("FATAL")), false, true},
		{"exec PANIC", exec(pg("PANIC")), false, true},
		{"exec plain", exec(errors.New("conn reset")), false, true},
		{"exec context canceled", exec(context.Canceled), false, true},
		{"unwrapped injected error", errors.New("simulated"), false, true},
		{"unwrapped PgError ERROR", pg("ERROR"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, amb := pmCreateOutcome(tc.err)
			if created != tc.wantCreated || amb != tc.wantAm {
				t.Fatalf("pmCreateOutcome = (created %v, ambiguous %v), want (%v, %v)", created, amb, tc.wantCreated, tc.wantAm)
			}
		})
	}
}

// pmGuardEnv builds the smallest pmEnv the sampler and the pool config need: a throwaway output
// file, an empty pid table and a candidate. It opens no connection.
func pmGuardEnv(t *testing.T, dsn string, cand pmCand) *pmEnv {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out.jsonl"))
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return &pmEnv{t: t, out: &pmOut{f: f}, dsn: dsn, cand: cand, start: time.Now(), pids: map[uint32]*pmPID{}}
}

// TestPMWithSamplerJoinsOnGoexit proves that a runtime.Goexit in the body (what t.Fatalf does)
// cancels the sampler's read context and joins its goroutine before the caller's earlier
// defers run.
func TestPMWithSamplerJoinsOnGoexit(t *testing.T) {
	e := pmGuardEnv(t, "", pmCand{Name: "T"})
	e.curPID.Store(4242)
	entered := make(chan struct{}, 1)
	var mu sync.Mutex
	var readErr error
	read := func(ctx context.Context, _ uint32) (pmMem, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		mu.Lock()
		readErr = ctx.Err()
		mu.Unlock()
		return pmMem{}, ctx.Err()
	}
	var s *pmSampler
	var cleanupRan, doneClosed bool
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() {
			cleanupRan = true
			select {
			case <-s.done:
				doneClosed = true
			default:
			}
		}()
		pmWithSampler(func() *pmSampler {
			s = newPMSampler(context.Background(), e, read, 5*time.Millisecond)
			return s
		}, func(*pmSampler) {
			<-entered
			runtime.Goexit()
		})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("pmWithSampler did not cancel and join the sampler after Goexit")
	}
	if !cleanupRan {
		t.Fatal("the caller's deferred cleanup did not run")
	}
	if !doneClosed {
		t.Error("the sampler goroutine was still running when the caller's earlier defer ran")
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(readErr, context.Canceled) {
		t.Errorf("blocked read saw %v, want context.Canceled", readErr)
	}
}

// TestPMSamplerStopKeepsHealthyRead proves Stop is graceful (healthy reads never see a
// cancelled context), escalates to cancellation only after joinWait, and caches its result.
func TestPMSamplerStopKeepsHealthyRead(t *testing.T) {
	t.Run("healthy reads", func(t *testing.T) {
		e := pmGuardEnv(t, "", pmCand{Name: "T"})
		e.curPID.Store(4242)
		var calls, cancelled atomic.Int64
		read := func(ctx context.Context, _ uint32) (pmMem, error) {
			calls.Add(1)
			if ctx.Err() != nil {
				cancelled.Add(1)
			}
			return pmMem{PrivateKB: 100, AnonymousKB: 50}, nil
		}
		s := newPMSampler(context.Background(), e, read, 5*time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for calls.Load() < 3 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		first := s.Stop()
		if first.N == 0 {
			t.Fatalf("Stop result has no samples: %+v", first)
		}
		if first.Errors != 0 {
			t.Errorf("Stop result has %d errors (first: %s)", first.Errors, first.FirstError)
		}
		if n := cancelled.Load(); n != 0 {
			t.Errorf("%d reads saw a cancelled context before or during a graceful Stop", n)
		}
		if again := s.Stop(); again != first {
			t.Errorf("second Stop = %+v, want %+v", again, first)
		}
		if closed := s.Close(); closed != first {
			t.Errorf("Close after Stop = %+v, want %+v", closed, first)
		}
	})
	t.Run("blocked read escalates", func(t *testing.T) {
		e := pmGuardEnv(t, "", pmCand{Name: "T"})
		e.curPID.Store(4242)
		entered := make(chan struct{}, 1)
		read := func(ctx context.Context, _ uint32) (pmMem, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return pmMem{}, ctx.Err()
		}
		s := newPMSampler(context.Background(), e, read, 5*time.Millisecond)
		s.joinWait = 50 * time.Millisecond
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the read never started")
		}
		got := make(chan pmSamplerResult, 1)
		go func() { got <- s.Stop() }()
		var first pmSamplerResult
		select {
		case first = <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not escalate to cancelling a blocked read")
		}
		if first.Errors != 0 {
			t.Errorf("a read cancelled by Stop counted as %d errors (first: %s)", first.Errors, first.FirstError)
		}
		if first.Dropped == 0 {
			t.Errorf("a read cancelled by Stop was not counted as dropped: %+v", first)
		}
		if again := s.Stop(); again != first {
			t.Errorf("second Stop = %+v, want %+v", again, first)
		}
		if closed := s.Close(); closed != first {
			t.Errorf("Close after Stop = %+v, want %+v", closed, first)
		}
	})
}

func TestPMRoundLoop(t *testing.T) {
	const step = 10 * time.Second
	cases := []struct {
		name       string
		minRounds  int
		recycle    bool
		maxWait    time.Duration
		pids       func(r int) int
		wantRounds int
		wantErr    string
	}{
		{"non-recycle stops at minRounds", 3, false, time.Nanosecond, func(int) int { return 1 }, 3, ""},
		{"recycle with four recycles", 2, true, time.Hour, func(r int) int { return r + 1 }, 4, ""},
		{"recycle waits for minRounds", 6, true, time.Hour, func(r int) int { return r + 1 }, 6, ""},
		{"deadline before minRounds errors", 5, true, 15 * time.Second, func(int) int { return 1 }, 2, "2 of 5"},
		{"deadline after minRounds with three recycles stops", 2, true, 15 * time.Second, func(int) int { return 4 }, 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := 0
			rounds, err := pmRoundLoop(tc.minRounds, tc.recycle, tc.maxWait,
				func() time.Duration { return time.Duration(cur) * step },
				func(r int) int {
					cur = r
					return tc.pids(r)
				})
			if rounds != tc.wantRounds {
				t.Errorf("rounds = %d, want %d", rounds, tc.wantRounds)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "rounds") {
				t.Fatalf("error = %v, want it to mention %q rounds", err, tc.wantErr)
			}
		})
	}
}

func TestPMPoolConfig(t *testing.T) {
	const base = "postgres://localhost:5432/x?sslmode=disable"
	candA := pmCand{Name: "A"}
	candB := pmCand{Name: "B128", StmtCap: 128}
	cases := []struct {
		name    string
		dsn     string
		cand    pmCand
		wantErr bool
	}{
		{"plain A", base, candA, false},
		{"plain B128", base, candB, false},
		{"exec mode", base + "&default_query_exec_mode=exec", candA, true},
		{"simple_protocol mode", base + "&default_query_exec_mode=simple_protocol", candA, true},
		{"describe_exec mode", base + "&default_query_exec_mode=describe_exec", candA, true},
		{"DSN capacity under A", base + "&statement_cache_capacity=64", candA, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := pmGuardEnv(t, tc.dsn, tc.cand).poolConfig()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "would not measure the statement cache") {
					t.Fatalf("poolConfig error = %v, want a would-not-measure error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("poolConfig: %v", err)
			}
			if got := cfg.ConnConfig.StatementCacheCapacity; got != tc.cand.capacity() {
				t.Errorf("capacity = %d, want %d", got, tc.cand.capacity())
			}
		})
	}
}

func TestPMCheckPlanCacheMode(t *testing.T) {
	if err := pmCheckPlanCacheMode("auto"); err != nil {
		t.Errorf("auto rejected: %v", err)
	}
	for _, m := range []string{"force_generic_plan", "force_custom_plan"} {
		if err := pmCheckPlanCacheMode(m); err == nil {
			t.Errorf("%s accepted", m)
		}
	}
}

func pmGuardPrefix() string {
	return "pm_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

// TestPoolMemoryHarnessDDLGuardsLiveDB proves against a real throwaway server that the harness
// DDL is marker-guarded and that clone creation and cleanup only ever touch databases the
// invocation itself created. It skips only when no database is configured.
func TestPoolMemoryHarnessDDLGuardsLiveDB(t *testing.T) {
	adminDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	pmRequireThrowawayDB(ctx, t, adminDSN)

	// Both helpers report on the *testing.T they are given, so a subtest never fails its parent.
	dbExists := func(tt *testing.T, name string) bool {
		tt.Helper()
		c, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			tt.Fatalf("dbExists connect: %v", err)
		}
		defer func() { _ = c.Close(ctx) }()
		if err := pmCheckMarker(ctx, c); err != nil {
			tt.Fatalf("dbExists marker: %v", err)
		}
		var ok bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok); err != nil {
			tt.Fatalf("dbExists query: %v", err)
		}
		return ok
	}
	// makeDB creates a fixture database directly from template0 and registers its exact-name drop
	// on tt.
	makeDB := func(tt *testing.T, name string) {
		tt.Helper()
		if err := pmAdminExec(ctx, adminDSN, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
			tt.Fatalf("create fixture %s: %v", name, err)
		}
		tt.Cleanup(func() {
			if err := pmDropDB(adminDSN, name); err != nil {
				tt.Errorf("drop fixture %s: %v", name, err)
			}
		})
	}

	outFile, err := os.Create(filepath.Join(t.TempDir(), "out.jsonl"))
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	defer func() { _ = outFile.Close() }()

	labPrefix := pmGuardPrefix()
	tplName, err := pmDBName(labPrefix, "_tpl")
	if err != nil {
		t.Fatalf("template name: %v", err)
	}
	makeDB(t, tplName)
	lab := &pmLab{t: t, out: &pmOut{f: outFile}, adminDSN: adminDSN, rawDir: t.TempDir(), jit: "default",
		prefix: labPrefix, templateName: tplName, exec: pmAdminExec}

	t.Run("unmarked connection", func(t *testing.T) {
		p := pmGuardPrefix()
		unmarked := p + "_unmarked"
		probe := p + "_probe"
		makeDB(t, unmarked)
		t.Cleanup(func() { // safety net if the guard ever fails: drop the probe and fail the test
			if !dbExists(t, probe) {
				return
			}
			t.Errorf("probe database %s exists; dropping it", probe)
			if err := pmDropDB(adminDSN, probe); err != nil {
				t.Errorf("drop probe %s: %v", probe, err)
			}
		})
		udsn := pmDSNForDB(t, adminDSN, unmarked)
		uc, err := pgx.Connect(ctx, udsn)
		if err != nil {
			t.Fatalf("connect unmarked: %v", err)
		}
		pre := pmCheckMarker(ctx, uc)
		_ = uc.Close(ctx)
		if pre == nil {
			t.Fatal("precondition: the unmarked database unexpectedly carries the marker")
		}
		err = pmAdminExec(ctx, udsn, "CREATE DATABASE "+pgx.Identifier{probe}.Sanitize())
		var ae *pmAdminErr
		if !errors.As(err, &ae) || ae.Stage != pmStageMarker {
			t.Fatalf("pmAdminExec on unmarked db = %v, want marker-stage pmAdminErr", err)
		}
		if dbExists(t, probe) {
			t.Fatalf("probe database %s was created despite the missing marker", probe)
		}
	})

	t.Run("distinct names", func(t *testing.T) {
		a, b := pmGuardPrefix(), pmGuardPrefix()
		if a == b {
			t.Fatal("two prefixes are identical")
		}
		if _, err := pmDBName(a, strings.Repeat("a", 40)); err == nil {
			t.Error("over-long name accepted")
		}
		if got, err := pmDBName(a, "_Mem_R1"); err != nil || got != a+"_mem_r1" {
			t.Errorf("uppercase suffix: got %q, %v", got, err)
		}
		for _, bad := range []string{"_a-b", `_a"b`, "_a b"} {
			if _, err := pmDBName(a, bad); err == nil {
				t.Errorf("suffix %q accepted", bad)
			}
		}
	})

	t.Run("plan cache mode guard", func(t *testing.T) {
		name := pmGuardPrefix() + "_plan"
		makeDB(t, name)
		dsn := pmDSNForDB(t, adminDSN, name)
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		q := u.Query()
		q.Set("options", "-c plan_cache_mode=force_custom_plan")
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
		acquire := func(dsn string) (*pgxpool.Conn, *pgxpool.Pool, error) {
			cfg, err := pmGuardEnv(t, dsn, pmCand{Name: "A"}).poolConfig()
			if err != nil {
				t.Fatalf("poolConfig: %v", err)
			}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("new pool: %v", err)
			}
			c, err := pool.Acquire(ctx)
			return c, pool, err
		}
		c, pool, err := acquire(u.String())
		if c != nil {
			c.Release()
		}
		pool.Close()
		if err == nil || !strings.Contains(err.Error(), "plan_cache_mode") {
			t.Fatalf("Acquire with plan_cache_mode=force_custom_plan = %v, want a plan_cache_mode error", err)
		}
		c, pool, err = acquire(dsn)
		if err != nil {
			pool.Close()
			t.Fatalf("Acquire with the plain DSN: %v", err)
		}
		c.Release()
		pool.Close()
	})

	victim := labPrefix + "_victim"
	makeDB(t, victim)
	t.Run("collision", func(t *testing.T) {
		l := *lab
		l.t = t
		ran := false
		err := l.clone(ctx, "victim", func(string, string, *pmObserver) { ran = true })
		if err == nil {
			t.Fatal("clone over a pre-existing database returned nil")
		}
		if strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("a definite server ERROR was classified ambiguous: %v", err)
		}
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42P04" {
			t.Fatalf("clone error = %v, want a *pgconn.PgError with code 42P04 (duplicate_database)", err)
		}
		if ran {
			t.Fatal("fn ran although the CREATE failed")
		}
	})
	if !dbExists(t, victim) {
		t.Fatalf("pre-existing database %s was dropped", victim)
	}

	foreign := "pm_" + strings.ReplaceAll(uuid.New().String(), "-", "") + "_foreign"
	makeDB(t, foreign)
	okName := labPrefix + "_ok"
	t.Cleanup(func() { // safety net: drop okName only if the clone's own cleanup left it behind
		if !dbExists(t, okName) {
			return
		}
		if err := pmDropDB(adminDSN, okName); err != nil {
			t.Errorf("drop %s: %v", okName, err)
		}
	})
	t.Run("owned-only cleanup", func(t *testing.T) {
		l := *lab
		l.t = t
		ran := false
		err := l.clone(ctx, "ok", func(name, _ string, _ *pmObserver) {
			ran = true
			if name != okName {
				t.Errorf("clone name %q, want %q", name, okName)
			}
			if !dbExists(t, name) {
				t.Errorf("clone %s does not exist inside fn", name)
			}
		})
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if !ran {
			t.Fatal("fn did not run")
		}
	})
	if dbExists(t, okName) {
		t.Errorf("owned clone %s was not dropped", okName)
	}
	for _, keep := range []string{foreign, victim} {
		if !dbExists(t, keep) {
			t.Errorf("database %s that this lab did not create was dropped", keep)
		}
	}

	ambName := labPrefix + "_amb"
	t.Cleanup(func() {
		if err := pmDropDB(adminDSN, ambName); err != nil {
			t.Errorf("drop %s: %v", ambName, err)
		}
	})
	var recorded []string
	lab2 := *lab
	lab2.exec = func(ctx context.Context, dsn, sql string) error {
		recorded = append(recorded, sql)
		err := pmAdminExec(ctx, dsn, sql)
		if err == nil && strings.HasPrefix(sql, "CREATE DATABASE") {
			return errors.New("simulated lost acknowledgement")
		}
		return err
	}
	t.Run("ambiguous creation", func(t *testing.T) {
		lab2 := lab2
		lab2.t = t
		ran := false
		err := lab2.clone(ctx, "amb", func(string, string, *pmObserver) { ran = true })
		if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "preserved") {
			t.Fatalf("clone error = %v, want ambiguous/preserved", err)
		}
		if ran {
			t.Fatal("fn ran although creation was ambiguous")
		}
	})
	for _, s := range recorded {
		if strings.Contains(s, "DROP") {
			t.Errorf("a DROP was issued after an ambiguous CREATE: %s", s)
		}
	}
	if !dbExists(t, ambName) {
		t.Errorf("database %s was not preserved after the ambiguous CREATE", ambName)
	}
}
