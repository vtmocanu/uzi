//go:build analyticsevidence

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684 (m5): plan and runtime evidence for the Spend statements and the options reads.
// This is a measurement harness, not a gate: it is compiled only with -tags analyticsevidence
// and runs only with UZI_ANALYTICS_PLAN_EVIDENCE=1 against a throwaway database (it
// TRUNCATEs users CASCADE). Run, one named package at a time:
//
//	cd api && UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_TEST_DATABASE_URL=... \
//	  go test -tags analyticsevidence -count=1 -v -timeout 50m \
//	  -run TestAnalyticsPlanEvidenceLiveDB ./internal/store
//
// Optional environment:
//
//	UZI_EVIDENCE_LOG        raw log path (appended); default stdout only
//	UZI_EVIDENCE_KEEP=1     leave the seed in place (default: TRUNCATE at the end)
//	UZI_EVIDENCE_RESEED=0   reuse an existing seed (skips the TRUNCATE and the seed)
//	UZI_EVIDENCE_BASELINE=0 skip the baseline phase
//	UZI_EVIDENCE_INDEXES=   comma list of candidate numbers 1-4 (default 1,2,3,4; "" or "0" none)
//	UZI_EVIDENCE_ONLY=      comma list of workload-name substrings
//	UZI_EVIDENCE_RUNS=20    measured executions per workload and mode
//	UZI_EVIDENCE_MODES=     custom,generic (timing modes for the baseline)

const (
	evNowRFC = "2026-11-20T12:00:00Z"
	evRuns   = 120000
)

