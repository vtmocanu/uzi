package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestOAuthSweepAndCapQueriesUseIndexesLiveDB: the TTL sweep and the authorize cap counts must be
// served by the oauth_authorize_requests indexes, not by a scan of the table. It EXPLAINs the
// statements the generated code really executes (the unexported deleteExpiredOAuthAuthorizeRequests
// and countLivePendingOAuthRequests constants), as generic plans so the $n bind parameters stay
// parameters. A seq scan is made unavailable (SET enable_seqscan = off) because a near-empty
// table would otherwise always plan one; with it off, a predicate no index can serve shows as a
// Seq Scan anyway (a CASE-over-status sweep does) and fails here.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestOAuthSweepAndCapQueriesUseIndexesLiveDB(t *testing.T) {
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
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pg := conn.Conn().PgConn()
	// Raw simple-protocol statements: pgx's own simple mode would try to substitute the $n
	// placeholders, and the extended one demands their arguments.
	if _, err := pg.Exec(ctx, `SET enable_seqscan = off`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pg.Exec(ctx, `RESET enable_seqscan`).ReadAll() }()
	plan := func(sql string) string {
		t.Helper()
		results, err := pg.Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+sql).ReadAll()
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var b strings.Builder
		for _, res := range results {
			for _, row := range res.Rows {
				b.WriteString(string(row[0]) + "\n")
			}
		}
		return b.String()
	}

	sweep := plan(deleteExpiredOAuthAuthorizeRequests)
	for _, want := range []string{"idx_oauth_authorize_requests_expires", "idx_oauth_authorize_requests_code_expires"} {
		if !strings.Contains(sweep, want) {
			t.Errorf("the sweep plan does not use %s:\n%s", want, sweep)
		}
	}
	if strings.Contains(sweep, "Seq Scan") {
		t.Errorf("the sweep plan seq-scans the table:\n%s", sweep)
	}

	count := plan(countLivePendingOAuthRequests)
	if !strings.Contains(count, "idx_oauth_authorize_requests_pending") {
		t.Errorf("the pending-count plan does not use idx_oauth_authorize_requests_pending:\n%s", count)
	}
	if strings.Contains(count, "Seq Scan") {
		t.Errorf("the pending-count plan seq-scans the table:\n%s", count)
	}
}
