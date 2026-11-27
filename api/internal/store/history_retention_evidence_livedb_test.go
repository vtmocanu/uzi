//go:build historyretentionevidence

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/historyretention"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/sweeper"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4695 (M2): opt-in measurement harness for the time indexes of the four history tables
// the history_retention pass trims. It is NOT part of any gate: the build tag keeps it out of
// every default build and CI run, and it also needs UZI_HISTORY_RETENTION_EVIDENCE=1.
//
// It runs against a throwaway PostgreSQL (the e2e/test-database-marker.sql marker must be present
// in the database UZI_TEST_DATABASE_URL names) and keeps its own persistent database "hrev" on that
// server, so the same fixture survives a container restart (the cold phase). Phases are separate
// tests so each command stays inside a time cap:
//
//	SeedEvidenceLiveDB       create and migrate hrev, seed the four tables, report sizes
//	WarmEvidenceLiveDB       cases (c) no-op, (a) ~2% expired, (b) 90% backlog per candidate index
//	CompressedEvidenceLiveDB compressed-time drain runs (mode backlog = case b, mode churn = case d)
//	SmokeEvidenceLiveDB      the production historyretention.Pass under a real sweeper.Engine
//	ColdEvidenceLiveDB       fixture setup, or the first tick after a container restart
//
// Environment (all optional unless marked):
//
//	UZI_HISTORY_RETENTION_EVIDENCE=1   required
//	UZI_EVIDENCE_LOG                   required, absolute path, JSON lines are appended
//	UZI_TEST_DATABASE_URL              required
//	UZI_EVIDENCE_ROWS                  rows per table (default 1000000)
//	UZI_EVIDENCE_TABLES                comma list of pr,anthropic,codex,worker (default all four)
//	UZI_EVIDENCE_CODEX                 typical (default) or max: the codex jsonb size
//	UZI_EVIDENCE_CANDS                 comma list of none,btree,brin_off,brin_on
//	UZI_EVIDENCE_MODE                  backlog or churn (Compressed)
//	UZI_EVIDENCE_TICKS                 compressed ticks (default 1100 backlog, 4400 churn)
//	UZI_EVIDENCE_PR_ARRIVALS           pr_history_events arrivals per 15 s tick (default 6)
//	UZI_EVIDENCE_MAX_MINUTES           wall-clock cap of one compressed run (default 20)
//	UZI_EVIDENCE_SMOKE_INDEXES         pr=btree,anthropic=btree,codex=btree,worker=btree (Smoke)
//	UZI_EVIDENCE_SMOKE_SECONDS         smoke length (default 300)
//	UZI_EVIDENCE_COLD_TABLE / _CASE / _SETUP / _LABEL   see ColdEvidenceLiveDB
//
// Measurements use the production session (store.OpenHistoryRetentionSession, DeleteBatch, a fresh
// connection per tick, 300 ms statement timeout and 200 ms lock timeout). Run without -race.

const (
	heDBName      = "hrev"
	heTick        = 15 * time.Second
	heStmtTimeout = 300 * time.Millisecond
	heLockTimeout = 200 * time.Millisecond
	heMaxBatch    = int32(1000)
	heMinBatch    = int32(50)
	heSampleArr   = 250
	heMarker      = "recovery-reset-v1"
)

// heBase is bucket-aligned (day-aligned) and is the fixed start of every warm and cold fixture.
var heBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

var heCands = []string{"none", "btree", "brin_off", "brin_on"}

// ---------------------------------------------------------------------------------------------
// environment, log and fixture plumbing

type heLog struct {
	t    *testing.T
	path string
	mu   sync.Mutex
}

func (l *heLog) rec(ev string, kv map[string]any) {
	l.t.Helper()
	if kv == nil {
		kv = map[string]any{}
	}
	kv["ev"] = ev
	kv["ts"] = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(kv)
	if err != nil {
		l.t.Fatalf("evidence log marshal: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// The path is the operator-set absolute UZI_EVIDENCE_LOG of an opt-in, build-tagged harness.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: operator-set absolute path
	if err != nil {
		l.t.Fatalf("evidence log: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		l.t.Fatalf("evidence log write: %v", err)
	}
}

func heEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			panic(fmt.Sprintf("%s=%q: %v", name, v, err))
		}
		return n
	}
	return def
}

func heList(name string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	return strings.Split(v, ",")
}

type heFx struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	dsn    string
	log    *heLog
	repo   uuid.UUID
	user   uuid.UUID
	codex  string
	tables []store.HistoryRetentionTable
	// version is the server_version read on the marker check's own backend by requireEvidenceMarker.
	version string
}

func heTableByShort(s string) store.HistoryRetentionTable {
	switch s {
	case "pr":
		return store.HistoryRetentionPRHistoryEvents
	case "anthropic":
		return store.HistoryRetentionAnthropicUsage
	case "codex":
		return store.HistoryRetentionCodexUsage
	case "worker":
		return store.HistoryRetentionWorkerCapacitySample
	}
	panic("unknown table " + s)
}

func heShort(t store.HistoryRetentionTable) string {
	switch t {
	case store.HistoryRetentionPRHistoryEvents:
		return "pr"
	case store.HistoryRetentionAnthropicUsage:
		return "anthropic"
	case store.HistoryRetentionCodexUsage:
		return "codex"
	}
	return "worker"
}

func heRows() int { return heEnvInt("UZI_EVIDENCE_ROWS", 1_000_000) }