// evSeed is executed statement by statement on ONE connection (temp tables).
var evSeed = []string{
	`SELECT setseed(0.4684)`,
	`TRUNCATE users CASCADE`,
	`INSERT INTO users (id, email, password_hash)
	 SELECT gen_random_uuid(), 'evid-' || lpad(g::text, 4, '0') || '@e2e', 'x' FROM generate_series(1, 200) g`,
	`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	 SELECT gen_random_uuid(), id, 'gitlab', 'https://forge.e2e', 'bot', 1, '\x01'::bytea FROM users WHERE email LIKE 'evid-%'`,
	`CREATE TEMP TABLE ev_owner AS
	 SELECT row_number() OVER (ORDER BY u.email) AS idx, u.id AS user_id, c.id AS conn_id
	 FROM users u JOIN forge_connections c ON c.user_id = u.id WHERE u.email LIKE 'evid-%'`,
	// 1250 repos: owner 1 holds 100, owners 2-10 hold 200 between them, the rest spread over 11-200.
	`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	 SELECT gen_random_uuid(), o.conn_id, j, 'g/evid-' || lpad(j::text, 5, '0'), 'https://forge.e2e/g/evid-' || j, 'main', true
	 FROM generate_series(1, 1250) j
	 JOIN ev_owner o ON o.idx = CASE WHEN j <= 100 THEN 1 WHEN j <= 300 THEN 2 + (j % 9) ELSE 11 + (j % 190) END`,
	`CREATE TEMP TABLE ev_repo AS
	 SELECT r.id, o.idx, row_number() OVER (PARTITION BY o.idx ORDER BY r.path_with_namespace) AS rn,
	        count(*) OVER (PARTITION BY o.idx) AS cnt
	 FROM repos r JOIN ev_owner o ON o.conn_id = r.connection_id WHERE r.path_with_namespace LIKE 'g/evid-%'`,
	// Owner skew: one owner 20%, owners 2-10 another 40% (top 10 = 60%), the rest 40% over 190.
	// Kinds: delivery 81% (issue 50, task 10, self_improve 6, ci_fix 7, prompt 5, job 3),
	// supporting 19% (judge 17, chat 2).
	`CREATE TEMP TABLE ev_gen AS
	 SELECT g, oidx,
	        CASE WHEN rk < 0.50 THEN 'issue' WHEN rk < 0.60 THEN 'task' WHEN rk < 0.66 THEN 'self_improve'
	             WHEN rk < 0.73 THEN 'ci_fix' WHEN rk < 0.78 THEN 'prompt' WHEN rk < 0.81 THEN 'job'
	             WHEN rk < 0.98 THEN 'judge' ELSE 'chat' END AS kind,
	        rs, rm, rq, rr, rh,
	        TIMESTAMPTZ '` + evNowRFC + `' - (CASE WHEN rt < 0.10 THEN (rt / 0.10) * 30
	                                             ELSE 30 + ((rt - 0.10) / 0.90) * 1065 END) * interval '1 day' AS ts
	 FROM (SELECT g,
	              CASE WHEN a < 0.20 THEN 1 WHEN a < 0.60 THEN 2 + floor(random() * 9)::int
	                   ELSE 11 + floor(random() * 190)::int END AS oidx,
	              random() AS rk, random() AS rs, random() AS rt, random() AS rm, random() AS rq,
	              random() AS rr, random() AS rh
	       FROM (SELECT g, random() AS a FROM generate_series(1, ` + strconv.Itoa(evRuns) + `) g) s) s2`,
	`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness, fail_origin, mr_iid, mr_state, branch, pipeline_id,
	                   pipeline_ref, job_type)
	 SELECT s.id, s.user_id, s.repo_id,
	        CASE WHEN s.kind IN ('issue', 'self_improve') THEN s.g END,
	        't', 'd', s.status, s.kind, s.ts, s.fin, COALESCE(s.fin, s.ts),
	        CASE WHEN s.rh < 0.7 THEN 'claude' ELSE 'codex' END,
	        CASE WHEN s.status = 'failed' THEN
	             CASE WHEN s.rq < 0.15 THEN 'plan_rejected' WHEN s.rq < 0.25 THEN 'base_diverged'
	                  WHEN s.rq < 0.70 THEN 'agent_failure' END END,
	        s.mr_iid,
	        CASE WHEN s.mr_iid IS NOT NULL THEN CASE WHEN s.mr_iid % 5 < 2 THEN 'merged' ELSE 'opened' END END,
	        CASE WHEN s.kind = 'task' THEN 'uzi/task/' || s.id END,
	        CASE WHEN s.kind = 'ci_fix' THEN s.g END,
	        CASE WHEN s.kind = 'ci_fix' THEN 'refs/pipe/' || s.g END,
	        CASE WHEN s.kind = 'job' THEN 'research' END
	 FROM (SELECT gen_random_uuid() AS id, o.user_id, g.g, g.kind, g.ts, g.rh, g.rq,
	              CASE WHEN g.kind IN ('chat', 'job') THEN NULL ELSE r.id END AS repo_id,
	              CASE WHEN g.rs < 0.62 THEN 'completed' WHEN g.rs < 0.80 THEN 'failed' WHEN g.rs < 0.90 THEN 'cancelled'
	                   WHEN g.rs >= 0.97 AND g.kind = 'issue' THEN 'queued' ELSE 'completed' END AS status,
	              CASE WHEN NOT (g.rs >= 0.97 AND g.kind = 'issue') THEN g.ts + (60 + g.rh * 7200) * interval '1 second' END AS fin,
	              CASE WHEN g.kind IN ('issue', 'task', 'self_improve', 'ci_fix', 'prompt') AND g.rm < 0.30
	                   THEN 1 + ((hashtext(g.g::text)::bigint & 2147483647) % 150)::int END AS mr_iid
	       FROM ev_gen g
	       JOIN ev_owner o ON o.idx = g.oidx
	       LEFT JOIN ev_repo r ON r.idx = g.oidx AND r.rn = 1 + floor(g.rr * r.cnt)::int
	       WHERE g.kind <> 'judge') s`,
	`CREATE TEMP TABLE ev_iss AS
	 SELECT id, user_id, row_number() OVER (PARTITION BY user_id ORDER BY created_at, id) AS rn,
	        count(*) OVER (PARTITION BY user_id) AS cnt
	 FROM runs WHERE kind = 'issue'`,
	`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
	                   finished_at, status_since, harness, target_run_id)
	 SELECT gen_random_uuid(), o.user_id, NULL, NULL, 't', 'd', CASE WHEN g.rs < 0.8 THEN 'completed' ELSE 'failed' END,
	        'judge', g.ts, g.ts + (60 + g.rh * 900) * interval '1 second', g.ts + (60 + g.rh * 900) * interval '1 second',
	        'claude', i.id
	 FROM ev_gen g
	 JOIN ev_owner o ON o.idx = g.oidx
	 JOIN ev_iss i ON i.user_id = o.user_id AND i.rn = 1 + floor(g.rr * i.cnt)::int
	 WHERE g.kind = 'judge'`,
	// run_usage: ~2 sessions x 2 models x 1-3 epochs per non-chat run; 10% of runs have no usage
	// at all, 15% record session_cumulative legs, 10% subscription and 10% unreported legs.
	`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, input_tokens, cache_read_tokens,
	                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
	 SELECT r.id, 'sess-' || s, 'ev-model-' || ((r.h % 8 + m * 3) % 8), e, s,
	        CASE WHEN r.h % 100 < 25 THEN (e + 1) * (100 + r.h % 50) ELSE 100 + e * 10 + r.h % 50 END,
	        20 + r.h % 30, 5 + r.h % 7, 50 + e,
	        CASE (r.h / 100) % 10 WHEN 0 THEN 0 WHEN 1 THEN 0
	             ELSE 0.001 * (e + 1) * (1 + r.h % 7) * CASE WHEN r.h % 100 < 25 THEN 2 ELSE 1 END END,
	        r.harness,
	        CASE (r.h / 100) % 10 WHEN 0 THEN 'subscription' WHEN 1 THEN 'unreported' ELSE 'metered' END,
	        CASE WHEN r.h % 100 < 25 THEN 'session_cumulative' ELSE 'per_leg' END
	 FROM (SELECT id, harness, (hashtext(id::text)::bigint & 2147483647) AS h
	       FROM runs WHERE kind <> 'chat' AND user_id IN (SELECT user_id FROM ev_owner)) r
	 CROSS JOIN generate_series(0, 1) s
	 CROSS JOIN generate_series(0, 1) m
	 CROSS JOIN generate_series(0, 2) e
	 WHERE r.h % 100 >= 10 AND e <= r.h % 3`,
	`DROP TABLE ev_gen`, `DROP TABLE ev_iss`, `DROP TABLE ev_repo`, `DROP TABLE ev_owner`,
	`ANALYZE users`, `ANALYZE forge_connections`, `ANALYZE repos`, `ANALYZE runs`, `ANALYZE run_usage`,
}

