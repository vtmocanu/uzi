package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/dbdiskfull"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// diskFullPool opens a traced pool feeding a fresh Signal.
func diskFullPool(t *testing.T) (context.Context, *pgxpool.Pool, *dbdiskfull.Signal) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	sig := dbdiskfull.New(nil)
	pool, err := store.OpenPool(ctx, dsn, store.WithQueryTracer(&dbdiskfull.Tracer{Signal: sig}))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, sig
}

func TestDiskFullTracerStatementPathLiveDB(t *testing.T) {
	ctx, pool, sig := diskFullPool(t)
	_, err := pool.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = '53100'; END $$`)
	if !dbdiskfull.Is(err) {
		t.Fatalf("err = %v, want SQLSTATE 53100", err)
	}
	if !diskFullActive(sig) {
		t.Fatal("signal not active after statement-path 53100")
	}
}

func TestDiskFullTracerCommitPathLiveDB(t *testing.T) {
	ctx, pool, _ := diskFullPool(t)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	table, fn := "diskfull_t_"+suffix, "diskfull_f_"+suffix
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE TABLE %s (id int)`, table),
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION USING ERRCODE = '53100'; END $f$`, fn),
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s_trg AFTER INSERT ON %s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s()`, table, table, fn),
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})

	// A separate pool and Signal so the assertion is independent of the setup traffic.
	fresh := dbdiskfull.New(nil)
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	p2, err := store.OpenPool(ctx, dsn, store.WithQueryTracer(&dbdiskfull.Tracer{Signal: fresh}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Close)

	tx, err := p2.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (1)`, table)); err != nil {
		t.Fatalf("insert must succeed before commit: %v", err)
	}
	if diskFullActive(fresh) {
		t.Fatal("signal active before commit")
	}
	cerr := fmt.Errorf("commit: %w", tx.Commit(ctx))
	if !dbdiskfull.Is(cerr) {
		t.Fatalf("commit err = %v, want wrapped 53100", cerr)
	}
	if !diskFullActive(fresh) {
		t.Fatal("signal not active after commit-time 53100")
	}
}

func TestDiskFullTracerControlLiveDB(t *testing.T) {
	ctx, pool, sig := diskFullPool(t)
	var n int
	err := pool.QueryRow(ctx, `SELECT 1/0`).Scan(&n)
	if err == nil {
		t.Fatal("want division-by-zero error")
	}
	if dbdiskfull.Is(err) || diskFullActive(sig) {
		t.Fatalf("22012 must not activate the signal (err %v)", err)
	}
}

// diskFullActive reports whether sig currently sees a disk-full sighting.
func diskFullActive(sig *dbdiskfull.Signal) bool {
	active, _, _ := sig.Snapshot(time.Now())
	return active
}