// newHeFx connects to the evidence database. The marker of the database UZI_TEST_DATABASE_URL names
// is checked first (admin paths); the evidence database hrev is created with its own marker row
// (the e2e/test-database-marker.sql shape) and EVERY path, including the cold one, verifies that
// hrev itself carries the marker before anything mutates it, so a hrev that something else made
// is refused. With admin=true it then migrates hrev; with admin=false (the cold tick) it opens the
// pool and nothing else (the marker check ran on a separate pool of its own), so no query touches
// the history tables, and none runs on fx.pool, before the first measured tick.
func newHeFx(t *testing.T, admin bool) *heFx {
	t.Helper()
	if os.Getenv("UZI_HISTORY_RETENTION_EVIDENCE") != "1" {
		t.Skip("UZI_HISTORY_RETENTION_EVIDENCE=1 not set; opt-in evidence harness")
	}
	logPath := os.Getenv("UZI_EVIDENCE_LOG")
	if !filepath.IsAbs(logPath) {
		t.Fatalf("UZI_EVIDENCE_LOG must be an absolute path, got %q", logPath)
	}
	base := os.Getenv("UZI_TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + heDBName
	fx := &heFx{t: t, ctx: ctx, dsn: u.String(), log: &heLog{t: t, path: logPath}, codex: "typical"}
	if v := os.Getenv("UZI_EVIDENCE_CODEX"); v == "max" {
		fx.codex = "max"
	}
	for _, s := range heList("UZI_EVIDENCE_TABLES", []string{"pr", "anthropic", "codex", "worker"}) {
		fx.tables = append(fx.tables, heTableByShort(s))
	}
	if admin {
		ap, err := pgxpool.New(ctx, base)
		if err != nil {
			t.Fatalf("admin pool: %v", err)
		}
		defer ap.Close()
		var marker string
		if err := ap.QueryRow(ctx, `SELECT marker FROM uzi_test_harness.database_marker`).Scan(&marker); err != nil || marker != heMarker {
			t.Fatalf("UZI_TEST_DATABASE_URL does not name a throwaway marker database (marker=%q err=%v)", marker, err)
		}
		var exists bool
		if err := ap.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, heDBName).Scan(&exists); err != nil {
			t.Fatalf("pg_database: %v", err)
		}
		if !exists {
			if _, err := ap.Exec(ctx, `CREATE DATABASE `+heDBName); err != nil {
				t.Fatalf("create database: %v", err)
			}
			fx.markEvidenceDB()
		}
	}
	// Before ANY mutation of hrev (migrate, truncate, seed, delete, ALTER), on every path.
	fx.requireEvidenceMarker()
	if admin {
		if err := store.Migrate(ctx, fx.dsn); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	pool, err := store.OpenPool(ctx, fx.dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	fx.pool = pool
	fx.log.rec("env", map[string]any{"server_version": fx.serverVersion(), "rows": heRows(), "diagnostic_overrides": heDiagnosticReasons(fx, false)})
	if admin {
		fx.ensureOwner()
	}
	return fx
}

// markEvidenceDB writes the throwaway-database marker row into the freshly created hrev, in the
// shape of e2e/test-database-marker.sql.
func (fx *heFx) markEvidenceDB() {
	fx.t.Helper()
	p, err := pgxpool.New(fx.ctx, fx.dsn)
	if err != nil {
		fx.t.Fatalf("hrev marker pool: %v", err)
	}
	defer p.Close()
	for _, q := range []string{
		`CREATE SCHEMA uzi_test_harness`,
		`CREATE TABLE uzi_test_harness.database_marker (marker text PRIMARY KEY)`,
		`INSERT INTO uzi_test_harness.database_marker (marker) VALUES ('` + heMarker + `')`,
	} {
		if _, err := p.Exec(fx.ctx, q); err != nil {
			fx.t.Fatalf("write hrev marker: %v\n%s", err, q)
		}
	}
}

// requireEvidenceMarker refuses (t.Fatalf) unless hrev itself carries the throwaway marker. It is a
// read-only query over a connection of its own, so nothing is mutated on a refusal.
func (fx *heFx) requireEvidenceMarker() {
	fx.t.Helper()
	p, err := pgxpool.New(fx.ctx, fx.dsn)
	if err != nil {
		fx.t.Fatalf("refusing to touch %s: cannot connect to verify its marker: %v", heDBName, err)
	}
	defer p.Close()
	var marker string
	if err := p.QueryRow(fx.ctx, `SELECT marker FROM uzi_test_harness.database_marker`).Scan(&marker); err != nil || marker != heMarker {
		fx.t.Fatalf("refusing to touch database %s: it does not carry the throwaway marker (marker=%q err=%v); drop it so the harness can recreate it", heDBName, marker, err)
	}
	// server_version is read here, on the marker check's own backend, so fx.pool never runs it.
	if err := p.QueryRow(fx.ctx, `SHOW server_version`).Scan(&fx.version); err != nil {
		fx.t.Fatalf("SHOW server_version: %v", err)
	}
}

// serverVersion returns the version read by requireEvidenceMarker; it runs no query.
func (fx *heFx) serverVersion() string { return fx.version }

// heDiagnosticReasons lists every override that makes a run diagnostic only: fewer rows than the
// 1,000,000 of the evidence fixture, a smoke shorter than 300 s, a table or candidate selector
// that narrows coverage, or UZI_EVIDENCE_NO_AV=1 (autovacuum off during a compressed run). Empty
// means the run is qualifying. forSmoke counts only the overrides that affect the smoke: the
// candidate selector and the autovacuum switch only apply to the compressed runs.
func heDiagnosticReasons(fx *heFx, forSmoke bool) []string {
	var r []string
	if n := heRows(); n < 1_000_000 {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_ROWS=%d (below 1000000)", n))
	}
	if n := heEnvInt("UZI_EVIDENCE_SMOKE_SECONDS", 300); n < 300 {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_SMOKE_SECONDS=%d (below 300)", n))
	}
	if len(fx.tables) < len(store.HistoryRetentionTables()) {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_TABLES selects %d of %d tables", len(fx.tables), len(store.HistoryRetentionTables())))
	}
	if forSmoke {
		return r
	}
	if c := heList("UZI_EVIDENCE_CANDS", heCands); len(c) < len(heCands) {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_CANDS selects %d of %d candidates", len(c), len(heCands)))
	}
	if os.Getenv("UZI_EVIDENCE_NO_AV") == "1" {
		r = append(r, "UZI_EVIDENCE_NO_AV=1 (autovacuum off during compressed runs)")
	}
	return r
}

func (fx *heFx) exec(sql string, args ...any) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
		fx.t.Fatalf("SQL: %v\n%s", err, sql)
	}
}

func (fx *heFx) one(sql string, args ...any) (v int64) {
	fx.t.Helper()
	if err := fx.pool.QueryRow(fx.ctx, sql, args...).Scan(&v); err != nil {
		fx.t.Fatalf("SQL: %v\n%s", err, sql)
	}
	return v
}

// ensureOwner finds or creates the one user and repo every seeded row hangs off.
func (fx *heFx) ensureOwner() {
	fx.t.Helper()
	err := fx.pool.QueryRow(fx.ctx, `SELECT u.id, r.id FROM users u JOIN forge_connections c ON c.user_id = u.id
	        JOIN repos r ON r.connection_id = c.id WHERE u.email = 'hrev@e2e' LIMIT 1`).Scan(&fx.user, &fx.repo)
	if err == nil {
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		fx.t.Fatalf("owner lookup: %v", err)
	}
	fx.user, fx.repo = uuid.New(), uuid.New()
	conn := uuid.New()
	fx.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, 'hrev@e2e', 'x')`, fx.user)
	fx.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
	 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, fx.user, []byte{1})
	fx.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
	 VALUES ($1,$2,1,'g/hrev','https://forge.e2e/g/hrev','main',true)`, fx.repo, conn)
}

// ---------------------------------------------------------------------------------------------
// fixture specification and seeding

// heSpec is one table's fixture: n time-ordered rows, row g observed at start + g*step, where step
// is 15 s divided by the arrivals per tick, so the table holds n/arrivals ticks of history.
type heSpec struct {
	table    store.HistoryRetentionTable
	n        int
	arrivals int
	start    time.Time
}

func (s heSpec) stepUS() int64 { return int64(heTick/time.Microsecond) / int64(s.arrivals) }
func (s heSpec) at(g int64) time.Time {
	return s.start.Add(time.Duration(g*s.stepUS()) * time.Microsecond)
}
func (s heSpec) frac(f float64) time.Time {
	return s.at(int64(f * float64(s.n)))
}

// perBucket is the number of rows per five-minute bucket (the identity count of a sample table).
func (s heSpec) perBucket() int64 { return int64(300*time.Second/time.Microsecond) / s.stepUS() }

func (fx *heFx) specFor(table store.HistoryRetentionTable, arrivals int, start time.Time) heSpec {
	return heSpec{table: table, n: heRows(), arrivals: arrivals, start: start}
}

func (fx *heFx) prArrivals() int { return heEnvInt("UZI_EVIDENCE_PR_ARRIVALS", 6) }

func (fx *heFx) stdSpec(table store.HistoryRetentionTable) heSpec {
	a := heSampleArr
	if table == store.HistoryRetentionPRHistoryEvents {
		a = fx.prArrivals()
	}
	return fx.specFor(table, a, heBase)
}