type evStmt struct{ name, sql, args string }

type evWorkload struct {
	name  string
	stmts []evStmt
	run   func(ctx context.Context, q *store.Queries) error
	spend bool
}

type evHarness struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *store.Queries
	conn     *pgxpool.Conn
	log      *os.File
	now      time.Time
	topOwner uuid.UUID
	runs     int
	modes    []string
	sets     []string // extra session settings, e.g. "SET jit = off"
}

func (h *evHarness) logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if h.log != nil {
		_, _ = h.log.WriteString(line + "\n")
	}
	h.t.Log(line)
}

func (h *evHarness) simple(sql string) {
	h.t.Helper()
	if _, err := h.conn.Exec(h.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		h.t.Fatalf("exec %.200q: %v", sql, err)
	}
}

func (h *evHarness) scalar(sql string, dst ...any) {
	h.t.Helper()
	if err := h.conn.QueryRow(h.ctx, sql, pgx.QueryExecModeSimpleProtocol).Scan(dst...); err != nil {
		h.t.Fatalf("query %.200q: %v", sql, err)
	}
}

func evEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func TestAnalyticsPlanEvidenceLiveDB(t *testing.T) {
	if os.Getenv("UZI_ANALYTICS_PLAN_EVIDENCE") != "1" {
		t.Skip("UZI_ANALYTICS_PLAN_EVIDENCE=1 not set; this is a measurement harness, not a gate")
	}
	ctx, pool := openAnalyticsViewsDB(t)
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
	if m := os.Getenv("UZI_EVIDENCE_MODES"); m != "" {
		h.modes = strings.Split(m, ",")
	}
	if p := os.Getenv("UZI_EVIDENCE_LOG"); p != "" {
		f, err := os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304/G703: the operator-chosen log path of a measurement harness that never runs in CI
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		t.Cleanup(func() { _ = f.Close() })
		h.log = f
	}
	h.simple(`SET statement_timeout = 0`)
	if s := os.Getenv("UZI_EVIDENCE_SET"); s != "" {
		h.sets = strings.Split(s, ";")
		for _, stmt := range h.sets {
			h.simple(stmt)
		}
		h.logf("SESSION SETTINGS: %s", s)
	}
	for _, ix := range evIndexes {
		h.simple("DROP INDEX IF EXISTS " + ix.name)
	}
	if os.Getenv("UZI_EVIDENCE_KEEP") != "1" {
		t.Cleanup(func() {
			for _, ix := range evIndexes {
				_, _ = pool.Exec(context.Background(), "DROP INDEX IF EXISTS "+ix.name)
			}
			_, _ = pool.Exec(context.Background(), `TRUNCATE users CASCADE`)
		})
	}

	h.seed()
	workloads := h.workloads()

	if os.Getenv("UZI_EVIDENCE_BASELINE") != "0" {
		h.logf("\n######## BASELINE (no candidate index) ########")
		h.logDisk("baseline")
		for _, w := range workloads {
			h.measure("baseline", w, h.modes, true)
		}
	}
	list := "1,2,3,4"
	if v, ok := os.LookupEnv("UZI_EVIDENCE_INDEXES"); ok {
		list = v
	}
	for _, s := range strings.Split(list, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > len(evIndexes) {
			continue
		}
		ix := evIndexes[n-1]
		h.logf("\n######## CANDIDATE %d: %s ########", n, ix.ddl)
		start := time.Now()
		h.simple(ix.ddl)
		h.simple(`ANALYZE runs`)
		var size int64
		h.scalar(fmt.Sprintf("SELECT pg_relation_size('%s')", ix.name), &size)
		h.logf("CREATE INDEX + ANALYZE took %s; index size %d bytes", time.Since(start).Round(time.Millisecond), size)
		h.logDisk("with " + ix.name)
		for _, w := range workloads {
			h.measure("idx"+strconv.Itoa(n), w, []string{"custom"}, false, ix.name)
		}
		h.simple("DROP INDEX " + ix.name)
		h.simple(`ANALYZE runs`)
	}
}

