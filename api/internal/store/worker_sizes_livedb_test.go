package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// PRD #4213: the builtin worker-size reconcile over a real database. Every test runs in a
// transaction that rolls back, after clearing the table inside it, so the shared database's
// worker_sizes rows are never changed. Run via ./e2e/run-store-it.sh.

func sizesTx(t *testing.T) (context.Context, pgx.Tx, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	// Same order as production: the table lock first, then the mutation.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", store.WorkerSizesLockClass); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM worker_sizes`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	return ctx, tx, store.New(pool).WithTx(tx)
}

func TestWorkerSizesSeedInsertsTheFourBuiltinsLiveDB(t *testing.T) {
	ctx, tx, q := sizesTx(t)
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rows, err := q.ListWorkerSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := builtin.Sizes()
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		r := rows[i]
		if r.Name != w.Name || r.CpuRequestMillicores != w.CPURequestMillicores || r.CpuLimitMillicores != w.CPULimitMillicores ||
			r.MemoryRequestMib != w.MemoryRequestMiB || r.MemoryLimitMib != w.MemoryLimitMiB ||
			r.DataGib != w.DataGiB || r.DindDataGib != w.DinDDataGiB || r.DisplayOrder != w.DisplayOrder ||
			!r.Builtin || r.Customized {
			t.Errorf("row %d = %+v, want builtin %+v and not customized", i, r, w)
		}
	}
}

func TestWorkerSizesRefreshPristineRewritesAChangedRowLiveDB(t *testing.T) {
	ctx, tx, q := sizesTx(t)
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	// A pristine row whose values drifted from the shipped ones (an older release's values).
	if _, err := tx.Exec(ctx, `UPDATE worker_sizes SET memory_limit_mib = 9999, data_gib = 7 WHERE name = 'm'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	m, err := q.GetWorkerSize(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if m.MemoryLimitMib != 12288 || m.DataGib != 25 || m.Customized {
		t.Errorf("pristine m = %+v, want shipped 12288 MiB / 25 GiB", m)
	}
}

func TestWorkerSizesRefreshPristineQueryRowCountLiveDB(t *testing.T) {
	ctx, tx, q := sizesTx(t)
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	s := builtin.Sizes()[1]
	params := store.RefreshPristineBuiltinWorkerSizeParams{
		Name: s.Name, CpuRequestMillicores: s.CPURequestMillicores, CpuLimitMillicores: s.CPULimitMillicores,
		MemoryRequestMib: s.MemoryRequestMiB, MemoryLimitMib: s.MemoryLimitMiB, DataGib: s.DataGiB,
		DindDataGib: s.DinDDataGiB, DisplayOrder: s.DisplayOrder,
	}
	if n, err := q.RefreshPristineBuiltinWorkerSize(ctx, params); err != nil || n != 0 {
		t.Fatalf("shipped values: %d rows, %v; want 0 (nothing to refresh)", n, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE worker_sizes SET memory_limit_mib = 9999 WHERE name = $1`, s.Name); err != nil {
		t.Fatal(err)
	}
	if n, err := q.RefreshPristineBuiltinWorkerSize(ctx, params); err != nil || n != 1 {
		t.Fatalf("drifted values: %d rows, %v; want 1", n, err)
	}
}

func TestWorkerSizesRefreshNeverOverwritesACustomizedRowLiveDB(t *testing.T) {
	ctx, tx, q := sizesTx(t)
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE worker_sizes SET memory_limit_mib = 9999, customized = true WHERE name = 'm'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	m, err := q.GetWorkerSize(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if m.MemoryLimitMib != 9999 || !m.Customized {
		t.Errorf("customized m = %+v, want the admin value 9999 kept", m)
	}

	// Reset restores the shipped values and clears customized.
	s := builtin.Sizes()[1]
	n, err := q.ResetBuiltinWorkerSize(ctx, store.ResetBuiltinWorkerSizeParams{
		Name: s.Name, CpuRequestMillicores: s.CPURequestMillicores, CpuLimitMillicores: s.CPULimitMillicores,
		MemoryRequestMib: s.MemoryRequestMiB, MemoryLimitMib: s.MemoryLimitMiB, DataGib: s.DataGiB,
		DindDataGib: s.DinDDataGiB, DisplayOrder: s.DisplayOrder,
	})
	if err != nil || n != 1 {
		t.Fatalf("reset: %d, %v", n, err)
	}
	m, err = q.GetWorkerSize(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if m.MemoryLimitMib != 12288 || m.Customized {
		t.Errorf("reset m = %+v, want shipped values and customized=false", m)
	}
}

func TestWorkerSizesShadowingAdminSizeIsLeftAloneLiveDB(t *testing.T) {
	ctx, tx, q := sizesTx(t)
	// An admin size named like a builtin (builtin=false) exists before the builtin is seeded.
	if _, err := tx.Exec(ctx, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib,
		memory_limit_mib, data_gib, dind_data_gib, display_order, builtin, customized)
		VALUES ('l', 100, 200, 300, 400, 5, 6, 9, false, false)`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	l, err := q.GetWorkerSize(ctx, "l")
	if err != nil {
		t.Fatal(err)
	}
	if l.Builtin || l.CpuRequestMillicores != 100 || l.MemoryLimitMib != 400 {
		t.Errorf("shadowing admin size l = %+v, want it untouched", l)
	}
	rows, err := q.ListWorkerSizes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("got %d rows, want 4 (the other three builtins seeded, l kept)", len(rows))
	}
}

// The schema CHECKs are the backstop for the sanity caps and the name vocabulary.
func TestWorkerSizesSchemaRejectsInvalidRowsLiveDB(t *testing.T) {
	ctx, tx, _ := sizesTx(t)
	for name, vals := range map[string]string{
		"bad name":          `'Big', 1, 1, 1, 1, 1, 1, 0, false, false`,
		"request > limit":   `'xs', 2000, 1000, 1, 1, 1, 1, 0, false, false`,
		"cpu over cap":      `'xs', 1, 64001, 1, 1, 1, 1, 0, false, false`,
		"customized custom": `'xs', 1, 1, 1, 1, 1, 1, 0, false, true`,
	} {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib,
			memory_limit_mib, data_gib, dind_data_gib, display_order, builtin, customized) VALUES (`+vals+`)`)
		_ = sp.Rollback(ctx)
		if err == nil {
			t.Errorf("%s: insert succeeded, want a CHECK violation", name)
		}
	}
}