// codexBucketsSQL builds the jsonb projection: two buckets (typical) or 17 buckets with long ids
// (largest admissible, 4 KiB bound enforced by the table CHECK).
func heCodexBucketsSQL(variant string) string {
	win := func(pct, secs, reset string) string {
		return `jsonb_build_object('u', ` + pct + `, 'w', ` + secs + `, 'r', ` + reset + `)`
	}
	if variant == "max" {
		// 17 buckets with 99-byte high-entropy ids: the largest admissible shape (about 3.8 KB of text).
		return `(SELECT jsonb_agg(jsonb_build_object('id', left(md5(g::text || k::text) || md5(k::text || g::text) || md5(g::text) || md5(k::text), 99),
		            'al', true, 'lr', false,
		            'p', ` + win(`(g + k) % 100`, `18000`, `1760000000 + (g + k) % 100000`) + `,
		            's', ` + win(`(g * 3 + k) % 100`, `604800`, `1760300000 + (g + k) % 100000`) + `))
		        FROM generate_series(1, 17) k)`
	}
	return `jsonb_build_array(
	    jsonb_build_object('id', 'codex-' || left(md5((g % 5000)::text), 36), 'al', true, 'lr', false,
	        'p', ` + win(`g % 100`, `18000`, `1760000000 + g % 100000`) + `,
	        's', ` + win(`(g / 7) % 100`, `604800`, `1760300000 + g % 100000`) + `),
	    jsonb_build_object('id', 'codex-spark-' || left(md5((g % 5000)::text), 36), 'al', true, 'lr', false,
	        'p', ` + win(`(g / 3) % 100`, `18000`, `1760000000 + g % 100000`) + `,
	        's', ` + win(`(g / 11) % 100`, `604800`, `1760300000 + g % 100000`) + `))`
}

// insertSQL returns the INSERT of rows g in [$1, $2) of spec s (start $3, step_us $4, identities $5).
func (fx *heFx) insertSQL(table store.HistoryRetentionTable) string {
	ts := `(x.t)`
	from := ` FROM generate_series($1::bigint, $2::bigint - 1) g,
	        LATERAL (SELECT $3::timestamptz + interval '1 microsecond' * (g * $4::bigint) AS t) x`
	switch table {
	case store.HistoryRetentionPRHistoryEvents:
		return `INSERT INTO pr_history_events (repo_id, mr_iid, from_state, to_state, time_source, observed_at, bootstrap, first_merge)
		 SELECT '` + fx.repo.String() + `'::uuid, g / 3,
		        CASE g % 3 WHEN 0 THEN NULL ELSE 'opened' END,
		        CASE g % 3 WHEN 0 THEN 'opened' WHEN 1 THEN 'closed' ELSE 'merged' END,
		        'observation_fallback', ` + ts + `, g % 3 = 0, (g % 3 = 2 AND (g / 3) % 30 = 0)` + from
	case store.HistoryRetentionAnthropicUsage:
		return `INSERT INTO anthropic_usage_samples (user_id, user_secret_id, observed_at, enablement_rev, source,
		        five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at)
		 SELECT '` + fx.user.String() + `'::uuid, md5('a' || (g % $5::bigint)::text)::uuid, ` + ts + `, 1, 'usage_endpoint',
		        (g % 100)::smallint, ` + ts + ` + interval '5 hours', ((g / 7) % 100)::smallint, ` + ts + ` + interval '7 days'` + from
	case store.HistoryRetentionCodexUsage:
		return `INSERT INTO codex_usage_samples (user_id, provider_account_id, observed_at, observed_generation,
		        observed_credential_revision, buckets)
		 SELECT '` + fx.user.String() + `'::uuid, md5('c' || (g % $5::bigint)::text)::uuid, ` + ts + `, 1, 1,
		        ` + heCodexBucketsSQL(fx.codex) + from
	}
	return `INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, last_heartbeat_at,
	        worker_kind, ephemeral, isolated_lane, max_concurrent_runs, max_cross_check_slots, attribution,
	        ordinary_active, cross_check_lane_active, chat_active)
	 SELECT md5('w' || (g % $5::bigint)::text)::uuid, date_bin('5 minutes', ` + ts + `, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
	        '` + fx.user.String() + `'::uuid, ` + ts + `, 'online', ` + ts + `, 'external', false, false, 4, 2, 'owner',
	        (g % 5)::int, 0, 0` + from
}

// insertRows inserts rows [from, to) of spec s.
func (fx *heFx) insertRows(s heSpec, from, to int64) {
	fx.t.Helper()
	if s.table == store.HistoryRetentionPRHistoryEvents { // its statement has no identity parameter
		fx.exec(fx.insertSQL(s.table), from, to, s.start, s.stepUS())
		return
	}
	fx.exec(fx.insertSQL(s.table), from, to, s.start, s.stepUS(), s.perBucket())
}

// reseed empties the table and loads spec s, then VACUUM ANALYZE so the heap, visibility map and
// statistics are in the state a settled table has.
func (fx *heFx) reseed(s heSpec) time.Duration {
	fx.t.Helper()
	t0 := time.Now()
	// An interrupted compressed run with UZI_EVIDENCE_NO_AV=1 can leave the table with autovacuum
	// disabled; every fixture starts from the default.
	fx.exec(`ALTER TABLE ` + string(s.table) + ` RESET (autovacuum_enabled)`)
	fx.exec(`TRUNCATE ` + string(s.table))
	for from := int64(0); from < int64(s.n); from += 200_000 {
		fx.insertRows(s, from, min(from+200_000, int64(s.n)))
	}
	fx.exec(`VACUUM (ANALYZE) ` + string(s.table))
	return time.Since(t0)
}

// ---------------------------------------------------------------------------------------------
// candidate indexes and sizes

// heShipped is the btree the migrations 00549 to 00552 create on each table. The btree candidate IS
// this index (same name, same definition), so the database migrated by newHeFx already carries
// it; the BRIN candidates and none are the earlier selection evidence and drop it first.
var heShipped = map[store.HistoryRetentionTable]string{
	store.HistoryRetentionPRHistoryEvents:      "pr_history_events_observed_at_idx",
	store.HistoryRetentionAnthropicUsage:       "anthropic_usage_samples_bucket_start_idx",
	store.HistoryRetentionCodexUsage:           "codex_usage_samples_bucket_start_idx",
	store.HistoryRetentionWorkerCapacitySample: "worker_capacity_samples_bucket_start_idx",
}

func heIdxName(table store.HistoryRetentionTable, cand string) string {
	if cand == "btree" {
		return heShipped[table]
	}
	return "hrev_" + heShort(table) + "_" + cand
}

func heExpiryCol(table store.HistoryRetentionTable) string { return hrExpiryColumn(table) }

func heIdxDDL(table store.HistoryRetentionTable, cand string) string {
	col, name := heExpiryCol(table), heIdxName(table, cand)
	switch cand {
	case "btree":
		if table == store.HistoryRetentionPRHistoryEvents {
			return `CREATE INDEX ` + name + ` ON ` + string(table) + ` (observed_at) WHERE NOT first_merge`
		}
		return `CREATE INDEX ` + name + ` ON ` + string(table) + ` (` + col + `)`
	case "brin_off":
		return `CREATE INDEX ` + name + ` ON ` + string(table) + ` USING brin (` + col + `) WITH (autosummarize = off)`
	case "brin_on":
		return `CREATE INDEX ` + name + ` ON ` + string(table) + ` USING brin (` + col + `) WITH (autosummarize = on)`
	}
	return ""
}

// setIndex leaves exactly cand on the table (none leaves no index) and returns the build time. For
// btree it keeps the migration-created index when it exists and is valid (build time 0) and
// builds it from the same definition otherwise; every other candidate drops it first.
func (fx *heFx) setIndex(table store.HistoryRetentionTable, cand string) time.Duration {
	fx.t.Helper()
	valid := func(name string) bool {
		var ok bool
		_ = fx.pool.QueryRow(fx.ctx, `SELECT coalesce((SELECT i.indisvalid AND i.indisready FROM pg_index i
		        WHERE i.indexrelid = to_regclass($1)), false)`, name).Scan(&ok)
		return ok
	}
	for _, c := range heCands {
		if c == "none" || (c == "btree" && cand == "btree" && valid(heIdxName(table, c))) {
			continue
		}
		fx.exec(`DROP INDEX IF EXISTS ` + heIdxName(table, c))
	}
	t0 := time.Now()
	if cand != "none" && !valid(heIdxName(table, cand)) {
		fx.exec(heIdxDDL(table, cand))
	}
	d := time.Since(t0)
	fx.exec(`ANALYZE ` + string(table))
	return d
}