var evIndexes = []struct{ name, ddl string }{
	{"evid_runs_created_idx", `CREATE INDEX evid_runs_created_idx ON runs (created_at)`},
	{"evid_runs_repo_mr_idx", `CREATE INDEX evid_runs_repo_mr_idx ON runs (repo_id, mr_iid) WHERE mr_iid IS NOT NULL`},
	{"evid_runs_repo_mr_merged_idx", `CREATE INDEX evid_runs_repo_mr_merged_idx ON runs (repo_id, mr_iid) WHERE mr_state = 'merged'`},
	{"evid_runs_user_repo_mr_merged_idx", `CREATE INDEX evid_runs_user_repo_mr_merged_idx ON runs (user_id, repo_id, mr_iid) WHERE mr_state = 'merged'`},
}

func (h *evHarness) seed() {
	h.t.Helper()
	if os.Getenv("UZI_EVIDENCE_RESEED") != "0" {
		start := time.Now()
		for _, s := range evSeed {
			h.simple(s)
		}
		h.logf("seed took %s", time.Since(start).Round(time.Second))
	}
	var nRuns, nUsage, nRepos, nUsers, nRunRepos int64
	h.scalar(`SELECT (SELECT count(*) FROM runs), (SELECT count(*) FROM run_usage), (SELECT count(*) FROM repos),
	                 (SELECT count(*) FROM users WHERE email LIKE 'evid-%'), (SELECT count(DISTINCT repo_id) FROM runs)`,
		&nRuns, &nUsage, &nRepos, &nUsers, &nRunRepos)
	if nRuns < 100000 {
		h.t.Fatalf("seed has %d runs, need >= 100000", nRuns)
	}
	h.scalar(`SELECT id FROM users WHERE email = 'evid-0001@e2e'`, &h.topOwner)
	h.logf("SEED SHAPE: runs=%d run_usage=%d repos=%d users=%d distinct run repos=%d", nRuns, nUsage, nRepos, nUsers, nRunRepos)
	h.logf("%s", h.shape(`SELECT 'owner1 share', round(100.0 * count(*) FILTER (WHERE user_id = (SELECT id FROM users WHERE email = 'evid-0001@e2e')) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'top10 owners share', round(100.0 * sum(c) FILTER (WHERE rk <= 10) / sum(c), 1)
	                       FROM (SELECT count(*) AS c, row_number() OVER (ORDER BY count(*) DESC) AS rk FROM runs GROUP BY user_id) x`))
	h.logf("%s", h.shape(`SELECT 'last 30d share', round(100.0 * count(*) FILTER (WHERE created_at >= TIMESTAMPTZ '`+evNowRFC+`' - interval '30 days') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'last 90d share', round(100.0 * count(*) FILTER (WHERE created_at >= TIMESTAMPTZ '`+evNowRFC+`' - interval '90 days') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'mr_iid share', round(100.0 * count(mr_iid) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'merged share of mr', round(100.0 * count(*) FILTER (WHERE mr_state = 'merged') / nullif(count(mr_iid), 0), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'chat share', round(100.0 * count(*) FILTER (WHERE kind = 'chat') / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'delivery share', round(100.0 * count(*) FILTER (WHERE kind IN ('issue','mr_rework','ci_fix','prompt','self_improve','task','job')) / count(*), 1) FROM runs`))
	h.logf("%s", h.shape(`SELECT 'status mix', string_agg(status || '=' || c, ' ' ORDER BY status) FROM (SELECT status, count(*) c FROM runs GROUP BY status) x`))
	h.logf("%s", h.shape(`SELECT 'kind mix', string_agg(kind || '=' || c, ' ' ORDER BY kind) FROM (SELECT kind, count(*) c FROM runs GROUP BY kind) x`))
	h.logf("%s", h.shape(`SELECT 'merged witness distinct (repo,mr)', count(*) FROM (SELECT DISTINCT repo_id, mr_iid FROM runs WHERE mr_state = 'merged' AND mr_iid IS NOT NULL AND repo_id IS NOT NULL) x`))
	h.logf("%s", h.shape(`SELECT 'usage legs/run (with usage)', round(count(*)::numeric / count(DISTINCT run_id), 2) FROM run_usage`))
	h.logf("%s", h.shape(`SELECT 'runs without usage (non-chat)', count(*) FROM runs r WHERE kind <> 'chat' AND NOT EXISTS (SELECT 1 FROM run_usage u WHERE u.run_id = r.id)`))
	h.logf("%s", h.shape(`SELECT 'session_cumulative legs', count(*) FROM run_usage WHERE usage_basis = 'session_cumulative'`))
}

