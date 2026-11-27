package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #4784: live-DB coverage for module_user_data (migration 00551). Run via
// e2e/run-store-it.sh. Hooks are process globals, so none of these tests run in
// parallel.

func openModuleDataLiveDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func mdSeedUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, "md-"+id.String()+"@e2e"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func mdHook(t *testing.T, h *atomic.Pointer[func()], f func()) {
	t.Helper()
	h.Store(&f)
	t.Cleanup(func() { h.Store(nil) })
}

func mdCreate(ctx context.Context, pool *pgxpool.Pool, u uuid.UUID, mod, doc string) (int64, error) {
	return PutModuleUserData(ctx, pool, u, mod, []byte(doc), ExpectedRevision{Kind: ExpectCreate})
}

func mdMatch(ctx context.Context, pool *pgxpool.Pool, u uuid.UUID, mod, doc string, rev int64) (int64, error) {
	return PutModuleUserData(ctx, pool, u, mod, []byte(doc), ExpectedRevision{Kind: ExpectMatch, Value: rev})
}

func mdWantConflict(t *testing.T, err error, current *int64) {
	t.Helper()
	var c *ModuleDataConflict
	if !errors.As(err, &c) {
		t.Fatalf("want ModuleDataConflict, got %v", err)
	}
	switch {
	case current == nil && c.Current != nil:
		t.Fatalf("want Current nil, got %d", *c.Current)
	case current != nil && (c.Current == nil || *c.Current != *current):
		t.Fatalf("want Current %d, got %v", *current, c.Current)
	}
}

func mdWantDoc(ctx context.Context, t *testing.T, pool *pgxpool.Pool, u uuid.UUID, mod, doc string, rev int64) {
	t.Helper()
	data, cur, err := GetModuleUserDataDoc(ctx, New(pool), u, mod)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cur == nil || *cur != rev || string(data) != doc {
		t.Fatalf("want %s@%d, got %s@%v", doc, rev, data, cur)
	}
}

func mdCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, u uuid.UUID) int64 {
	t.Helper()
	n, err := New(pool).CountModuleUserData(ctx, u)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func mdSeedDocs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, u uuid.UUID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := mdCreate(ctx, pool, u, fmt.Sprintf("m%d", i), `{"i":1}`); err != nil {
			t.Fatalf("seed doc %d: %v", i, err)
		}
	}
}

func mdWaitNoAcquired(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("pool still has %d acquired conns", pool.Stat().AcquiredConns())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func i64(v int64) *int64 { return &v }

func TestModuleUserDataConditionalUpdateLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	r1, err := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mdMatch(ctx, pool, u, "notes", `{"v":2}`, r1)
	if err != nil {
		t.Fatal(err)
	}
	if r2 == r1 {
		t.Fatalf("revision did not change: %d", r2)
	}
	mdWantDoc(ctx, t, pool, u, "notes", `{"v": 2}`, r2)
	_, err = mdMatch(ctx, pool, u, "notes", `{"v":3}`, r1)
	mdWantConflict(t, err, &r2)
	err = DeleteModuleUserData(ctx, pool, u, "notes", ExpectedRevision{Kind: ExpectMatch, Value: r1})
	mdWantConflict(t, err, &r2)
	mdWantDoc(ctx, t, pool, u, "notes", `{"v": 2}`, r2)
}

func TestModuleUserDataDeleteRecreateLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	r1, _ := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
	r2, err := mdMatch(ctx, pool, u, "notes", `{"v":2}`, r1)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteModuleUserData(ctx, pool, u, "notes", ExpectedRevision{Kind: ExpectMatch, Value: r2}); err != nil {
		t.Fatal(err)
	}
	if d, cur, err := GetModuleUserDataDoc(ctx, New(pool), u, "notes"); err != nil || d != nil || cur != nil {
		t.Fatalf("want absent, got %s %v %v", d, cur, err)
	}
	r3, err := mdCreate(ctx, pool, u, "notes", `{"v":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if r3 == r1 || r3 == r2 {
		t.Fatalf("revision reused: %d vs %d,%d", r3, r1, r2)
	}
	_, err = mdMatch(ctx, pool, u, "notes", `{"v":4}`, r2)
	mdWantConflict(t, err, &r3)
	err = DeleteModuleUserData(ctx, pool, u, "notes", ExpectedRevision{Kind: ExpectMatch, Value: r2})
	mdWantConflict(t, err, &r3)
	mdWantDoc(ctx, t, pool, u, "notes", `{"v": 3}`, r3)
}

func TestModuleUserDataNeverMatchAndCreateExistingLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	_, err := PutModuleUserData(ctx, pool, u, "notes", []byte(`{}`), ExpectedRevision{Kind: ExpectNeverMatch})
	mdWantConflict(t, err, nil)
	err = DeleteModuleUserData(ctx, pool, u, "notes", ExpectedRevision{Kind: ExpectNeverMatch})
	mdWantConflict(t, err, nil)
	r1, err := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PutModuleUserData(ctx, pool, u, "notes", []byte(`{}`), ExpectedRevision{Kind: ExpectNeverMatch})
	mdWantConflict(t, err, &r1)
	err = DeleteModuleUserData(ctx, pool, u, "notes", ExpectedRevision{Kind: ExpectNeverMatch})
	mdWantConflict(t, err, &r1)
	_, err = mdCreate(ctx, pool, u, "notes", `{"v":2}`)
	mdWantConflict(t, err, &r1)
	mdWantDoc(ctx, t, pool, u, "notes", `{"v": 1}`, r1)
	if _, err := mdCreate(ctx, pool, uuid.New(), "notes", `{}`); !errors.Is(err, ErrModuleDataNoUser) {
		t.Fatalf("want ErrModuleDataNoUser, got %v", err)
	}
}

func TestModuleUserDataCascadeLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	mdSeedDocs(ctx, t, pool, u, 3)
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if n := mdCount(ctx, t, pool, u); n != 0 {
		t.Fatalf("want 0 documents after user delete, got %d", n)
	}
}

func TestModuleUserDataCrossUserLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	a, b := mdSeedUser(ctx, t, pool), mdSeedUser(ctx, t, pool)
	ra, err := mdCreate(ctx, pool, a, "notes", `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mdMatch(ctx, pool, b, "notes", `{"b":1}`, ra)
	mdWantConflict(t, err, nil)
	err = DeleteModuleUserData(ctx, pool, b, "notes", ExpectedRevision{Kind: ExpectMatch, Value: ra})
	mdWantConflict(t, err, nil)
	mdWantDoc(ctx, t, pool, a, "notes", `{"a": 1}`, ra)
}

func mdInvalidBodies() map[string][]byte {
	return map[string][]byte{
		"nul-escape":       []byte(`"\u0000"`),
		"lone-surrogate":   []byte(`"\ud800"`),
		"raw-invalid-utf8": {'"', 0xff, '"'},
		"huge-exponent":    []byte(`1e1000000`),
		"huge-integer":     []byte("1" + strings.Repeat("0", 131072)),
		"huge-fraction":    []byte("1." + strings.Repeat("1", 20000)),
	}
}

func TestModuleUserDataInvalidInputLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	r1, err := mdCreate(ctx, pool, u, "existing", `{"v":1}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range mdInvalidBodies() {
		t.Run(name, func(t *testing.T) {
			if _, err := PutModuleUserData(ctx, pool, u, "fresh", body, ExpectedRevision{Kind: ExpectCreate}); !errors.Is(err, ErrModuleDataInvalid) {
				t.Fatalf("create: want ErrModuleDataInvalid, got %v", err)
			}
			if _, err := PutModuleUserData(ctx, pool, u, "existing", body, ExpectedRevision{Kind: ExpectMatch, Value: r1}); !errors.Is(err, ErrModuleDataInvalid) {
				t.Fatalf("update: want ErrModuleDataInvalid, got %v", err)
			}
		})
	}
	mdWantDoc(ctx, t, pool, u, "existing", `{"v": 1}`, r1)
	if n := mdCount(ctx, t, pool, u); n != 1 {
		t.Fatalf("want 1 document, got %d", n)
	}
}

func TestModuleUserDataInvalidOverlapLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	bad := []byte(`"\u0000"`)

	t.Run("document-present-create", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		r, _ := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
		if _, err := PutModuleUserData(ctx, pool, u, "notes", bad, ExpectedRevision{Kind: ExpectCreate}); !errors.Is(err, ErrModuleDataInvalid) {
			t.Fatalf("got %v", err)
		}
		mdWantDoc(ctx, t, pool, u, "notes", `{"v": 1}`, r)
	})
	t.Run("at-cap-create", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		mdSeedDocs(ctx, t, pool, u, MaxModuleUserDocuments)
		if _, err := PutModuleUserData(ctx, pool, u, "extra", bad, ExpectedRevision{Kind: ExpectCreate}); !errors.Is(err, ErrModuleDataInvalid) {
			t.Fatalf("got %v", err)
		}
		if n := mdCount(ctx, t, pool, u); n != MaxModuleUserDocuments {
			t.Fatalf("count %d", n)
		}
	})
	t.Run("stale-match", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		r1, _ := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
		r2, _ := mdMatch(ctx, pool, u, "notes", `{"v":2}`, r1)
		if _, err := PutModuleUserData(ctx, pool, u, "notes", bad, ExpectedRevision{Kind: ExpectMatch, Value: r1}); !errors.Is(err, ErrModuleDataInvalid) {
			t.Fatalf("got %v", err)
		}
		mdWantDoc(ctx, t, pool, u, "notes", `{"v": 2}`, r2)
	})
	t.Run("never-match", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		r, _ := mdCreate(ctx, pool, u, "notes", `{"v":1}`)
		if _, err := PutModuleUserData(ctx, pool, u, "notes", bad, ExpectedRevision{Kind: ExpectNeverMatch}); !errors.Is(err, ErrModuleDataInvalid) {
			t.Fatalf("got %v", err)
		}
		mdWantDoc(ctx, t, pool, u, "notes", `{"v": 1}`, r)
	})
}

func TestModuleUserDataCancelAfterBeginLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mdHook(t, &moduleDataAfterBeginHook, cancel)
	_, err := mdCreate(cctx, pool, u, "notes", `{"v":1}`)
	if err == nil || errors.Is(err, ErrModuleDataInvalid) {
		t.Fatalf("want non-invalid error after cancel, got %v", err)
	}
	moduleDataAfterBeginHook.Store(nil)
	mdWaitNoAcquired(t, pool)
	if n := mdCount(ctx, t, pool, u); n != 0 {
		t.Fatalf("want 0 docs, got %d", n)
	}
}

// Begin-failure coverage: a closed pool fails at BeginTx, before any statement.
func TestModuleUserDataBeginFailureLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	cfg := pool.Config()
	closed, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	_, err = mdCreate(ctx, closed, u, "notes", `{}`)
	if err == nil || errors.Is(err, ErrModuleDataInvalid) {
		t.Fatalf("want begin error, got %v", err)
	}
}

func TestModuleUserDataReleaseOnEveryExitLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	freshCreate := func(u uuid.UUID, mod string) {
		t.Helper()
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := mdCreate(fctx, pool, u, mod, `{}`); err != nil {
			t.Fatalf("fresh create after prior exit: %v", err)
		}
	}

	t.Run("cap-rejection", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		mdSeedDocs(ctx, t, pool, u, MaxModuleUserDocuments)
		if _, err := mdCreate(ctx, pool, u, "extra", `{}`); !errors.Is(err, ErrModuleDataCap) {
			t.Fatalf("want cap, got %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM module_user_data WHERE user_id = $1 AND module_id = 'm0'`, u); err != nil {
			t.Fatal(err)
		}
		freshCreate(u, "extra")
	})
	t.Run("cancel-while-lock-held", func(t *testing.T) {
		u := mdSeedUser(ctx, t, pool)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		mdHook(t, &moduleDataAfterCountHook, cancel)
		if _, err := mdCreate(cctx, pool, u, "notes", `{}`); err == nil {
			t.Fatal("want error after cancel")
		}
		moduleDataAfterCountHook.Store(nil)
		freshCreate(u, "notes")
	})
	mdWaitNoAcquired(t, pool)
}

func TestModuleUserDataLastSlotRaceLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	u := mdSeedUser(ctx, t, pool)
	mdSeedDocs(ctx, t, pool, u, MaxModuleUserDocuments-1)

	const (
		pathNone = iota
		pathBothArrived
		pathLockWaiter
	)
	var (
		arrivals   atomic.Int32
		path       atomic.Int32
		bothArrive = make(chan struct{})
	)
	mdHook(t, &moduleDataAfterCountHook, func() {
		if arrivals.Add(1) == 2 {
			close(bothArrive)
			return
		}
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-bothArrive:
				path.CompareAndSwap(pathNone, pathBothArrived)
				return
			case <-deadline:
				t.Error("race hook timed out: neither path observed")
				return
			case <-tick.C:
				var n int
				err := pool.QueryRow(context.Background(),
					`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n)
				if err != nil {
					t.Errorf("observer query: %v", err)
					return
				}
				if n >= 1 {
					path.CompareAndSwap(pathNone, pathLockWaiter)
					return
				}
			}
		}
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = mdCreate(ctx, pool, u, fmt.Sprintf("race%d", i), `{}`)
		}()
	}
	wg.Wait()

	if p := path.Load(); p != pathLockWaiter {
		t.Errorf("want lock-waiter path (b), got %d (1 = both reached hook, 0 = none)", p)
	}
	var ok, capped int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrModuleDataCap):
			capped++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || capped != 1 {
		t.Errorf("want 1 success and 1 cap, got %d and %d", ok, capped)
	}
	if n := mdCount(ctx, t, pool, u); n != MaxModuleUserDocuments {
		t.Errorf("final count %d, want %d", n, MaxModuleUserDocuments)
	}
	mdWaitNoAcquired(t, pool)
}

func TestModuleUserDataEnvironmentEvidenceLiveDB(t *testing.T) {
	ctx, pool := openModuleDataLiveDB(t)
	for _, s := range []string{"synchronous_commit", "fsync", "full_page_writes", "wal_sync_method"} {
		var v string
		if err := pool.QueryRow(ctx, "SHOW "+s).Scan(&v); err != nil {
			t.Fatalf("show %s: %v", s, err)
		}
		t.Logf("test-environment configuration evidence: %s = %s", s, v)
	}
}