type heSizes struct {
	Heap, Idx, Toast, PK, Total int64
	Rows                        int64
}

func (fx *heFx) sizes(table store.HistoryRetentionTable, cand string) heSizes {
	fx.t.Helper()
	var s heSizes
	if err := fx.pool.QueryRow(fx.ctx, `SELECT pg_relation_size(c.oid), pg_total_relation_size(c.oid),
	        CASE WHEN c.reltoastrelid <> 0 THEN pg_total_relation_size(c.reltoastrelid) ELSE 0 END
	        FROM pg_class c WHERE c.oid = $1::regclass`, string(table)).Scan(&s.Heap, &s.Total, &s.Toast); err != nil {
		fx.t.Fatalf("sizes: %v", err)
	}
	if cand != "none" {
		_ = fx.pool.QueryRow(fx.ctx, `SELECT coalesce(pg_relation_size(to_regclass($1)), 0)`, heIdxName(table, cand)).Scan(&s.Idx)
	}
	s.PK = s.Total - s.Heap - s.Toast - s.Idx
	s.Rows = fx.one(`SELECT count(*) FROM ` + string(table))
	return s
}

func (s heSizes) kv() map[string]any {
	return map[string]any{"heap_bytes": s.Heap, "cand_idx_bytes": s.Idx, "toast_bytes": s.Toast,
		"other_idx_bytes": s.PK, "total_bytes": s.Total, "rows": s.Rows}
}

// ---------------------------------------------------------------------------------------------
// ticks

type heTickRes struct {
	Table   store.HistoryRetentionTable
	OpenMS  float64
	DelMS   float64
	CloseMS float64
	N       int64
	Batch   int32
	Err     string // "", "57014", "55P03", or "unknown: ..."
}

func heMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// oneTick opens a fresh production session (the pass opens one per tick), runs one DeleteBatch and
// closes it, under the same client deadlines as the pass (2 s total, work context 250 ms shorter).
func (fx *heFx) oneTick(table store.HistoryRetentionTable, cutoff time.Time, batch int32) heTickRes {
	fx.t.Helper()
	start := time.Now()
	cleanupCtx, cancelC := context.WithDeadline(fx.ctx, start.Add(2*time.Second))
	defer cancelC()
	workCtx, cancelW := context.WithDeadline(cleanupCtx, start.Add(1750*time.Millisecond))
	defer cancelW()
	r := heTickRes{Table: table, Batch: batch}
	sess, ok, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, fx.pool, heStmtTimeout, heLockTimeout)
	r.OpenMS = heMS(time.Since(start))
	if err != nil || !ok {
		r.Err = fmt.Sprintf("unknown: open ok=%v err=%v", ok, err)
		return r
	}
	r.N, r.Err = fx.deleteOn(workCtx, sess, table, cutoff, batch, &r)
	t1 := time.Now()
	if _, err := sess.Close(cleanupCtx); err != nil && r.Err == "" {
		r.Err = "unknown: close: " + err.Error()
	}
	r.CloseMS = heMS(time.Since(t1))
	return r
}

func (fx *heFx) deleteOn(ctx context.Context, sess *store.HistoryRetentionSession, table store.HistoryRetentionTable,
	cutoff time.Time, batch int32, r *heTickRes) (int64, string) {
	t0 := time.Now()
	n, err := sess.DeleteBatch(ctx, table, cutoff, batch)
	r.DelMS = heMS(time.Since(t0))
	if err == nil {
		return n, ""
	}
	var rb *store.HistoryRetentionRollbackError
	if errors.As(err, &rb) {
		return 0, rb.Code
	}
	return 0, "unknown: " + err.Error()
}

