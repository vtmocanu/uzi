package store

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
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

	// The count statement is five scalar subqueries, in order: the three source tiers, the
	// product, the whole table. Postgres renders each as an InitPlan section; assert per section
	// which index serves it, so a regression of one subquery cannot hide behind another's index.
	// An empty table has no statistics, so the planner would pick any of the equally priced
	// indexes. Seed 20 products' worth of rows with realistic selectivity (600 finest buckets,
	// 60 mid, 12 wide) and ANALYZE, all in one transaction that is rolled back, so the plan is the
	// one a populated table gets and nothing is left behind.
	const productID = `('00000000-0000-4000-8000-' || lpad(%s::text, 12, '0'))::uuid`
	for _, stmt := range []string{
		`BEGIN`,
		`INSERT INTO products (id, name) SELECT ` + fmt.Sprintf(productID, "i") + `, 'plan-test-' || i FROM generate_series(1, 20) i`,
		`INSERT INTO oauth_authorize_requests (product_id, redirect_uri, scopes, state, code_challenge, binding_hash, source_prefix, source_mid, source_wide, expires_at)
		 SELECT ` + fmt.Sprintf(productID, "((i % 20) + 1)") + `, 'https://p.example.test/cb', ARRAY['jobs:run'], 's', 'c', '\x00', 'p' || (i % 600), 'm' || (i % 60), 'w' || (i % 12), now() + interval '1 hour'
		   FROM generate_series(1, 6000) i`,
		`ANALYZE oauth_authorize_requests`,
	} {
		if _, err := pg.Exec(ctx, stmt).ReadAll(); err != nil {
			_, _ = pg.Exec(ctx, `ROLLBACK`).ReadAll()
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	count := plan(countLivePendingOAuthRequests)
	if _, err := pg.Exec(ctx, `ROLLBACK`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(count, "Seq Scan") {
		t.Errorf("the pending-count plan seq-scans the table:\n%s", count)
	}
	sections := strings.Split(count, "InitPlan")[1:]
	wantIdx := []string{
		"idx_oauth_authorize_requests_pending ",
		"idx_oauth_authorize_requests_pending_mid ",
		"idx_oauth_authorize_requests_pending_wide ",
		"idx_oauth_authorize_requests_pending", // any of the three: each leads with product_id
		"idx_oauth_authorize_requests_expires ",
	}
	if len(sections) != len(wantIdx) {
		t.Fatalf("the pending-count plan has %d InitPlans, want %d:\n%s", len(sections), len(wantIdx), count)
	}
	for i, want := range wantIdx {
		if !strings.Contains(sections[i], want) {
			t.Errorf("pending-count subquery %d does not use %q:\n%s", i+1, strings.TrimSpace(want), sections[i])
		}
	}
}

// TestGrantTokenCountsUseIndexesLiveDB: the two per-mint counts run under a grant's lock on every
// token mint, so they must read only the rows they count and never the grant's whole history,
// which a refresh-then-revoke loop can grow without bound. CountLiveGrantTokens must be served by
// idx_product_tokens_grant_live (partial on NOT revoked) and CountGrantTokensMintedSince by
// idx_product_tokens_grant (grant_id, created_at). It EXPLAINs the statements the generated code
// really executes (the unexported countLiveGrantTokens and countGrantTokensMintedSince
// constants) as generic plans, over a populated, analyzed table seeded in a transaction that is
// rolled back: five grants of 2000 rows each, almost all revoked and expired, which is the
// shape a loop leaves. A seq scan is made unavailable, as in the test above, so a predicate no
// index can serve fails here. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestGrantTokenCountsUseIndexesLiveDB(t *testing.T) {
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
	exec := func(sql string) {
		t.Helper()
		if _, err := pg.Exec(ctx, sql).ReadAll(); err != nil {
			_, _ = pg.Exec(ctx, `ROLLBACK`).ReadAll()
			t.Fatalf("%q: %v", sql, err)
		}
	}
	exec(`SET enable_seqscan = off`)
	defer func() { _, _ = pg.Exec(ctx, `RESET enable_seqscan`).ReadAll() }()

	exec(`BEGIN`)
	exec(`INSERT INTO users (id, email, password_hash) VALUES ('00000000-0000-4000-8000-0000000000a1', 'plan-grant-tokens@example.test', 'x')`)
	exec(`INSERT INTO products (id, name) VALUES ('00000000-0000-4000-8000-0000000000b1', 'plan-grant-tokens')`)
	exec(`INSERT INTO oauth_grants (id, user_id, product_id, scopes, revoked_at)
	      SELECT ('00000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid, '00000000-0000-4000-8000-0000000000a1',
	             '00000000-0000-4000-8000-0000000000b1', ARRAY['jobs:read'], now()
	        FROM generate_series(1, 5) i`)
	exec(`INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, revoked, created_at, expires_at, grant_id)
	      SELECT '00000000-0000-4000-8000-0000000000a1', '00000000-0000-4000-8000-0000000000b1', 'plan', sha256((g || '-' || i)::text::bytea), 'uzp_plan', ARRAY['jobs:read'],
	             i > 3, now() - (i || ' minutes')::interval, now() - (i || ' minutes')::interval + interval '1 hour',
	             ('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid
	        FROM generate_series(1, 5) g, generate_series(1, 2000) i`)
	exec(`ANALYZE product_tokens`)
	plan := func(sql string) string {
		t.Helper()
		results, err := pg.Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+sql).ReadAll()
		if err != nil {
			_, _ = pg.Exec(ctx, `ROLLBACK`).ReadAll()
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
	live := plan(countLiveGrantTokens)
	minted := plan(countGrantTokensMintedSince)
	exec(`ROLLBACK`)

	// Naming the index is not enough: with (grant_id) alone as its key the same index is still
	// chosen and the time predicate becomes a Filter that reads every row of the grant. The time
	// column must sit in the Index Cond of the scan, and in no Filter line.
	for _, c := range []struct{ name, got, index, column string }{
		{"CountLiveGrantTokens", live, "idx_product_tokens_grant_live ", "expires_at"},
		{"CountGrantTokensMintedSince", minted, "idx_product_tokens_grant ", "created_at"},
	} {
		if !strings.Contains(c.got, c.index) {
			t.Errorf("%s does not use %s:\n%s", c.name, c.index, c.got)
		}
		var inIndexCond, inFilter bool
		for _, line := range strings.Split(c.got, "\n") {
			switch trimmed := strings.TrimSpace(line); {
			case strings.HasPrefix(trimmed, "Index Cond:"):
				inIndexCond = inIndexCond || strings.Contains(trimmed, c.column)
			case strings.HasPrefix(trimmed, "Filter:"):
				inFilter = inFilter || strings.Contains(trimmed, c.column)
			}
		}
		if !inIndexCond {
			t.Errorf("%s: %s is not in the Index Cond of the plan (the index key must carry it):\n%s", c.name, c.column, c.got)
		}
		if inFilter {
			t.Errorf("%s: %s is applied as a Filter, so the scan reads rows it then discards:\n%s", c.name, c.column, c.got)
		}
		if strings.Contains(c.got, "Seq Scan") {
			t.Errorf("%s seq-scans the table:\n%s", c.name, c.got)
		}
	}
}

// TestGrantListLastUsedIsATopOneIndexProbeLiveDB: the two connections lists (ListLiveOAuthGrantsForUser
// and, reachable with a uza_ token, ListLiveOAuthGrantsForProduct) show each grant's last use. A
// grant's product_tokens history is never pruned (about 64,800 rows for one 90-day grant at the
// 30/h mint cap), so the last-use subquery must be a top-1 probe of idx_product_tokens_grant_last_used
// (grant_id, last_used_at DESC) per grant and never an aggregate over the whole history. It runs
// EXPLAIN (ANALYZE) on the statements the generated code really executes (the unexported
// listLiveOAuthGrantsForUser and listLiveOAuthGrantsForProduct constants), with the parameters
// inlined as literals, over three live grants of 5000 tokens each seeded in a transaction that is
// rolled back, and asserts the subplan is an index scan under a Limit that returns one row per
// probe. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestGrantListLastUsedIsATopOneIndexProbeLiveDB(t *testing.T) {
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
	exec := func(sql string) {
		t.Helper()
		if _, err := pg.Exec(ctx, sql).ReadAll(); err != nil {
			_, _ = pg.Exec(ctx, `ROLLBACK`).ReadAll()
			t.Fatalf("%q: %v", sql, err)
		}
	}
	exec(`SET enable_seqscan = off`)
	defer func() { _, _ = pg.Exec(ctx, `RESET enable_seqscan`).ReadAll() }()

	const (
		productID = "00000000-0000-4000-8000-0000000000b2"
		userID    = "00000000-0000-4000-8000-000000000001"
	)
	exec(`BEGIN`)
	exec(`INSERT INTO users (id, email, password_hash)
	      SELECT ('00000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid, 'grant-last-used-' || i || '@example.test', 'x'
	        FROM generate_series(1, 3) i`)
	exec(`INSERT INTO products (id, name) VALUES ('` + productID + `', 'grant-last-used')`)
	exec(`INSERT INTO oauth_grants (id, user_id, product_id, scopes)
	      SELECT ('00000000-0000-4000-8000-' || lpad((100 + i)::text, 12, '0'))::uuid,
	             ('00000000-0000-4000-8000-' || lpad(i::text, 12, '0'))::uuid,
	             '` + productID + `', ARRAY['jobs:read']
	        FROM generate_series(1, 3) i`)
	exec(`INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes, revoked, created_at, expires_at, last_used_at, grant_id)
	      SELECT ('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, '` + productID + `', 'plan', sha256((g || '-' || i)::text::bytea), 'uzp_plan', ARRAY['jobs:read'],
	             i > 3, now() - (i || ' minutes')::interval, now() - (i || ' minutes')::interval + interval '1 hour',
	             CASE WHEN i % 2 = 0 THEN now() - (i || ' seconds')::interval END,
	             ('00000000-0000-4000-8000-' || lpad((100 + g)::text, 12, '0'))::uuid
	        FROM generate_series(1, 3) g, generate_series(1, 5000) i`)
	exec(`ANALYZE product_tokens`)
	analyze := func(sql string) string {
		t.Helper()
		sql = strings.ReplaceAll(sql, "$2", "1000")
		sql = strings.ReplaceAll(sql, "$1", "'"+productID+"'::uuid")
		results, err := pg.Exec(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) `+sql).ReadAll()
		if err != nil {
			_, _ = pg.Exec(ctx, `ROLLBACK`).ReadAll()
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
	// The user list takes the user id as $1; the product list takes the product id as $1.
	forUser := analyze(strings.Replace(listLiveOAuthGrantsForUser, "$1", "'"+userID+"'::uuid", 1))
	forProduct := analyze(listLiveOAuthGrantsForProduct)
	exec(`ROLLBACK`)

	oneRow := regexp.MustCompile(`rows=1(\.00)? loops=\d+`)
	for _, c := range []struct {
		name, got string
		probes    int
	}{
		{"ListLiveOAuthGrantsForUser", forUser, 1},
		{"ListLiveOAuthGrantsForProduct", forProduct, 3},
	} {
		if !strings.Contains(c.got, "idx_product_tokens_grant_last_used") {
			t.Errorf("%s does not use idx_product_tokens_grant_last_used:\n%s", c.name, c.got)
		}
		if !strings.Contains(c.got, "Limit") {
			t.Errorf("%s has no Limit over the last-use probe:\n%s", c.name, c.got)
		}
		if strings.Contains(c.got, "Seq Scan on product_tokens") || strings.Contains(c.got, "Aggregate") {
			t.Errorf("%s scans or aggregates the token history:\n%s", c.name, c.got)
		}
		var scanned bool
		for _, line := range strings.Split(c.got, "\n") {
			if !strings.Contains(line, "idx_product_tokens_grant_last_used") {
				continue
			}
			scanned = true
			if !oneRow.MatchString(line) {
				t.Errorf("%s: the index probe does not return one row per grant: %s\n%s", c.name, strings.TrimSpace(line), c.got)
			}
			if !strings.Contains(line, "loops="+strconv.Itoa(c.probes)) {
				t.Errorf("%s: want %d probes (one per live grant): %s", c.name, c.probes, strings.TrimSpace(line))
			}
		}
		if !scanned {
			t.Errorf("%s: no index scan line in the plan:\n%s", c.name, c.got)
		}
	}
}