func (h *evHarness) shape(sql string) string {
	var label, val string
	h.scalar("SELECT a::text, b::text FROM ("+sql+") x(a, b)", &label, &val)
	return fmt.Sprintf("  %s: %s", label, val)
}

func (h *evHarness) logDisk(label string) {
	for _, rel := range []string{"runs", "run_usage"} {
		var rs, is, ts int64
		h.scalar(fmt.Sprintf("SELECT pg_relation_size('%[1]s'), pg_indexes_size('%[1]s'), pg_total_relation_size('%[1]s')", rel), &rs, &is, &ts)
		h.logf("DISK [%s] %-9s relation=%d indexes=%d total=%d bytes", label, rel, rs, is, ts)
	}
}

// ---- workloads ----

func (h *evHarness) spendWorkload(name string, owner uuid.UUID, win analytics.Window, split, breakdown string) evWorkload {
	h.t.Helper()
	kinds := runkind.All()
	var hist pgtype.Timestamptz
	var err error
	if owner != uuid.Nil {
		hist, err = h.q.AnalyticsOwnerHistoryStart(h.ctx, store.AnalyticsOwnerHistoryStartParams{UserID: owner, Kinds: kinds})
	} else {
		hist, err = h.q.AnalyticsAllHistoryStart(h.ctx, store.AnalyticsAllHistoryStartParams{Kinds: kinds})
	}
	if err != nil {
		h.t.Fatalf("history start: %v", err)
	}
	var earliest *time.Time
	if hist.Valid {
		e := hist.Time
		earliest = &e
	}
	b, err := analytics.ComputeBounds(h.now, time.UTC, win, earliest)
	if err != nil {
		h.t.Fatalf("ComputeBounds: %v", err)
	}
	var prev pgtype.Timestamptz
	if b.PreviousFrom != nil {
		prev = spendTS(*b.PreviousFrom)
	}
	ownerP := store.AnalyticsOwnerSpendParams{
		UserID: owner, RevealRepos: false,
		Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), Split: split, Breakdown: breakdown,
	}
	allP := store.AnalyticsAllSpendParams{
		Now: spendTS(b.Now), From: spendTS(b.From), PreviousFrom: prev, FoldFrom: spendTS(b.FoldFrom),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		RollStarts: spendTSs(b.RollStarts), RollEnds: spendTSs(b.RollEnds),
		Kinds: kinds, DeliveryKinds: runkind.Delivery(), Split: split, Breakdown: breakdown,
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	w := evWorkload{name: name, spend: true}
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerSpend(h.ctx, ownerP)
		w.stmts = []evStmt{{name: "AnalyticsOwnerSpend", sql: store.AnalyticsOwnerSpendSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerSpend(ctx, ownerP)
			return err
		}
	} else {
		_, _ = cq.AnalyticsAllSpend(h.ctx, allP)
		w.stmts = []evStmt{{name: "AnalyticsAllSpend", sql: store.AnalyticsAllSpendSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllSpend(ctx, allP)
			return err
		}
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = sqlLiteral(h.t, a)
	}
	w.stmts[0].args = strings.Join(lits, ", ")
	h.logf("WORKLOAD %s: window=%s buckets=%d rolling=%d from=%s fold_from=%s split=%s breakdown=%s",
		name, win, len(b.BucketStarts), len(b.RollStarts), b.From.Format(time.RFC3339), b.FoldFrom.Format(time.RFC3339), split, breakdown)
	return w
}

const (
	evSQLOwnerRepoOptions = `SELECT r.id, r.path_with_namespace FROM repos r JOIN forge_connections c ON c.id = r.connection_id
		WHERE c.user_id = $1::uuid ORDER BY r.path_with_namespace COLLATE "C", r.id LIMIT 201`
	evSQLAllRepoOptions = `SELECT r.id, r.path_with_namespace FROM repos r ORDER BY r.path_with_namespace COLLATE "C", r.id LIMIT 201`
	evSQLUserOptions    = `SELECT u.id, u.email FROM users u ORDER BY u.email COLLATE "C", u.id LIMIT 201`
	evSQLOwnerHistory   = `SELECT MIN(r.created_at)::timestamptz FROM runs r WHERE r.user_id = $1::uuid
		AND ($2::uuid IS NULL OR r.repo_id = $2::uuid) AND ($3::text IS NULL OR r.harness = $3::text) AND r.kind = ANY($4::text[])`
	evSQLAllHistory = `SELECT MIN(r.created_at)::timestamptz FROM runs r
		WHERE ($1::uuid IS NULL OR r.repo_id = $1::uuid) AND ($2::text IS NULL OR r.harness = $2::text) AND r.kind = ANY($3::text[])`
)

func (h *evHarness) optionWorkloads() []evWorkload {
	kinds := sqlLiteral(h.t, runkind.All())
	owner := sqlLiteral(h.t, h.topOwner)
	return []evWorkload{
		{
			name: "owner options",
			stmts: []evStmt{
				{"AnalyticsOwnerRepoOptions", evSQLOwnerRepoOptions, owner},
				{"AnalyticsOwnerHistoryStart", evSQLOwnerHistory, owner + ", NULL::uuid, NULL::text, " + kinds},
			},
			run: func(ctx context.Context, q *store.Queries) error {
				if _, err := q.AnalyticsOwnerRepoOptions(ctx, h.topOwner); err != nil {
					return err
				}
				_, err := q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: h.topOwner, Kinds: runkind.All()})
				return err
			},
		},
		{
			name: "admin options",
			stmts: []evStmt{
				{"AnalyticsAllRepoOptions", evSQLAllRepoOptions, ""},
				{"AnalyticsUserOptions", evSQLUserOptions, ""},
				{"AnalyticsAllHistoryStart", evSQLAllHistory, "NULL::uuid, NULL::text, " + kinds},
			},
			run: func(ctx context.Context, q *store.Queries) error {
				if _, err := q.AnalyticsAllRepoOptions(ctx); err != nil {
					return err
				}
				if _, err := q.AnalyticsUserOptions(ctx); err != nil {
					return err
				}
				_, err := q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{Kinds: runkind.All()})
				return err
			},
		},
	}
}

