//go:build analyticsevidence

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4780: plan and runtime evidence for the two repo-options statements after they gained a
// run-existence predicate. This is a measurement harness, not a gate: it is compiled only with
// -tags analyticsevidence and runs only with UZI_ANALYTICS_PLAN_EVIDENCE=1. The shared database
// must carry the throwaway marker (e2e/test-database-marker.sql); the seed itself lives in a
// private database (capPrivateDB) that the test drops. Run:
//
//	cd api && UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_EVIDENCE_LOG=/abs/log UZI_TEST_DATABASE_URL=... \
//	  go test -tags analyticsevidence -count=1 -v -timeout 50m \
//	  -run TestAnalyticsOptionsPlanEvidenceLiveDB ./internal/store
func TestAnalyticsOptionsPlanEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_PLAN_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PLAN_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set; the evidence harness needs a throwaway database")
	}
	pmRequireThrowawayDB(context.Background(), t, dsn)
	ctx, pool := capPrivateDB(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	now, err := time.Parse(time.RFC3339, evNowRFC)
	if err != nil {
		t.Fatalf("now: %v", err)
	}
	h := &evHarness{t: t, ctx: ctx, pool: pool, q: store.New(pool), conn: conn, now: now, runs: evEnvInt("UZI_EVIDENCE_RUNS", 20), modes: []string{"custom", "generic"}}
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		f, err := os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		h.log = f
	}

	h.simple(`SET statement_timeout = 0`)
	start := time.Now()
	for _, s := range evSeed {
		h.simple(s)
	}
	h.scalar(`SELECT id FROM users WHERE email = 'evid-0001@e2e'`, &h.topOwner)
	h.logf("base seed took %s", time.Since(start).Round(time.Second))

	// Extras on owner 1's connection (evid-0001): 1000 run-less repos, 100 repos only owner 2 ran on, and
	// 2000 owner-2 runs on owner 1's own repos.
	h.simple(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	 SELECT gen_random_uuid(), (SELECT c.id FROM forge_connections c JOIN users u ON u.id = c.user_id WHERE u.email = 'evid-0001@e2e'), 100000 + j * 100 + k,
	        'g/evid-' || lpad(j::text, 5, '0') || '-x' || lpad(k::text, 2, '0'),
	        'https://forge.e2e/g/evid-' || j || '-x' || k, 'main', true
	 FROM generate_series(1, 100) j, generate_series(1, 10) k`)
	h.simple(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	 SELECT gen_random_uuid(), (SELECT c.id FROM forge_connections c JOIN users u ON u.id = c.user_id WHERE u.email = 'evid-0001@e2e'), 200000 + j,
	        'g/evid-' || lpad(j::text, 5, '0') || '-y', 'https://forge.e2e/g/evid-' || j || '-y', 'main', true
	 FROM generate_series(1, 100) j`)
	const insertRuns = `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness)
	 SELECT gen_random_uuid(), (SELECT id FROM users WHERE email = 'evid-0002@e2e'), s.id, %IID% + row_number() OVER (), 't', 'd',
	        'completed', 'issue', TIMESTAMPTZ '` + evNowRFC + `' - interval '40 days', TIMESTAMPTZ '` + evNowRFC + `' - interval '40 days' + interval '1 hour',
	        TIMESTAMPTZ '` + evNowRFC + `' - interval '40 days' + interval '1 hour', 'claude'
	 FROM (SELECT r.id FROM repos r WHERE r.connection_id = (SELECT c.id FROM forge_connections c JOIN users u ON u.id = c.user_id WHERE u.email = 'evid-0001@e2e')
	       AND r.path_with_namespace ~ '%PATH%') s CROSS JOIN generate_series(1, %N%) n`
	for _, q := range []struct{ iid, path, n string }{
		{"9000000", `^g/evid-[0-9]{5}-y$`, "3"},
		{"9100000", `^g/evid-[0-9]{5}$`, "20"},
	} {
		sql := insertRuns
		for k, v := range map[string]string{"%IID%": q.iid, "%PATH%": q.path, "%N%": q.n} {
			sql = strings.ReplaceAll(sql, k, v)
		}
		h.simple(sql)
	}
	h.simple(`VACUUM ANALYZE repos`)
	h.simple(`VACUUM ANALYZE runs`)

	var runless, ownerRuns, otherOnly, both, repoTotal, runBearing int64
	h.scalar(`WITH c AS (SELECT r.id,
	            EXISTS (SELECT 1 FROM runs ru WHERE ru.repo_id = r.id AND ru.user_id = (SELECT id FROM users WHERE email = 'evid-0001@e2e')) AS mine,
	            EXISTS (SELECT 1 FROM runs ru WHERE ru.repo_id = r.id AND ru.user_id <> (SELECT id FROM users WHERE email = 'evid-0001@e2e')) AS theirs
	          FROM repos r WHERE r.connection_id = (SELECT c.id FROM forge_connections c JOIN users u ON u.id = c.user_id WHERE u.email = 'evid-0001@e2e'))
	  SELECT count(*) FILTER (WHERE NOT mine AND NOT theirs), count(*) FILTER (WHERE mine),
	         count(*) FILTER (WHERE theirs AND NOT mine), count(*) FILTER (WHERE mine AND theirs),
	         (SELECT count(*) FROM repos), (SELECT count(DISTINCT repo_id) FROM runs) FROM c`,
		&runless, &ownerRuns, &otherOnly, &both, &repoTotal, &runBearing)
	h.logf("SEED SHAPE (owner 1 connection): run-less=%d with-owner-runs=%d other-only=%d both=%d; repos total=%d run-bearing repos=%d",
		runless, ownerRuns, otherOnly, both, repoTotal, runBearing)
	if runless != 1000 || ownerRuns != 100 || otherOnly != 100 || both != 100 {
		t.Fatalf("seed shape run-less/owner/other-only/both = %d/%d/%d/%d, want 1000/100/100/100", runless, ownerRuns, otherOnly, both)
	}

	owner := sqlLiteral(t, h.topOwner)
	for _, w := range []evWorkload{
		{
			name:  "owner repo options",
			stmts: []evStmt{{"AnalyticsOwnerRepoOptions", store.AnalyticsOwnerRepoOptionsSQL, owner}},
			run: func(ctx context.Context, q *store.Queries) error {
				rows, err := q.AnalyticsOwnerRepoOptions(ctx, h.topOwner)
				if err == nil && len(rows) != 100 {
					t.Errorf("owner repo options returned %d rows, want 100", len(rows))
				}
				return err
			},
		},
		{
			name:  "all repo options",
			stmts: []evStmt{{"AnalyticsAllRepoOptions", store.AnalyticsAllRepoOptionsSQL, ""}},
			run: func(ctx context.Context, q *store.Queries) error {
				rows, err := q.AnalyticsAllRepoOptions(ctx)
				if err == nil && len(rows) != 201 {
					t.Errorf("all repo options returned %d rows, want 201", len(rows))
				}
				return err
			},
		},
	} {
		h.measure("options", w, h.modes, true)
	}
}