func heQuant(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

func heMaxF(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func heRound(xs []float64) []float64 {
	o := make([]float64, len(xs))
	for i, x := range xs {
		o[i] = float64(int64(x*10+0.5)) / 10
	}
	return o
}

// ---------------------------------------------------------------------------------------------
// EXPLAIN (ANALYZE, BUFFERS) of the production statement, rolled back

type heExplain struct {
	Plan          []string
	Hit, Read     int64
	ExecMS        float64
	Dirtied, Wrte int64
	StatIO        map[string]int64
}

func heParseBuffers(line string) (hit, read, dirt, wr int64) {
	i := strings.Index(line, "Buffers:")
	if i < 0 {
		return
	}
	for _, f := range strings.Fields(line[i+len("Buffers:"):]) {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(kv[1], 10, 64)
		if err != nil {
			continue
		}
		switch kv[0] {
		case "hit":
			hit = n
		case "read":
			read = n
		case "dirtied":
			dirt = n
		case "written":
			wr = n
		}
	}
	return
}

func (fx *heFx) statio(conn *pgxpool.Conn, table store.HistoryRetentionTable) map[string]int64 {
	fx.t.Helper()
	_, _ = conn.Exec(fx.ctx, `SELECT pg_stat_force_next_flush()`)
	cols := []string{"heap_blks_read", "heap_blks_hit", "idx_blks_read", "idx_blks_hit", "toast_blks_read", "toast_blks_hit", "tidx_blks_read", "tidx_blks_hit"}
	vals := make([]int64, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	row := conn.QueryRow(fx.ctx, `SELECT coalesce(heap_blks_read,0), coalesce(heap_blks_hit,0), coalesce(idx_blks_read,0), coalesce(idx_blks_hit,0),
	        coalesce(toast_blks_read,0), coalesce(toast_blks_hit,0), coalesce(tidx_blks_read,0), coalesce(tidx_blks_hit,0)
	        FROM pg_statio_user_tables WHERE relname = $1`, string(table))
	if err := row.Scan(ptrs...); err != nil {
		return map[string]int64{}
	}
	m := map[string]int64{}
	for i, c := range cols {
		m[c] = vals[i]
	}
	return m
}

// explain runs the production DELETE of one batch under EXPLAIN (ANALYZE, BUFFERS) with a custom
// plan (a fresh session's first execution is always planned for its parameters) inside a
// transaction that is rolled back, so the fixture is unchanged. Per-relation block counters
// (heap, index, TOAST) come from pg_statio_user_tables before and after.
func (fx *heFx) explain(table store.HistoryRetentionTable, cutoff time.Time, batch int32) heExplain {
	fx.t.Helper()
	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		fx.t.Fatalf("explain acquire: %v", err)
	}
	defer conn.Release()
	var ex heExplain
	before := fx.statio(conn, table)
	mustExec := func(sql string) {
		if _, err := conn.Exec(fx.ctx, sql); err != nil {
			fx.t.Fatalf("explain setup: %v\n%s", err, sql)
		}
	}
	mustExec(`BEGIN`)
	mustExec(`SET LOCAL plan_cache_mode = force_custom_plan`)
	mustExec(`SET LOCAL statement_timeout = 0`)
	mustExec(`PREPARE hrev_p(timestamptz, int) AS ` + store.HistoryRetentionDeleteSQLForTest(table))
	rows, err := conn.Query(fx.ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, BUFFERS) EXECUTE hrev_p('%s'::timestamptz, %d)`,
		cutoff.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), batch))
	if err != nil {
		fx.t.Fatalf("explain: %v", err)
	}
	seenTop := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			fx.t.Fatalf("explain scan: %v", err)
		}
		ex.Plan = append(ex.Plan, line)
		if !seenTop && strings.Contains(line, "Buffers:") {
			seenTop = true
			ex.Hit, ex.Read, ex.Dirtied, ex.Wrte = heParseBuffers(line)
		}
		if strings.HasPrefix(line, "Execution Time:") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				ex.ExecMS, _ = strconv.ParseFloat(f[2], 64)
			}
		}
	}
	rows.Close()
	mustExec(`ROLLBACK`)
	mustExec(`DEALLOCATE hrev_p`)
	after := fx.statio(conn, table)
	ex.StatIO = map[string]int64{}
	for k, v := range after {
		ex.StatIO[k] = v - before[k]
	}
	return ex
}

func (e heExplain) kv() map[string]any {
	return map[string]any{"buf_hit": e.Hit, "buf_read": e.Read, "buf_dirtied": e.Dirtied, "buf_written": e.Wrte,
		"explain_exec_ms": e.ExecMS, "statio_delta": e.StatIO, "plan": e.Plan}
}

// ---------------------------------------------------------------------------------------------
// warm cases (a), (b), (c)

type heCase struct {
	name   string
	frac   float64 // fraction of the fixture expired; 0 means a cutoff before every row
	ticks  int
	commit bool
}

var heWarmCases = []heCase{{"c", 0, 25, false}, {"a", 0.0205, 21, true}, {"b", 0.90, 40, true}}

func (fx *heFx) tableLabel(table store.HistoryRetentionTable) string {
	if table == store.HistoryRetentionCodexUsage {
		return string(table) + "/" + fx.codex
	}
	return string(table)
}

func (fx *heFx) runCase(spec heSpec, cand string, c heCase) {
	fx.t.Helper()
	cutoff := spec.start.Add(-time.Hour)
	if c.frac > 0 {
		cutoff = spec.frac(c.frac)
	}
	col := heExpiryCol(spec.table)
	where := col + ` < $1`
	if spec.table == store.HistoryRetentionPRHistoryEvents {
		where += ` AND NOT first_merge`
	}
	expired := fx.one(`SELECT count(*) FROM `+string(spec.table)+` WHERE `+where, cutoff)
	before := fx.one(`SELECT count(*) FROM ` + string(spec.table))
	ex := fx.explain(spec.table, cutoff, heMaxBatch)
	var del, open, closeMS []float64
	var deleted int64
	errs := map[string]int{}
	perTick := []int64{}
	for i := 0; i < c.ticks; i++ {
		r := fx.oneTick(spec.table, cutoff, heMaxBatch)
		del = append(del, r.DelMS)
		open = append(open, r.OpenMS)
		closeMS = append(closeMS, r.CloseMS)
		deleted += r.N
		perTick = append(perTick, r.N)
		if r.Err != "" {
			errs[strings.SplitN(r.Err, ":", 2)[0]]++
		}
	}
	after := fx.one(`SELECT count(*) FROM ` + string(spec.table))
	kv := map[string]any{"table": fx.tableLabel(spec.table), "cand": cand, "case": c.name, "ticks": c.ticks,
		"expired_before": expired, "count_before": before, "count_after": after, "confirmed_deleted": deleted,
		"median_ms": heQuant(del, 0.5), "p95_ms": heQuant(del, 0.95), "max_ms": heMaxF(del), "first_ms": del[0],
		"open_median_ms": heQuant(open, 0.5), "close_median_ms": heQuant(closeMS, 0.5), "errors": errs,
		"tick_ms": heRound(del), "tick_deleted": perTick}
	for k, v := range ex.kv() {
		kv[k] = v
	}
	fx.log.rec("warm", kv)
	fx.t.Logf("warm %s %s case %s: median %.1f ms max %.1f ms deleted %d (count %d -> %d) errs %v buffers hit=%d read=%d",
		fx.tableLabel(spec.table), cand, c.name, heQuant(del, 0.5), heMaxF(del), deleted, before, after, errs, ex.Hit, ex.Read)
}

// TestHistoryRetentionSeedEvidenceLiveDB creates and migrates the evidence database, seeds each
// selected table with the standard fixture and reports bytes per row and sizes.
func TestHistoryRetentionSeedEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	for _, table := range fx.tables {
		spec := fx.stdSpec(table)
		d := fx.reseed(spec)
		s := fx.sizes(table, "none")
		kv := s.kv()
		kv["table"], kv["seed_seconds"] = fx.tableLabel(table), d.Seconds()
		kv["bytes_per_row_heap_toast"] = float64(s.Heap+s.Toast) / float64(max(s.Rows, 1))
		kv["bytes_per_row_total"] = float64(s.Total) / float64(max(s.Rows, 1))
		kv["arrivals_per_tick"], kv["span_hours"] = spec.arrivals, float64(spec.n)*float64(spec.stepUS())/3.6e9
		if table == store.HistoryRetentionCodexUsage {
			kv["avg_jsonb_bytes"] = fx.one(`SELECT avg(octet_length(buckets::text))::bigint FROM codex_usage_samples`)
			kv["max_jsonb_bytes"] = fx.one(`SELECT max(octet_length(buckets::text)) FROM codex_usage_samples`)
		}
		fx.log.rec("seed", kv)
		t.Logf("seeded %s: %d rows in %s, %.0f B/row heap+toast", fx.tableLabel(table), s.Rows, d.Round(time.Second), kv["bytes_per_row_heap_toast"])
	}
}

// TestHistoryRetentionWarmEvidenceLiveDB runs cases (c), (a) and (b) for every selected
// table and candidate index. Each (table, candidate) starts from a fresh fixture, because (a) and
// (b) delete committed rows.
func TestHistoryRetentionWarmEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	for _, table := range fx.tables {
		spec := fx.stdSpec(table)
		for _, cand := range heList("UZI_EVIDENCE_CANDS", heCands) {
			seedD := fx.reseed(spec)
			build := fx.setIndex(table, cand)
			kv := fx.sizes(table, cand).kv()
			kv["table"], kv["cand"], kv["build_ms"], kv["seed_seconds"] = fx.tableLabel(table), cand, heMS(build), seedD.Seconds()
			fx.log.rec("sizes", kv)
			for _, c := range heWarmCases {
				fx.runCase(spec, cand, c)
			}
			kv = fx.sizes(table, cand).kv()
			kv["table"], kv["cand"], kv["after"] = fx.tableLabel(table), cand, "warm"
			fx.log.rec("sizes", kv)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// compressed-time drain runs

// hePassState emulates the pass's per-process state: the rotation counter and the per-table
// adaptive batch. The production Pass cannot be driven with an advanced cutoff (its cutoff is the
// database clock minus the retention), so the harness reproduces its ordering, batching and
// deadline logic around the production session; the real Pass is exercised by the smoke.
type hePassState struct {
	counter uint64
	batch   map[store.HistoryRetentionTable]int32
}

func newPassState(tables []store.HistoryRetentionTable) *hePassState {
	ps := &hePassState{batch: map[store.HistoryRetentionTable]int32{}}
	for _, t := range tables {
		ps.batch[t] = heMaxBatch
	}
	return ps
}

// passTick runs one emulated tick over the selected tables: visit in counter order, one adaptive
// batch per table, continue past a known rollback, end at an unknown outcome or the work deadline.
func (fx *heFx) passTick(ps *hePassState, cutoffs map[store.HistoryRetentionTable]time.Time) ([]heTickRes, time.Duration) {
	fx.t.Helper()
	start := time.Now()
	cleanupCtx, cancelC := context.WithDeadline(fx.ctx, start.Add(2*time.Second))
	defer cancelC()
	workCtx, cancelW := context.WithDeadline(cleanupCtx, start.Add(1750*time.Millisecond))
	defer cancelW()
	var out []heTickRes
	sess, ok, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, fx.pool, heStmtTimeout, heLockTimeout)
	if err != nil || !ok {
		return []heTickRes{{Err: fmt.Sprintf("unknown: open ok=%v err=%v", ok, err)}}, time.Since(start)
	}
	first := ps.counter
	ps.counter++
	all := store.HistoryRetentionTables()
	for i := 0; i < len(all) && workCtx.Err() == nil; i++ {
		table := all[(first+uint64(i))%uint64(len(all))]
		cutoff, sel := cutoffs[table]
		if !sel {
			continue
		}
		r := heTickRes{Table: table, Batch: ps.batch[table]}
		r.N, r.Err = fx.deleteOn(workCtx, sess, table, cutoff, r.Batch, &r)
		switch r.Err {
		case "":
			ps.batch[table] = min(r.Batch*2, heMaxBatch)
		case "57014":
			ps.batch[table] = max(r.Batch/2, heMinBatch)
		}
		out = append(out, r)
		if strings.HasPrefix(r.Err, "unknown") {
			break
		}
	}
	if _, err := sess.Close(cleanupCtx); err != nil {
		out = append(out, heTickRes{Err: "unknown: close: " + err.Error()})
	}
	return out, time.Since(start)
}

type heTabRun struct {
	spec       heSpec
	arrivals   int
	offset     time.Duration // initial backlog: cutoff starts this far past the fixture start
	delByTick  []int64
	msByTick   []float64
	batchByTk  []int32
	errByTick  []string
	backlogEnd int // first tick whose batch came back short and clean; -1 while a backlog exists
}

// autovacuumState reads the table's actual autovacuum_enabled reloption from pg_class.reloptions
// (not the environment flag) and returns whether it is off, with the raw reloptions text. The value
// is matched case-insensitively against the boolean spellings PostgreSQL treats as false.
func (fx *heFx) autovacuumState(table store.HistoryRetentionTable) (off bool, reloptions string) {
	fx.t.Helper()
	if err := fx.pool.QueryRow(fx.ctx, `SELECT EXISTS (SELECT 1 FROM unnest(coalesce(reloptions, '{}'::text[])) o
		          WHERE lower(o) ~ '^autovacuum_enabled=(false|off|0|no|f|n)$'), coalesce(reloptions::text, '')
	        FROM pg_class WHERE oid = $1::regclass`, string(table)).Scan(&off, &reloptions); err != nil {
		fx.t.Fatalf("read reloptions of %s: %v", table, err)
	}
	return off, reloptions
}

func (fx *heFx) tableStats(table store.HistoryRetentionTable) map[string]any {
	var live, dead, av, aav, ins int64
	_ = fx.pool.QueryRow(fx.ctx, `SELECT n_live_tup, n_dead_tup, autovacuum_count, autoanalyze_count, n_tup_ins
	        FROM pg_stat_user_tables WHERE relname = $1`, string(table)).Scan(&live, &dead, &av, &aav, &ins)
	return map[string]any{"n_live_tup": live, "n_dead_tup": dead, "autovacuum_count": av, "autoanalyze_count": aav, "n_tup_ins": ins}
}

// TestHistoryRetentionCompressedEvidenceLiveDB runs the compressed-time drain over the selected
// tables together, one candidate profile at a time (the same candidate on every selected table).
// Mode backlog starts with a 90% backlog (case b); mode churn starts with a 5% backlog and then
// keeps arrivals and expiries in step for 1.1 retention windows (case d). Arrivals per 15 s tick:
// 250 per sample table and UZI_EVIDENCE_PR_ARRIVALS (default 6) for pr_history_events. The cutoff
// advances 15 s per tick. Default autovacuum runs and no manual VACUUM is issued during the run.
func TestHistoryRetentionCompressedEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	mode := os.Getenv("UZI_EVIDENCE_MODE")
	if mode != "backlog" && mode != "churn" {
		t.Fatalf("UZI_EVIDENCE_MODE must be backlog or churn, got %q", mode)
	}
	defTicks := 4400
	offFrac := 0.05
	if mode == "backlog" {
		defTicks, offFrac = 1100, 0.90
	}
	ticks := heEnvInt("UZI_EVIDENCE_TICKS", defTicks)
	wall := time.Duration(heEnvInt("UZI_EVIDENCE_MAX_MINUTES", 20)) * time.Minute
	for _, cand := range heList("UZI_EVIDENCE_CANDS", heCands) {
		fx.compressedRun(mode, cand, ticks, offFrac, wall)
	}
}

func (fx *heFx) compressedRun(mode, cand string, ticks int, offFrac float64, wall time.Duration) {
	fx.t.Helper()
	runs := map[store.HistoryRetentionTable]*heTabRun{}
	for _, table := range fx.tables {
		spec := fx.stdSpec(table)
		fx.reseed(spec)
		fx.setIndex(table, cand)
		// Diagnostic only (UZI_EVIDENCE_NO_AV=1): switch autovacuum off for the table during the run, to
		// tell vacuum interference from the statement itself. reseed has just reset it to the default,
		// so a run without the flag measures with autovacuum on whatever an earlier run left behind.
		if os.Getenv("UZI_EVIDENCE_NO_AV") == "1" {
			fx.exec(`ALTER TABLE ` + string(table) + ` SET (autovacuum_enabled = false)`)
		}
		fx.t.Cleanup(func() {
			_, _ = fx.pool.Exec(context.Background(), `ALTER TABLE `+string(table)+` RESET (autovacuum_enabled)`)
		})
		spanDur := time.Duration(int64(spec.n)*spec.stepUS()) * time.Microsecond
		runs[table] = &heTabRun{spec: spec, arrivals: spec.arrivals, offset: time.Duration(offFrac * float64(spanDur)), backlogEnd: -1}
		kv := fx.sizes(table, cand).kv()
		kv["table"], kv["cand"], kv["mode"], kv["after"] = fx.tableLabel(table), cand, mode, "run_start"
		kv["autovacuum_off"], kv["reloptions"] = fx.autovacuumState(table)
		kv["server_version"] = fx.serverVersion()
		for k, v := range fx.tableStats(table) {
			kv[k] = v
		}
		fx.log.rec("sizes", kv)
	}
	ps := newPassState(fx.tables)
	deadline := time.Now().Add(wall)
	done := 0
	var passMS []float64
	var cutoffs map[store.HistoryRetentionTable]time.Time
	insMS := map[store.HistoryRetentionTable][]float64{}
	var slowSamples []map[string]any
	for k := 0; k < ticks && time.Now().Before(deadline); k++ {
		cutoffs = map[store.HistoryRetentionTable]time.Time{}
		for _, table := range fx.tables {
			r := runs[table]
			t0 := time.Now()
			fx.insertRows(r.spec, int64(r.spec.n+k*r.arrivals), int64(r.spec.n+(k+1)*r.arrivals))
			insMS[table] = append(insMS[table], heMS(time.Since(t0)))
			cutoffs[table] = r.spec.start.Add(r.offset).Add(time.Duration(k+1) * heTick)
		}
		res, el := fx.passTick(ps, cutoffs)
		passMS = append(passMS, heMS(el))
		got := map[store.HistoryRetentionTable]heTickRes{}
		for _, x := range res {
			if x.Table != "" {
				got[x.Table] = x
			}
		}
		slow := false
		for _, x := range res {
			slow = slow || x.DelMS > 100
		}
		if slow && len(slowSamples) < 300 {
			// What the server was doing when a batch was slow: autovacuum, checkpoints.
			var vac, ckpt, workers int64
			_ = fx.pool.QueryRow(fx.ctx, `SELECT (SELECT count(*) FROM pg_stat_progress_vacuum),
			        (SELECT num_timed + num_requested FROM pg_stat_checkpointer),
			        (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'autovacuum worker')`).Scan(&vac, &ckpt, &workers)
			slowSamples = append(slowSamples, map[string]any{"tick": k, "pass_ms": heMS(el), "vacuum_running": vac, "checkpoints": ckpt, "av_workers": workers,
				"ms": func() map[string]float64 {
					m := map[string]float64{}
					for _, x := range res {
						m[heShort(x.Table)] = x.DelMS
					}
					return m
				}()})
		}
		for _, table := range fx.tables {
			r := runs[table]
			x, ok := got[table]
			if !ok { // not reached this tick (deadline or earlier unknown outcome)
				x = heTickRes{Table: table, Err: "skipped"}
			}
			r.delByTick = append(r.delByTick, x.N)
			r.msByTick = append(r.msByTick, x.DelMS)
			r.batchByTk = append(r.batchByTk, x.Batch)
			r.errByTick = append(r.errByTick, x.Err)
			if r.backlogEnd < 0 && x.Err == "" && x.N < int64(x.Batch) {
				r.backlogEnd = k
			}
		}
		done = k + 1
	}
	for _, table := range fx.tables {
		fx.reportCompressed(mode, cand, table, runs[table], done, ticks, insMS[table])
	}
	fx.log.rec("slow_ticks", map[string]any{"mode": mode, "cand": cand, "samples": slowSamples})
	fx.log.rec("compressed_pass", map[string]any{"mode": mode, "cand": cand, "ticks_done": done, "ticks_planned": ticks,
		"pass_median_ms": heQuant(passMS, 0.5), "pass_max_ms": heMaxF(passMS), "pass_p99_ms": heQuant(passMS, 0.99)})
	// near-empty tick after the run: cutoff at the last tick's value (everything below is gone)
	for _, table := range fx.tables {
		r := runs[table]
		cut := r.spec.start.Add(r.offset).Add(time.Duration(done) * heTick)
		ex := fx.explain(table, cut, heMaxBatch)
		var del []float64
		var n int64
		for i := 0; i < 5; i++ {
			x := fx.oneTick(table, cut, heMaxBatch)
			del = append(del, x.DelMS)
			n += x.N
		}
		kv := ex.kv()
		kv["table"], kv["cand"], kv["mode"] = fx.tableLabel(table), cand, mode
		kv["noop_median_ms"], kv["noop_max_ms"], kv["noop_deleted"] = heQuant(del, 0.5), heMaxF(del), n
		sz := fx.sizes(table, cand)
		for a, b := range sz.kv() {
			kv[a] = b
		}
		for a, b := range fx.tableStats(table) {
			kv[a] = b
		}
		fx.log.rec("near_empty", kv)
	}
}

// reportCompressed evaluates the drain gate for one table: while a backlog exists, deletes exceed
// arrivals over every complete 4-tick rotation cycle and over the whole backlog interval.
func (fx *heFx) reportCompressed(mode, cand string, table store.HistoryRetentionTable, r *heTabRun, done, planned int, insMS []float64) {
	fx.t.Helper()
	end := done // ticks [0, end) have a backlog
	if r.backlogEnd >= 0 {
		end = r.backlogEnd
	}
	var cum, cycles, badCycles, worstDeficit int64
	var cyc int64
	for i := 0; i < end; i++ {
		cum += r.delByTick[i]
		cyc += r.delByTick[i]
		if (i+1)%4 == 0 {
			cycles++
			arr := int64(4 * r.arrivals)
			if cyc <= arr {
				badCycles++
				worstDeficit = max(worstDeficit, arr-cyc)
			}
			cyc = 0
		}
	}
	arrivalsOverBacklog := int64(end * r.arrivals)
	var timeouts, lockTO, unknown, skipped int
	for _, e := range r.errByTick {
		switch {
		case e == "57014":
			timeouts++
		case e == "55P03":
			lockTO++
		case e == "skipped":
			skipped++
		case e != "":
			unknown++
		}
	}
	var total int64
	for _, d := range r.delByTick {
		total += d
	}
	steady := r.msByTick
	if r.backlogEnd >= 0 && r.backlogEnd < len(steady) {
		steady = steady[r.backlogEnd:]
	}
	over300 := 0
	for _, m := range r.msByTick {
		if m > 300 {
			over300++
		}
	}
	kv := map[string]any{"table": fx.tableLabel(table), "cand": cand, "mode": mode, "ticks_done": done, "ticks_planned": planned,
		"arrivals_per_tick": r.arrivals, "backlog_end_tick": r.backlogEnd, "backlog_ticks": end,
		"backlog_deleted": cum, "backlog_arrivals": arrivalsOverBacklog, "backlog_whole_ok": cum > arrivalsOverBacklog || end == 0,
		"cycles": cycles, "cycles_not_exceeding": badCycles, "worst_cycle_deficit": worstDeficit,
		"total_deleted": total, "total_arrivals": int64(done * r.arrivals),
		"timeouts_57014": timeouts, "lock_timeouts_55P03": lockTO, "unknown": unknown, "skipped": skipped, "ticks_over_300ms": over300,
		"del_median_ms": heQuant(r.msByTick, 0.5), "del_p99_ms": heQuant(r.msByTick, 0.99), "del_max_ms": heMaxF(r.msByTick),
		"steady_median_ms": heQuant(steady, 0.5), "steady_p99_ms": heQuant(steady, 0.99), "steady_max_ms": heMaxF(steady),
		"insert_median_ms": heQuant(insMS, 0.5), "insert_max_ms": heMaxF(insMS)}
	reasons := heDiagnosticReasons(fx, false)
	kv["qualifying"], kv["diagnostic_overrides"] = len(reasons) == 0, reasons
	// compact per-tick record: deletes per tick and latency, in 50-tick blocks to keep the log small
	var blockDel []int64
	var blockMax []float64
	for i := 0; i < len(r.delByTick); i += 50 {
		j := min(i+50, len(r.delByTick))
		var s int64
		mx := 0.0
		for q := i; q < j; q++ {
			s += r.delByTick[q]
			mx = max(mx, r.msByTick[q])
		}
		blockDel, blockMax = append(blockDel, s), append(blockMax, mx)
	}
	kv["block50_deleted"], kv["block50_max_ms"] = blockDel, heRound(blockMax)
	if end > 0 && end <= 400 {
		kv["backlog_delete_per_tick"] = r.delByTick[:end]
	}
	fx.log.rec("compressed", kv)
	fx.t.Logf("compressed %s %s %s: ticks %d/%d backlog_end %d, backlog deleted %d vs arrivals %d (cycles %d, not exceeding %d), steady median %.1f max %.1f ms, 57014=%d",
		mode, fx.tableLabel(table), cand, done, planned, r.backlogEnd, cum, arrivalsOverBacklog, cycles, badCycles,
		heQuant(steady, 0.5), heMaxF(steady), timeouts)
}

// ---------------------------------------------------------------------------------------------
// real-engine smoke

type heLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *heLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

type heStubSweeper struct{}

func (heStubSweeper) Sweep(context.Context) (workersvc.SweepResult, error) {
	return workersvc.SweepResult{}, nil
}

// TestHistoryRetentionSmokeEvidenceLiveDB runs the production historyretention.Pass under a real
// sweeper.Engine (stub Sweeper, a timing decorator around the unchanged Pass) at the production
// 15 s interval, driving Run only, retention 24h, on the 1M-row fixture with the chosen indexes.
// Each table holds 30,000 expired rows (pr_history_events: ordinary, not first_merge) and no
// writer runs.
func TestHistoryRetentionSmokeEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	idx := map[store.HistoryRetentionTable]string{}
	for _, table := range store.HistoryRetentionTables() {
		idx[table] = "none"
	}
	for _, kv := range heList("UZI_EVIDENCE_SMOKE_INDEXES", nil) {
		p := strings.SplitN(kv, "=", 2)
		idx[heTableByShort(p[0])] = p[1]
	}
	const expiredRows = 30_000
	now := time.Now().UTC()
	for _, table := range store.HistoryRetentionTables() {
		// Density of the sample fixtures for every table, so rows span 16.7 h and none is in the future.
		spec := fx.specFor(table, heSampleArr, time.Time{})
		spec.start = now.Add(-24 * time.Hour).Add(-time.Duration(int64(expiredRows)*spec.stepUS()) * time.Microsecond).Truncate(5 * time.Minute)
		fx.reseed(spec)
		fx.setIndex(table, idx[table])
		fx.exec(`CHECKPOINT`)
		var expired int64
		where := heExpiryCol(table) + ` < now() - interval '24 hours'`
		if table == store.HistoryRetentionPRHistoryEvents {
			where += ` AND NOT first_merge`
		}
		expired = fx.one(`SELECT count(*) FROM ` + string(table) + ` WHERE ` + where)
		fx.log.rec("smoke_fixture", map[string]any{"table": string(table), "cand": idx[table], "expired_ordinary": expired,
			"start": spec.start.Format(time.RFC3339)})
		if expired < 25_000 {
			t.Fatalf("%s has only %d expired ordinary rows, need at least 25,000", table, expired)
		}
	}
	before := map[store.HistoryRetentionTable]int64{}
	for _, table := range store.HistoryRetentionTables() {
		before[table] = fx.one(`SELECT count(*) FROM ` + string(table))
	}

	capture := &heLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(capture, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	type tickRec struct {
		DurMS float64
		N     int64
		Err   string
	}
	var mu sync.Mutex
	var recs []tickRec
	pass := historyretention.New(fx.pool, 24*time.Hour)
	timed := func(ctx context.Context) (int64, error) {
		t0 := time.Now()
		n, err := pass.Pass(ctx)
		r := tickRec{DurMS: heMS(time.Since(t0)), N: n}
		if err != nil {
			r.Err = err.Error()
		}
		mu.Lock()
		recs = append(recs, r)
		mu.Unlock()
		return n, err
	}
	engine := sweeper.New(heStubSweeper{}, heTick, sweeper.Pass{Name: "history_retention", Run: timed})
	secs := heEnvInt("UZI_EVIDENCE_SMOKE_SECONDS", 300)
	runCtx, cancel := context.WithTimeout(fx.ctx, time.Duration(secs)*time.Second+time.Second)
	defer cancel()
	t0 := time.Now()
	engine.Run(runCtx)
	elapsed := time.Since(t0)

	after := map[store.HistoryRetentionTable]int64{}
	var sumDelta int64
	for _, table := range store.HistoryRetentionTables() {
		after[table] = fx.one(`SELECT count(*) FROM ` + string(table))
		sumDelta += before[table] - after[table]
	}
	var sumConfirmed int64
	maxDur := 0.0
	var durs []float64
	var ns []int64
	var errTicks []string
	for _, r := range recs {
		sumConfirmed += r.N
		maxDur = max(maxDur, r.DurMS)
		durs, ns = append(durs, r.DurMS), append(ns, r.N)
		if r.Err != "" {
			errTicks = append(errTicks, r.Err)
		}
	}
	logs := capture.buf.String()
	kv := map[string]any{"seconds": elapsed.Seconds(), "ticks": len(recs), "tick_ms": heRound(durs), "tick_confirmed": ns,
		"max_tick_ms": maxDur, "sum_confirmed": sumConfirmed, "sum_delta": sumDelta,
		"unknown_outcome_log_lines": strings.Count(logs, "unknown delete outcome"),
		"timeout_log_lines":         strings.Count(logs, "repeated statement timeouts"), "tick_errors": errTicks}
	deltas := map[string]int64{}
	for _, table := range store.HistoryRetentionTables() {
		deltas[string(table)] = before[table] - after[table]
	}
	kv["delta"] = deltas
	thr := map[store.HistoryRetentionTable]int64{store.HistoryRetentionPRHistoryEvents: 105,
		store.HistoryRetentionAnthropicUsage: 5000, store.HistoryRetentionCodexUsage: 5000, store.HistoryRetentionWorkerCapacitySample: 5000}
	pass2 := maxDur <= 2000 && sumConfirmed <= sumDelta
	perTable := map[string]bool{}
	for table, th := range thr {
		ok := before[table]-after[table] > th
		perTable[string(table)] = ok
		pass2 = pass2 && ok
	}
	kv["per_table_delta_ok"], kv["smoke_pass"] = perTable, pass2
	kv["indexes"] = fmt.Sprint(idx)
	kv["server_version"] = fx.serverVersion()
	reasons := heDiagnosticReasons(fx, true)
	kv["qualifying"], kv["diagnostic_overrides"] = len(reasons) == 0, reasons
	av := map[string]bool{}
	for _, table := range store.HistoryRetentionTables() {
		off, _ := fx.autovacuumState(table)
		av[string(table)] = off
	}
	kv["autovacuum_off"] = av
	fx.log.rec("smoke", kv)
	fx.log.rec("smoke_slog", map[string]any{"log": truncateTail(logs, 60_000)})
	t.Logf("smoke: %d ticks, max %.0f ms, deltas %v, confirmed %d <= %d, pass=%v qualifying=%v", len(recs), maxDur, deltas, sumConfirmed, sumDelta, pass2, len(reasons) == 0)
	if len(reasons) > 0 {
		t.Logf("smoke is diagnostic only, not qualifying evidence: %s", strings.Join(reasons, "; "))
	}
	if !pass2 {
		t.Errorf("smoke gate failed: max tick %.0f ms (limit 2000), confirmed %d vs table delta %d (confirmed must not exceed it), per-table delta ok %v", maxDur, sumConfirmed, sumDelta, perTable)
	}
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------------------------------------------------------------------------------------------
// cold phase

// TestHistoryRetentionColdEvidenceLiveDB has two modes selected by UZI_EVIDENCE_COLD_TABLE (pr,
// anthropic, codex or worker) and UZI_EVIDENCE_COLD_SETUP:
//
//	UZI_EVIDENCE_COLD_SETUP=1  reseed the table with the candidate UZI_EVIDENCE_CANDS (one value),
//	                           VACUUM ANALYZE and CHECKPOINT, then stop
//	otherwise                  run EXACTLY one tick of UZI_EVIDENCE_COLD_CASE (a, b or c), as the
//	                           first work after the pool opens. A shell script restarts the
//	                           container before each invocation. The cutoff is computed from the
//	                           fixture spec, so no query touches the table first. The marker
//	                           check (requireEvidenceMarker, which also reads server_version)
//	                           opens a separate backend first on its own pool; the tick is the
//	                           first work on fx.pool.
func TestHistoryRetentionColdEvidenceLiveDB(t *testing.T) {
	table := heTableByShort(os.Getenv("UZI_EVIDENCE_COLD_TABLE"))
	if os.Getenv("UZI_EVIDENCE_COLD_SETUP") == "1" {
		fx := newHeFx(t, true)
		cand := heList("UZI_EVIDENCE_CANDS", []string{"none"})[0]
		spec := fx.stdSpec(table)
		fx.reseed(spec)
		fx.setIndex(table, cand)
		fx.exec(`CHECKPOINT`)
		kv := fx.sizes(table, cand).kv()
		kv["table"], kv["cand"], kv["after"] = fx.tableLabel(table), cand, "cold_setup"
		fx.log.rec("sizes", kv)
		return
	}
	fx := newHeFx(t, false)
	spec := fx.stdSpec(table)
	cutoff := spec.start.Add(-time.Hour)
	switch os.Getenv("UZI_EVIDENCE_COLD_CASE") {
	case "a":
		cutoff = spec.frac(0.0205)
	case "b":
		cutoff = spec.frac(0.90)
	case "c":
	default:
		t.Fatal("UZI_EVIDENCE_COLD_CASE must be a, b or c")
	}
	t0 := time.Now()
	r := fx.oneTick(table, cutoff, heMaxBatch)
	fx.log.rec("cold", map[string]any{"table": fx.tableLabel(table), "label": os.Getenv("UZI_EVIDENCE_COLD_LABEL"),
		"case": os.Getenv("UZI_EVIDENCE_COLD_CASE"), "open_ms": r.OpenMS, "delete_ms": r.DelMS, "close_ms": r.CloseMS,
		"total_ms": heMS(time.Since(t0)), "deleted": r.N, "err": r.Err})
	t.Logf("cold %s case %s: open %.1f ms delete %.1f ms deleted %d err %q", table, os.Getenv("UZI_EVIDENCE_COLD_CASE"), r.OpenMS, r.DelMS, r.N, r.Err)
}