func (h *evHarness) workloads() []evWorkload {
	all := []evWorkload{
		h.spendWorkload("owner-30d", h.topOwner, analytics.Window30d, "harness", "kind"),
		h.spendWorkload("owner-90d", h.topOwner, analytics.Window90d, "harness", "kind"),
		h.spendWorkload("admin-all-30d", uuid.Nil, analytics.Window30d, "harness", "kind"),
		h.spendWorkload("admin-all-window-all", uuid.Nil, analytics.WindowAll, "harness", "kind"),
		h.spendWorkload("admin-30d-user-user", uuid.Nil, analytics.Window30d, "user", "user"),
		h.spendWorkload("admin-all-window-user-user", uuid.Nil, analytics.WindowAll, "user", "user"),
		h.spendWorkload("admin-30d-repo-repo", uuid.Nil, analytics.Window30d, "repo", "repo"),
		h.spendWorkload("admin-all-window-repo-repo", uuid.Nil, analytics.WindowAll, "repo", "repo"),
	}
	all = append(all, h.optionWorkloads()...)
	only := os.Getenv("UZI_EVIDENCE_ONLY")
	if only == "" {
		return all
	}
	var out []evWorkload
	for _, w := range all {
		for _, f := range strings.Split(only, ",") {
			if f != "" && strings.Contains(w.name, f) {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

// ---- measurement ----

type evTiming struct {
	median, p95, max time.Duration
	n                int
	reduced          bool
}

func (h *evHarness) measure(phase string, w evWorkload, modes []string, fullPlans bool, idxNames ...string) {
	h.t.Helper()
	phase = os.Getenv("UZI_EVIDENCE_LABEL") + phase
	h.logf("\n==== [%s] %s ====", phase, w.name)
	for _, mode := range []string{"custom", "generic"} {
		for _, st := range w.stmts {
			p := h.explain(mode, st)
			h.logf("-- EXPLAIN %s plan_cache_mode=%s (generic_plans=%d custom_plans=%d): exec=%.1fms planning=%.1fms temp_written_blocks=%d temp_read_blocks=%d disk_sorts=%d jit_ms=%.1f jit_funcs=%.0f shared_hit=%d shared_read=%d",
				st.name, mode, p.generic, p.custom, p.execMs, p.planMs, p.tempW, p.tempR, p.sortDisk, p.jitMs, p.jitFuncs, p.sharedHit, p.sharedRead)
			for _, n := range idxNames {
				h.logf("   uses %s: %v", n, p.usesIndex(n))
			}
			h.logf("   runs_pkey probes (loops): %.0f", p.loopsOf("Index Scan using runs_pkey on runs")+p.loopsOf("Index Only Scan using runs_pkey on runs"))
			h.logf("   scans:")
			for _, l := range p.scanLines() {
				h.logf("     %s", l)
			}
			for _, l := range p.nlLines {
				h.logf("   NL %s", l)
			}
			if fullPlans {
				h.logf("   plan (interesting nodes):")
				for _, l := range p.lines {
					h.logf("     %s", l)
				}
			}
		}
	}
	for _, mode := range modes {
		tm := h.timeWorkload(w, mode)
		h.logf("TIMING %s %s mode=%s n=%d reduced=%v median=%.1fms p95=%.1fms max=%.1fms",
			phase, w.name, mode, tm.n, tm.reduced, ms(tm.median), ms(tm.p95), ms(tm.max))
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (h *evHarness) timeWorkload(w evWorkload, mode string) evTiming {
	h.t.Helper()
	c, err := h.pool.Acquire(h.ctx)
	if err != nil {
		h.t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	stmts := append([]string{`SET statement_timeout = 0`, fmt.Sprintf("SET plan_cache_mode = force_%s_plan", mode)}, h.sets...)
	for _, s := range stmts {
		if _, err := c.Exec(h.ctx, s, pgx.QueryExecModeSimpleProtocol); err != nil {
			h.t.Fatalf("%s: %v", s, err)
		}
	}
	// The connection returns to the pool; RESET ALL clears plan_cache_mode, statement_timeout and
	// the extra settings (it leaves pgx's client-side statement cache valid, unlike DISCARD ALL).
	defer func() { _, _ = c.Exec(context.Background(), `RESET ALL`, pgx.QueryExecModeSimpleProtocol) }()
	q := store.New(c)
	warm, n := 3, h.runs
	tm := evTiming{}
	for i := 0; i < warm; i++ {
		start := time.Now()
		if err := w.run(h.ctx, q); err != nil {
			h.t.Fatalf("%s warm-up: %v", w.name, err)
		}
		// A statement that already takes more than 8 s (the request budget) is measured with one
		// warm-up and at most 8 runs; the table flags it as reduced.
		if i == 0 && time.Since(start) > 8*time.Second {
			warm, n, tm.reduced = 1, min(n, 8), true
		}
	}
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := w.run(h.ctx, q); err != nil {
			h.t.Fatalf("%s: %v", w.name, err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	tm.n = len(durs)
	if tm.n%2 == 1 {
		tm.median = durs[tm.n/2]
	} else {
		tm.median = (durs[tm.n/2-1] + durs[tm.n/2]) / 2
	}
	tm.p95 = durs[int(math.Ceil(0.95*float64(tm.n)))-1]
	tm.max = durs[tm.n-1]
	return tm
}

// ---- EXPLAIN ----

type evScan struct{ loops, rows, ms float64 }

type evPlan struct {
	execMs, planMs        float64
	jitMs, jitFuncs       float64
	tempW, tempR          int64
	sharedHit, sharedRead int64
	generic, custom       int64
	scans                 map[string]*evScan
	lines, nlLines        []string
	indexNames            map[string]bool
	sortDisk              int
	totalMs               float64
}

func (p *evPlan) loopsOf(label string) float64 {
	if s := p.scans[label]; s != nil {
		return s.loops
	}
	return 0
}

func (p *evPlan) usesIndex(name string) bool { return p.indexNames[name] }

func (p *evPlan) scanLines() []string {
	keys := make([]string, 0, len(p.scans))
	for k := range p.scans {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		s := p.scans[k]
		out = append(out, fmt.Sprintf("%-70s loops=%.0f rows=%.0f time=%.1fms", k, s.loops, s.rows, s.ms))
	}
	return out
}

// explain PREPAREs st and runs EXPLAIN (ANALYZE, BUFFERS) EXECUTE under the given plan_cache_mode.
// A generic-mode plan is only accepted when pg_prepared_statements shows generic_plans >= 1.
func (h *evHarness) explain(mode string, st evStmt) *evPlan {
	h.t.Helper()
	h.simple(fmt.Sprintf("SET plan_cache_mode = force_%s_plan", mode))
	h.simple("DEALLOCATE ALL")
	h.simple("PREPARE evstmt AS " + st.sql)
	exec := "EXECUTE evstmt"
	if st.args != "" {
		exec += "(" + st.args + ")"
	}
	var raw []byte
	h.scalar("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+exec, &raw)
	p := &evPlan{scans: map[string]*evScan{}, indexNames: map[string]bool{}}
	h.scalar(`SELECT generic_plans, custom_plans FROM pg_prepared_statements WHERE name = 'evstmt'`, &p.generic, &p.custom)
	h.simple("DEALLOCATE ALL")
	h.simple("RESET plan_cache_mode")
	if mode == "generic" && p.generic < 1 {
		h.t.Fatalf("%s: generic_plans = %d; EXPLAIN did not run the generic plan", st.name, p.generic)
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		h.t.Fatalf("%s: decode EXPLAIN JSON: %v", st.name, err)
	}
	p.execMs, _ = doc[0]["Execution Time"].(float64)
	p.planMs, _ = doc[0]["Planning Time"].(float64)
	if jit, ok := doc[0]["JIT"].(map[string]any); ok {
		p.jitFuncs = evNum(jit, "Functions")
		if tm, ok := jit["Timing"].(map[string]any); ok {
			p.jitMs = evNum(tm, "Total")
		}
	}
	root, _ := doc[0]["Plan"].(map[string]any)
	p.totalMs = p.execMs
	p.sharedHit, p.sharedRead = int64(evNum(root, "Shared Hit Blocks")), int64(evNum(root, "Shared Read Blocks"))
	p.walk(root, 0)
	return p
}

func evNum(n map[string]any, k string) float64 { f, _ := n[k].(float64); return f }

func evStr(n map[string]any, k string) string { s, _ := n[k].(string); return s }

func evKids(n map[string]any) []map[string]any {
	raw, _ := n["Plans"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, k := range raw {
		if m, ok := k.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func evLabel(n map[string]any) string {
	l := evStr(n, "Node Type")
	if jt := evStr(n, "Join Type"); jt != "" && strings.Contains(l, "Join") {
		l += " (" + jt + ")"
	}
	if ix := evStr(n, "Index Name"); ix != "" {
		l += " using " + ix
	}
	if rel := evStr(n, "Relation Name"); rel != "" {
		l += " on " + rel
	}
	if cte := evStr(n, "CTE Name"); cte != "" {
		l += " " + cte
	}
	if sn := evStr(n, "Subplan Name"); sn != "" {
		l += " [" + sn + "]"
	}
	return l
}

func (p *evPlan) walk(n map[string]any, depth int) {
	loops := evNum(n, "Actual Loops")
	rows := evNum(n, "Actual Rows") * loops
	timeMs := evNum(n, "Actual Total Time") * loops
	label := evLabel(n)
	tw := int64(evNum(n, "Temp Written Blocks"))
	if tw > p.tempW {
		p.tempW = tw
	}
	if tr := int64(evNum(n, "Temp Read Blocks")); tr > p.tempR {
		p.tempR = tr
	}
	if evStr(n, "Sort Space Type") == "Disk" {
		p.sortDisk++
	}
	nodeType := evStr(n, "Node Type")
	isScan := evStr(n, "Relation Name") != "" || evStr(n, "Index Name") != ""
	if ix := evStr(n, "Index Name"); ix != "" {
		p.indexNames[ix] = true
	}
	if isScan {
		s := p.scans[label]
		if s == nil {
			s = &evScan{}
			p.scans[label] = s
		}
		s.loops += loops
		s.rows += rows
		s.ms += timeMs
	}
	isJoin := strings.Contains(nodeType, "Join") || nodeType == "Nested Loop"
	interesting := isScan || isJoin || nodeType == "CTE Scan" || nodeType == "WindowAgg" ||
		tw > 0 || (p.totalMs > 0 && timeMs >= 0.05*p.totalMs)
	if interesting {
		spill := ""
		if tw > 0 {
			spill = fmt.Sprintf(" temp_written=%d", tw)
		}
		if sm := evStr(n, "Sort Method"); sm != "" {
			spill += " sort=" + sm + "/" + evStr(n, "Sort Space Type")
		}
		p.lines = append(p.lines, fmt.Sprintf("%s%s rows=%.0f loops=%.0f time=%.1fms%s", strings.Repeat("  ", min(depth, 24)), label, rows, loops, timeMs, spill))
	}
	kids := evKids(n)
	if nodeType == "Nested Loop" && len(kids) == 2 {
		inner := kids[1]
		il := evNum(inner, "Actual Loops")
		if il >= 1000 || rows >= 50000 {
			p.nlLines = append(p.nlLines, fmt.Sprintf("%s: outer=%s (rows %.0f) inner=%s (loops %.0f, rows/loop %.1f) total_rows=%.0f time=%.1fms",
				evLabel(n), evLabel(kids[0]), evNum(kids[0], "Actual Rows")*evNum(kids[0], "Actual Loops"), evLabel(inner), il, evNum(inner, "Actual Rows"), rows, timeMs))
		}
	}
	for _, k := range kids {
		p.walk(k, depth+1)
	}
}
