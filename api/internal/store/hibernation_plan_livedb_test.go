package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// PRD #4205 M5: the two queries that evaluate fn_placement_admitting_workers per run must let the
// planner inline it. A "Function Scan on fn_placement_admitting_workers" node would mean the
// function body is opaque and evaluated as a set per outer row. Skipped without
// UZI_TEST_DATABASE_URL; run via e2e/run-store-it.sh.
func TestPlacementFunctionIsInlinedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for name, sql := range map[string]string{
		"ListUnplaceableQueuedRunsForEphemeral": listUnplaceableQueuedRunsForEphemeral,
		"ListHibernationWakeDemand":             listHibernationWakeDemand,
	} {
		// Raw simple-protocol exec: pgx would otherwise try to bind the $n placeholders.
		results, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN) "+sql).ReadAll()
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var plan strings.Builder
		for _, res := range results {
			for _, row := range res.Rows {
				plan.Write(row[0])
				plan.WriteString("\n")
			}
		}
		if plan.Len() == 0 {
			t.Fatalf("%s: empty plan", name)
		}
		if strings.Contains(plan.String(), "Function Scan on fn_placement_admitting_workers") {
			t.Errorf("%s: fn_placement_admitting_workers was not inlined:\n%s", name, plan.String())
		}
	}
}
