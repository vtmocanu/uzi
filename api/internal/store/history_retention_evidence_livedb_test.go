//go:build historyretentionevidence

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
//	ColdEvidenceLiveDB       fixture setup (publishes a binding), or the first tick after a container restart
//
// Environment (all optional unless marked):
//
//	UZI_HISTORY_RETENTION_EVIDENCE=1   required; without it every entrypoint skips
//	UZI_EVIDENCE_LOG                   required, absolute path, JSON lines are appended
//	UZI_TEST_DATABASE_URL              required
//	UZI_EVIDENCE_ROWS                  rows per table (default 1000000)
//	UZI_EVIDENCE_TABLES                comma list of pr,anthropic,codex,worker (default all four);
//	                                   deduplicated, an unknown name is refused
//	UZI_EVIDENCE_CODEX                 typical (default) or max: the codex jsonb size
//	UZI_EVIDENCE_CANDS                 comma list of none,btree,brin_off,brin_on (default all four);
//	                                   deduplicated, an unknown name is refused
//	UZI_EVIDENCE_MODE                  backlog or churn (Compressed)
//	UZI_EVIDENCE_TICKS                 compressed ticks; the default is the required ticks, the
//	                                   maximum over the selected tables (heRequiredTicks)
//	UZI_EVIDENCE_BACKLOG_PREFLIGHT=1   Compressed, mode backlog: one production DeleteBatch at the
//	                                   initial backlog cutoff plus a rolled-back EXPLAIN per selected
//	                                   table and candidate, then exit without the drain loop
//	UZI_EVIDENCE_PR_ARRIVALS           pr_history_events arrivals per 15 s tick (default 6)
//	UZI_EVIDENCE_MAX_MINUTES           wall-clock cap of one compressed run (default 20)
//	UZI_EVIDENCE_SMOKE_INDEXES         table=candidate list (Smoke); an omitted table keeps btree,
//	                                   the production index
//	UZI_EVIDENCE_SMOKE_SECONDS         smoke length (default 300)
//	UZI_EVIDENCE_COLD_TABLE / _CASE / _SETUP / _SETUP_ID / _LABEL   see ColdEvidenceLiveDB
//
// Labels. The compressed, smoke and preflight records carry coverage_reasons (settings that narrow the run
// below the evidence fixture: fewer tables, candidates or rows, a changed arrival rate or codex
// variant, autovacuum off, a short or non-btree smoke) and run_reasons (what this run itself
// observed: short or failed ticks, a wall-clock or error stop, a residual backlog, a probe error).
// Only a record with both lists empty is qualifying. Warm and cold records are unlabelled
// measurements; a cold record is bound to its setup instead.
//
// Measurements use the production session (store.OpenHistoryRetentionSession, DeleteBatch, a fresh
// connection per tick, 300 ms statement timeout and 200 ms lock timeout). Run without -race.
//
// Scope. The harness is an opt-in single-operator tool. Running two invocations against one hrev at
// once is unsupported and is not fenced. The hrev database is trusted only when it carries the
// evidence marker this harness writes when it creates the database; an unmarked hrev left behind by
// a creator that exited is refused by the marker check, and the operator drops it only after
// verifying that they created it.

const (
	heDBName            = "hrev"
	heTick              = 15 * time.Second
	heStmtTimeout       = 300 * time.Millisecond
	heLockTimeout       = 200 * time.Millisecond
	heMaxBatch          = int32(1000)
	heDefaultPRArrivals = 6
	heSampleArr         = 250
	heMarker            = "recovery-reset-v1"
	// heEvidenceMarker marks only the hrev database this harness created. An ordinary LiveDB
	// database that happens to be named hrev carries heMarker alone and is refused.
	heEvidenceMarker = "history-retention-evidence-v1"
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

// heParseList splits a comma list, trims each name, drops repeats (first occurrence wins, order is
// kept) and refuses a name outside allowed. An empty list selects every allowed name.
func heParseList(raw string, allowed []string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), allowed...), nil
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("unknown name %q (allowed: %s)", name, strings.Join(allowed, ","))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// heParseSelection resolves the raw table and candidate selectors.
func heParseSelection(rawTables, rawCands string) ([]store.HistoryRetentionTable, []string, error) {
	var allowed []string
	for _, t := range store.HistoryRetentionTables() {
		allowed = append(allowed, heShort(t))
	}
	shorts, err := heParseList(rawTables, allowed)
	if err != nil {
		return nil, nil, fmt.Errorf("UZI_EVIDENCE_TABLES: %w", err)
	}
	var tables []store.HistoryRetentionTable
	for _, s := range shorts {
		t, err := heTableByShort(s)
		if err != nil {
			return nil, nil, err
		}
		tables = append(tables, t)
	}
	cands, err := heParseList(rawCands, heCands)
	if err != nil {
		return nil, nil, fmt.Errorf("UZI_EVIDENCE_CANDS: %w", err)
	}
	return tables, cands, nil
}

// heParseSelectors is the only reader of UZI_EVIDENCE_TABLES and UZI_EVIDENCE_CANDS; newHeFx calls it
// once and every phase and the label use the result.
func heParseSelectors() ([]store.HistoryRetentionTable, []string, error) {
	return heParseSelection(os.Getenv("UZI_EVIDENCE_TABLES"), os.Getenv("UZI_EVIDENCE_CANDS"))
}

// heRequireOptIn skips the calling test unless the operator opted in, before anything reads or
// parses the rest of the environment.
func heRequireOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("UZI_HISTORY_RETENTION_EVIDENCE") != "1" {
		t.Skip("UZI_HISTORY_RETENTION_EVIDENCE=1 not set; opt-in evidence harness")
	}
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
	cands  []string
	// version is the server_version read on the marker check's own backend by requireEvidenceMarker.
	version string
}

func heTableByShort(s string) (store.HistoryRetentionTable, error) {
	switch s {
	case "pr":
		return store.HistoryRetentionPRHistoryEvents, nil
	case "anthropic":
		return store.HistoryRetentionAnthropicUsage, nil
	case "codex":
		return store.HistoryRetentionCodexUsage, nil
	case "worker":
		return store.HistoryRetentionWorkerCapacitySample, nil
	}
	return "", fmt.Errorf("unknown table %q", s)
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

func heEvidenceDSN(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/" + heDBName
	dsn := u.String()
	// A URL query parameter can override the database named by the path.
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse evidence DSN: %w", err)
	}
	if cfg.Database != heDBName {
		return "", fmt.Errorf("evidence DSN selects database %q, want %q", cfg.Database, heDBName)
	}
	// Each harness connection (the marker check, store.Migrate, store.OpenPool) dials anew. A
	// multi-host DSN, or a hostname resolving to several servers, could verify the marker on one
	// server and write on another, so require one direct endpoint: a loopback IP or a local Unix
	// socket. Fallbacks are allowed only as TLS retries of that same endpoint (sslmode=prefer).
	if ip := net.ParseIP(cfg.Host); !strings.HasPrefix(cfg.Host, "/") && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("evidence DSN host %q must be a loopback IP or a Unix socket", cfg.Host)
	}
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return "", fmt.Errorf("evidence DSN names more than one endpoint (%s:%d)", fb.Host, fb.Port)
		}
	}
	return dsn, nil
}

func TestHistoryRetentionEvidenceDSN(t *testing.T) {
	const base = "postgres://tester@127.0.0.1/sweep?sslmode=disable"
	dsn, err := heEvidenceDSN(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database != heDBName {
		t.Fatalf("effective database = %q, want %q", cfg.Database, heDBName)
	}

	if _, err := heEvidenceDSN(base + "&dbname=sweep"); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("conflicting dbname error = %v, want a database identity refusal", err)
	}
	// Default sslmode=prefer adds a same-endpoint TLS fallback; a Unix socket is a direct endpoint.
	for _, ok := range []string{"postgres://tester@127.0.0.1:5433/sweep", "postgres://tester@[::1]/sweep", "postgres://tester@/sweep?host=/tmp"} {
		if _, err := heEvidenceDSN(ok); err != nil {
			t.Fatalf("heEvidenceDSN(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{
		"postgres://tester@127.0.0.1,127.0.0.2/sweep?sslmode=disable",
		"postgres://tester@127.0.0.1:5432,127.0.0.1:5433/sweep?sslmode=disable",
		"postgres://tester@localhost/sweep?sslmode=disable",
		"postgres://tester@db.example.com/sweep",
	} {
		if _, err := heEvidenceDSN(bad); err == nil || !strings.Contains(err.Error(), "evidence DSN") {
			t.Fatalf("heEvidenceDSN(%q) = %v, want an endpoint refusal", bad, err)
		}
	}
}

// newHeFx connects to the evidence database. The marker of the database UZI_TEST_DATABASE_URL names
// is checked first (admin paths); the evidence database hrev is created with its own evidence
// marker row (heEvidenceMarker, in the e2e/test-database-marker.sql shape) and EVERY path, including
// the cold one, verifies that hrev carries only that marker before anything mutates it, so a hrev
// that something else made, an ordinary marked LiveDB database included, is refused. With admin=true it then migrates hrev; with admin=false (the cold tick) it opens the
// pool and nothing else (the marker check ran on a separate pool of its own), so no query touches
// the history tables, and none runs on fx.pool, before the first measured tick.
func newHeFx(t *testing.T, admin bool) *heFx {
	t.Helper()
	heRequireOptIn(t)
	logPath := os.Getenv("UZI_EVIDENCE_LOG")
	if !filepath.IsAbs(logPath) {
		t.Fatalf("UZI_EVIDENCE_LOG must be an absolute path, got %q", logPath)
	}
	base := os.Getenv("UZI_TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dsn, err := heEvidenceDSN(base)
	if err != nil {
		t.Fatal(err)
	}
	fx := &heFx{t: t, ctx: ctx, dsn: dsn, log: &heLog{t: t, path: logPath}, codex: "typical"}
	if v := os.Getenv("UZI_EVIDENCE_CODEX"); v == "max" {
		fx.codex = "max"
	}
	if fx.tables, fx.cands, err = heParseSelectors(); err != nil {
		t.Fatal(err)
	}
	if err := heCheckPRArrivals(fx.prArrivals()); err != nil {
		t.Fatal(err)
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
	fx.log.rec("env", map[string]any{"server_version": fx.serverVersion(), "rows": heRows(), "coverage_reasons": fx.coverageReasons(false, nil)})
	if admin {
		fx.ensureOwner()
	}
	return fx
}

// heCheckEvidenceMarkers accepts exactly the harness's own evidence marker.
func heCheckEvidenceMarkers(markers []string) error {
	if len(markers) != 1 || markers[0] != heEvidenceMarker {
		return fmt.Errorf("it does not carry only the evidence marker %q (markers=%q)", heEvidenceMarker, markers)
	}
	return nil
}

func TestHistoryRetentionEvidenceMarker(t *testing.T) {
	if err := heCheckEvidenceMarkers([]string{heEvidenceMarker}); err != nil {
		t.Fatalf("evidence marker refused: %v", err)
	}
	for _, bad := range [][]string{nil, {heMarker}, {heMarker, heEvidenceMarker}, {"other"}} {
		if err := heCheckEvidenceMarkers(bad); err == nil {
			t.Fatalf("markers %q accepted, want refusal", bad)
		}
	}
}

// markEvidenceDB writes the evidence marker row (heEvidenceMarker) into the freshly created hrev,
// in the shape of e2e/test-database-marker.sql.
func (fx *heFx) markEvidenceDB() {
	fx.t.Helper()
	p, err := pgxpool.New(fx.ctx, fx.dsn)
	if err != nil {
		fx.t.Fatalf("hrev marker pool: %v", err)
	}
	defer p.Close()
	fx.requireEvidenceDatabase(p)
	for _, q := range []string{
		`CREATE SCHEMA uzi_test_harness`,
		`CREATE TABLE uzi_test_harness.database_marker (marker text PRIMARY KEY)`,
		`INSERT INTO uzi_test_harness.database_marker (marker) VALUES ('` + heEvidenceMarker + `')`,
	} {
		if _, err := p.Exec(fx.ctx, q); err != nil {
			fx.t.Fatalf("write hrev marker: %v\n%s", err, q)
		}
	}
}

// requireEvidenceMarker refuses unless the server reports hrev and it carries the marker. It is a
// read-only query over a connection of its own, so nothing is mutated on a refusal.
func (fx *heFx) requireEvidenceMarker() {
	fx.t.Helper()
	p, err := pgxpool.New(fx.ctx, fx.dsn)
	if err != nil {
		fx.t.Fatalf("refusing to touch %s: cannot connect to verify its marker: %v", heDBName, err)
	}
	defer p.Close()
	fx.requireEvidenceDatabase(p)
	rows, err := p.Query(fx.ctx, `SELECT marker FROM uzi_test_harness.database_marker ORDER BY marker`)
	if err != nil {
		fx.t.Fatalf("refusing to touch database %s: cannot read its marker: %v", heDBName, err)
	}
	markers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		fx.t.Fatalf("refusing to touch database %s: cannot read its marker: %v", heDBName, err)
	}
	if err := heCheckEvidenceMarkers(markers); err != nil {
		fx.t.Fatalf("refusing to touch database %s: %v", heDBName, err)
	}
	// server_version is read here, on the marker check's own backend, so fx.pool never runs it.
	if err := p.QueryRow(fx.ctx, `SHOW server_version`).Scan(&fx.version); err != nil {
		fx.t.Fatalf("SHOW server_version: %v", err)
	}
}

// requireEvidenceDatabase verifies the server-selected database before a marker can authorize
// fixture work or be written to a newly created database.
func (fx *heFx) requireEvidenceDatabase(p *pgxpool.Pool) {
	fx.t.Helper()
	var database string
	if err := p.QueryRow(fx.ctx, `SELECT pg_catalog.current_database()`).Scan(&database); err != nil {
		fx.t.Fatalf("read evidence database identity: %v", err)
	}
	if database != heDBName {
		fx.t.Fatalf("evidence connection selected database %q, want %q", database, heDBName)
	}
}

// serverVersion returns the version read by requireEvidenceMarker; it runs no query.
func (fx *heFx) serverVersion() string { return fx.version }

// heCoverageIn is everything the coverage label looks at: the fixture and selector settings of a run.
type heCoverageIn struct {
	tables       []store.HistoryRetentionTable
	cands        []string
	rows         int
	noAV         bool
	prArrivals   int
	codex        string
	smokeSeconds int
	smokeIdx     map[store.HistoryRetentionTable]string
}

// heCoverageReasons lists every setting that narrows what a run covers below the evidence fixture:
// fewer tables or candidates (compared as sets, so a repeated name never fills a slot), fewer rows
// than 1,000,000, autovacuum off, a non-default pr_history_events arrival rate or codex jsonb
// variant, and, for the smoke, a non-btree index or fewer than 300 seconds. Empty means the settings
// cover the fixture; the run itself is judged separately by heRunReasons. smoke selects the smoke
// subset (the candidate selector, autovacuum switch and PR arrivals apply only to compressed runs).
func heCoverageReasons(in heCoverageIn, smoke bool) []string {
	r := []string{}
	if in.rows < 1_000_000 {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_ROWS=%d (below 1000000)", in.rows))
	}
	var want, have []string
	for _, t := range store.HistoryRetentionTables() {
		want = append(want, string(t))
	}
	for _, t := range in.tables {
		have = append(have, string(t))
	}
	if !heSameSet(have, want) {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_TABLES selects %d of %d distinct tables", len(heDistinct(have)), len(want)))
	}
	if in.codex != "typical" {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_CODEX=%s (not the default typical variant)", in.codex))
	}
	if smoke {
		if in.smokeSeconds < 300 {
			r = append(r, fmt.Sprintf("UZI_EVIDENCE_SMOKE_SECONDS=%d (below 300)", in.smokeSeconds))
		}
		for _, t := range store.HistoryRetentionTables() {
			if c := in.smokeIdx[t]; c != "btree" {
				r = append(r, fmt.Sprintf("smoke index of %s is %q (production is btree)", heShort(t), c))
			}
		}
		return r
	}
	if !heSameSet(in.cands, heCands) {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_CANDS selects %d of %d distinct candidates", len(heDistinct(in.cands)), len(heCands)))
	}
	if in.prArrivals != heDefaultPRArrivals {
		r = append(r, fmt.Sprintf("UZI_EVIDENCE_PR_ARRIVALS=%d (default %d)", in.prArrivals, heDefaultPRArrivals))
	}
	if in.noAV {
		r = append(r, "UZI_EVIDENCE_NO_AV=1 (autovacuum off during compressed runs)")
	}
	return r
}

func heDistinct(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func heSameSet(a, b []string) bool {
	da, db := heDistinct(a), heDistinct(b)
	if len(da) != len(db) {
		return false
	}
	for _, x := range da {
		if !slices.Contains(db, x) {
			return false
		}
	}
	return true
}

// coverageReasons builds the coverage label inputs from the parsed selectors and the environment.
func (fx *heFx) coverageReasons(smoke bool, smokeIdx map[store.HistoryRetentionTable]string) []string {
	return heCoverageReasons(heCoverageIn{tables: fx.tables, cands: fx.cands, rows: heRows(),
		noAV: os.Getenv("UZI_EVIDENCE_NO_AV") == "1", prArrivals: fx.prArrivals(), codex: fx.codex,
		smokeSeconds: heEnvInt("UZI_EVIDENCE_SMOKE_SECONDS", 300), smokeIdx: smokeIdx}, smoke)
}

// heParseSmokeIndexes resolves UZI_EVIDENCE_SMOKE_INDEXES (pr=btree,anthropic=btree,...). A table the
// selector omits keeps the production index, btree; a malformed entry, an unknown table or candidate
// or a repeated table is an error.
func heParseSmokeIndexes(raw string) (map[store.HistoryRetentionTable]string, error) {
	idx := map[store.HistoryRetentionTable]string{}
	for _, t := range store.HistoryRetentionTables() {
		idx[t] = "btree"
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return idx, nil
	}
	seen := map[store.HistoryRetentionTable]bool{}
	for _, kv := range strings.Split(raw, ",") {
		name, cand, ok := strings.Cut(strings.TrimSpace(kv), "=")
		name, cand = strings.TrimSpace(name), strings.TrimSpace(cand)
		if !ok {
			return nil, fmt.Errorf("UZI_EVIDENCE_SMOKE_INDEXES entry %q is not table=candidate", kv)
		}
		t, err := heTableByShort(name)
		if err != nil {
			return nil, fmt.Errorf("UZI_EVIDENCE_SMOKE_INDEXES: %w", err)
		}
		if !slices.Contains(heCands, cand) {
			return nil, fmt.Errorf("UZI_EVIDENCE_SMOKE_INDEXES: unknown candidate %q for %s (allowed: %s)", cand, name, strings.Join(heCands, ","))
		}
		if seen[t] {
			return nil, fmt.Errorf("UZI_EVIDENCE_SMOKE_INDEXES names %s twice", name)
		}
		seen[t] = true
		idx[t] = cand
	}
	return idx, nil
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

// perBucket is the number of rows per five-minute bucket (the identity count of a sample table).
func (s heSpec) perBucket() int64 { return int64(300*time.Second/time.Microsecond) / s.stepUS() }

func (s heSpec) bucketed() bool { return s.table != store.HistoryRetentionPRHistoryEvents }

// span is the time the n fixture rows cover.
func (s heSpec) span() time.Duration { return time.Duration(int64(s.n)*s.stepUS()) * time.Microsecond }

// heCaseCutoff returns the cutoff for a fixture fraction frac and the number of rows eligible under
// it. A frac of 0 or less is a cutoff before every row. pr_history_events keeps the observation-time
// cutoff and excludes first_merge rows (1 of every 90). The bucketed tables expire by bucket_start,
// so a cutoff inside a five-minute bucket already makes the whole bucket eligible: the cutoff is
// aligned up to the bucket boundary and the eligible rows are the whole buckets below it.
func heCaseCutoff(s heSpec, frac float64) (time.Time, int64) {
	if frac <= 0 {
		return s.start.Add(-time.Hour), 0
	}
	g := min(int64(frac*float64(s.n)), int64(s.n))
	raw := s.at(g)
	if !s.bucketed() {
		fm := int64(0)
		if g >= 3 { // first_merge rows are g = 90j+2
			fm = (g-3)/90 + 1
		}
		return raw, g - fm
	}
	cut := raw.Truncate(5 * time.Minute)
	if cut.Before(raw) {
		cut = cut.Add(5 * time.Minute)
	}
	step := s.stepUS()
	rows := (int64(cut.Sub(s.start)/time.Microsecond) + step - 1) / step
	return cut, min(rows, int64(s.n))
}

// heEligibleWhere is the expiry predicate of the production DELETE, with the cutoff as $1.
func heEligibleWhere(table store.HistoryRetentionTable) string {
	w := hrExpiryColumn(table) + ` < $1`
	if table == store.HistoryRetentionPRHistoryEvents {
		w += ` AND NOT first_merge`
	}
	return w
}

// heRequiredTicks is the number of 15 s ticks a compressed run must complete to support its claim.
// Churn mode covers the window from the initial cutoff to the end of the fixture, one tick per 15 s.
// Backlog mode must drain the initially eligible rows (bucket-aligned for the bucketed tables) at the
// batch the pass can take net of the arrivals of each tick, plus 5%. An arrival rate that the batch
// cannot outpace (arrivals of 1000 or more per tick) or below 1 is refused.
func heRequiredTicks(s heSpec, offFrac float64, mode string, arrivals int) (int, error) {
	if arrivals < 1 || arrivals >= int(heMaxBatch) {
		return 0, fmt.Errorf("%s: %d arrivals per tick outside [1, %d)", s.table, arrivals, heMaxBatch)
	}
	cutoff0, eligible0 := heCaseCutoff(s, offFrac)
	switch mode {
	case "churn":
		rem := max(s.span()-cutoff0.Sub(s.start), 0)
		return int((rem + heTick - 1) / heTick), nil
	case "backlog":
		per := int64(heMaxBatch) - int64(arrivals)
		base := (eligible0 + per - 1) / per
		return int(base + (base*5+99)/100), nil
	}
	return 0, fmt.Errorf("unknown mode %q", mode)
}

func (fx *heFx) specFor(table store.HistoryRetentionTable, arrivals int, start time.Time) heSpec {
	return heSpec{table: table, n: heRows(), arrivals: arrivals, start: start}
}

// heCheckPRArrivals bounds UZI_EVIDENCE_PR_ARRIVALS: below 1 divides by zero in heSpec.stepUS, and a
// batch cannot outpace heMaxBatch or more arrivals per tick.
func heCheckPRArrivals(n int) error {
	if n < 1 || n >= int(heMaxBatch) {
		return fmt.Errorf("UZI_EVIDENCE_PR_ARRIVALS=%d outside [1, %d]", n, heMaxBatch-1)
	}
	return nil
}

func (fx *heFx) prArrivals() int { return heEnvInt("UZI_EVIDENCE_PR_ARRIVALS", heDefaultPRArrivals) }

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

// insertRowsCtx inserts rows [from, to) of spec s under ctx and returns the error.
func (fx *heFx) insertRowsCtx(ctx context.Context, s heSpec, from, to int64) error {
	var err error
	if s.table == store.HistoryRetentionPRHistoryEvents { // its statement has no identity parameter
		_, err = fx.pool.Exec(ctx, fx.insertSQL(s.table), from, to, s.start, s.stepUS())
	} else {
		_, err = fx.pool.Exec(ctx, fx.insertSQL(s.table), from, to, s.start, s.stepUS(), s.perBucket())
	}
	return err
}

// insertRows inserts rows [from, to) of spec s and fails the test on an error (fixture seeding).
func (fx *heFx) insertRows(s heSpec, from, to int64) {
	fx.t.Helper()
	if err := fx.insertRowsCtx(fx.ctx, s, from, to); err != nil {
		fx.t.Fatalf("insert %s rows [%d,%d): %v", s.table, from, to, err)
	}
}

// reseed first deletes the table's cold binding (any fixture change invalidates a published setup),
// then empties the table and loads spec s, then VACUUM ANALYZE so the heap, visibility map and
// statistics are in the state a settled table has.
func (fx *heFx) reseed(s heSpec) time.Duration {
	fx.t.Helper()
	t0 := time.Now()
	fx.ensureBindingTable()
	fx.exec(`DELETE FROM `+heBindingTable+` WHERE table_name = $1`, string(s.table))
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

// heShipped is the btree the migrations 00555 to 00558 create on each table. The btree candidate IS
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
	// ObsUnavailable is set, and StatIO nil, when the per-relation counters could not be read.
	ObsUnavailable string
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

func (fx *heFx) statio(conn *pgxpool.Conn, table store.HistoryRetentionTable) (map[string]int64, error) {
	fx.t.Helper()
	if _, err := conn.Exec(fx.ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		return nil, fmt.Errorf("pg_stat_force_next_flush: %w", err)
	}
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
		return nil, fmt.Errorf("pg_statio_user_tables: %w", err)
	}
	m := map[string]int64{}
	for i, c := range cols {
		m[c] = vals[i]
	}
	return m, nil
}

// explainE runs the production DELETE of one batch under EXPLAIN (ANALYZE, BUFFERS) with a custom
// plan (a fresh session's first execution is always planned for its parameters) inside a
// transaction that is rolled back, so the fixture is unchanged. Per-relation block counters
// (heap, index, TOAST) come from pg_statio_user_tables before and after.
//
// Every resource the call acquires is released on every path, in order: the rows are closed, the
// transaction is rolled back, then the harness's own prepared statement (heExplainStmt, never
// DEALLOCATE ALL) is deallocated. A connection whose cleanup fails is destroyed instead of being
// returned to the pool. The error is returned; the caller decides whether it is fatal.
func (fx *heFx) explainE(table store.HistoryRetentionTable, cutoff time.Time, batch int32) (heExplain, error) {
	conn, err := fx.pool.Acquire(fx.ctx)
	if err != nil {
		return heExplain{}, fmt.Errorf("explain acquire: %w", err)
	}
	destroy := false
	defer func() {
		if destroy {
			// Hijack takes the connection out of the pool; closing it ends the backend.
			_ = conn.Hijack().Close(context.Background())
			return
		}
		conn.Release()
	}()
	before, errBefore := fx.statio(conn, table)
	lines, err := fx.explainLines(conn, table, cutoff, batch, &destroy)
	if err != nil {
		return heExplain{}, err
	}
	ex, err := heParseExplain(lines)
	if err != nil {
		return heExplain{}, err
	}
	after, errAfter := fx.statio(conn, table)
	if err := errors.Join(errBefore, errAfter); err != nil {
		ex.ObsUnavailable = err.Error()
		return ex, nil
	}
	ex.StatIO = map[string]int64{}
	for k, v := range after {
		ex.StatIO[k] = v - before[k]
	}
	return ex, nil
}

// heExplainStmt is the only prepared statement name the EXPLAIN uses and deallocates.
const heExplainStmt = "hrev_p"

// explainLines runs the EXPLAIN on conn and returns its plan lines. Its deferred cleanup closes the
// rows, rolls the transaction back and deallocates heExplainStmt, in that order; a failed cleanup
// step sets *destroy and joins its error into the result.
func (fx *heFx) explainLines(conn *pgxpool.Conn, table store.HistoryRetentionTable, cutoff time.Time, batch int32, destroy *bool) (lines []string, err error) {
	var rows pgx.Rows
	began, prepared := false, false
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if rows != nil {
			// A server-side error in the rows is returned to the caller; the connection is judged by
			// whether the rollback and deallocate below succeed.
			rows.Close()
		}
		if began {
			if _, cerr := conn.Exec(cctx, `ROLLBACK`); cerr != nil {
				*destroy = true
				err = errors.Join(err, fmt.Errorf("explain cleanup rollback: %w", cerr))
				return // the session state is unknown; a DEALLOCATE would not be trustworthy
			}
		}
		if prepared {
			if _, cerr := conn.Exec(cctx, `DEALLOCATE `+heExplainStmt); cerr != nil {
				*destroy = true
				err = errors.Join(err, fmt.Errorf("explain cleanup deallocate: %w", cerr))
			}
		}
	}()
	step := func(sql string) error {
		if _, serr := conn.Exec(fx.ctx, sql); serr != nil {
			return fmt.Errorf("explain setup: %w\n%s", serr, sql)
		}
		return nil
	}
	if err = step(`BEGIN`); err != nil {
		return nil, err
	}
	began = true
	for _, s := range []string{`SET LOCAL plan_cache_mode = force_custom_plan`, `SET LOCAL statement_timeout = 0`} {
		if err = step(s); err != nil {
			return nil, err
		}
	}
	if err = step(`PREPARE ` + heExplainStmt + `(timestamptz, int) AS ` + store.HistoryRetentionDeleteSQLForTest(table)); err != nil {
		return nil, err
	}
	prepared = true
	rows, err = conn.Query(fx.ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, BUFFERS) EXECUTE `+heExplainStmt+`('%s'::timestamptz, %d)`,
		cutoff.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), batch))
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	for rows.Next() {
		var line string
		if serr := rows.Scan(&line); serr != nil {
			return nil, fmt.Errorf("explain scan: %w", serr)
		}
		lines = append(lines, line)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("explain rows: %w", rerr)
	}
	return lines, nil
}

// heParseExplain reads the plan lines of EXPLAIN (ANALYZE, BUFFERS): the first Buffers line is the
// top node's counters and the Execution Time line is required, so a truncated or unexpected result
// is an error and never a zero.
func heParseExplain(lines []string) (heExplain, error) {
	ex := heExplain{Plan: append([]string(nil), lines...)}
	seenTop, seenExec := false, false
	for _, line := range lines {
		if !seenTop && strings.Contains(line, "Buffers:") {
			seenTop = true
			ex.Hit, ex.Read, ex.Dirtied, ex.Wrte = heParseBuffers(line)
		}
		if strings.HasPrefix(line, "Execution Time:") {
			f := strings.Fields(line)
			if len(f) < 3 {
				return heExplain{}, fmt.Errorf("explain: malformed Execution Time line %q", line)
			}
			ms, err := strconv.ParseFloat(f[2], 64)
			if err != nil {
				return heExplain{}, fmt.Errorf("explain: Execution Time %q: %w", line, err)
			}
			ex.ExecMS, seenExec = ms, true
		}
	}
	if !seenExec {
		return heExplain{}, fmt.Errorf("explain: no Execution Time line in %d plan lines", len(lines))
	}
	return ex, nil
}

func (e heExplain) kv() map[string]any {
	m := map[string]any{"buf_hit": e.Hit, "buf_read": e.Read, "buf_dirtied": e.Dirtied, "buf_written": e.Wrte,
		"explain_exec_ms": e.ExecMS, "plan": e.Plan}
	if e.ObsUnavailable != "" {
		m["obs_unavailable"] = e.ObsUnavailable
	} else {
		m["statio_delta"] = e.StatIO
	}
	return m
}

// ---------------------------------------------------------------------------------------------
// warm cases (a), (b), (c)

type heCase struct {
	name   string
	frac   float64 // fraction of the fixture expired; 0 means a cutoff before every row
	ticks  int
	commit bool
	// drain extends ticks to the batches the eligible rows need, so the case ends with none left.
	drain bool
}

var heWarmCases = []heCase{{"c", 0, 25, false, false}, {"a", 0.0205, 21, true, true}, {"b", 0.90, 40, true, false}}

func (fx *heFx) tableLabel(table store.HistoryRetentionTable) string {
	if table == store.HistoryRetentionCodexUsage {
		return string(table) + "/" + fx.codex
	}
	return string(table)
}

// runCase runs one warm case on the fixture as it stands; seedD is the time the fixture that this
// case starts from took to seed.
func (fx *heFx) runCase(spec heSpec, cand string, c heCase, seedD time.Duration) {
	fx.t.Helper()
	cutoff, eligibleExp := heCaseCutoff(spec, c.frac)
	if c.drain {
		c.ticks = max(c.ticks, int((eligibleExp+int64(heMaxBatch)-1)/int64(heMaxBatch)))
	}
	expired := fx.one(`SELECT count(*) FROM `+string(spec.table)+` WHERE `+heEligibleWhere(spec.table), cutoff)
	before := fx.one(`SELECT count(*) FROM ` + string(spec.table))
	ex, err := fx.explainE(spec.table, cutoff, heMaxBatch)
	if err != nil {
		fx.t.Fatalf("warm %s %s case %s: %v", spec.table, cand, c.name, err)
	}
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
	kv := map[string]any{"table": fx.tableLabel(spec.table), "cand": cand, "case": c.name, "ticks": c.ticks, "seed_seconds": seedD.Seconds(),
		"expired_before": expired, "eligible_expected": eligibleExp, "count_before": before, "count_after": after, "confirmed_deleted": deleted,
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
// table and candidate index. Each (table, candidate) starts from a freshly seeded fixture, and every
// committing case, (a) and (b), is preceded by its own reseed (which ends in VACUUM (ANALYZE)) because it deletes
// committed rows; the seed time each case started from is its seed_seconds.
func TestHistoryRetentionWarmEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	for _, table := range fx.tables {
		spec := fx.stdSpec(table)
		for _, cand := range fx.cands {
			seedD := fx.reseed(spec)
			build := fx.setIndex(table, cand)
			kv := fx.sizes(table, cand).kv()
			kv["table"], kv["cand"], kv["build_ms"], kv["seed_seconds"] = fx.tableLabel(table), cand, heMS(build), seedD.Seconds()
			fx.log.rec("sizes", kv)
			for _, c := range heWarmCases {
				caseSeed := seedD
				if c.commit {
					caseSeed = fx.reseed(spec)
				}
				fx.runCase(spec, cand, c, caseSeed)
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
	batch   map[store.HistoryRetentionTable]*historyretention.BatchController
}

func newPassState(tables []store.HistoryRetentionTable) *hePassState {
	ps := &hePassState{batch: map[store.HistoryRetentionTable]*historyretention.BatchController{}}
	for _, t := range tables {
		ps.batch[t] = historyretention.NewBatchController()
	}
	return ps
}

// passTick runs one emulated tick over the selected tables: visit in counter order, one adaptive
// batch per table, continue past a known rollback, end at an unknown outcome or the work deadline.
// Session open and close failures belong to no table and are returned apart as passErrs.
func (fx *heFx) passTick(state *hePassState, cutoffs map[store.HistoryRetentionTable]time.Time) (out []heTickRes, passErrs []string, el time.Duration) {
	fx.t.Helper()
	start := time.Now()
	cleanupCtx, cancelC := context.WithDeadline(fx.ctx, start.Add(2*time.Second))
	defer cancelC()
	workCtx, cancelW := context.WithDeadline(cleanupCtx, start.Add(1750*time.Millisecond))
	defer cancelW()
	sess, ok, err := store.OpenHistoryRetentionSession(workCtx, cleanupCtx, fx.pool, heStmtTimeout, heLockTimeout)
	if err != nil || !ok {
		return nil, []string{fmt.Sprintf("open ok=%v err=%v", ok, err)}, time.Since(start)
	}
	first := state.counter
	state.counter++
	all := store.HistoryRetentionTables()
	for i := 0; i < len(all) && workCtx.Err() == nil; i++ {
		table := all[(first+uint64(i))%uint64(len(all))]
		cutoff, sel := cutoffs[table]
		if !sel {
			continue
		}
		ctl := state.batch[table]
		r := heTickRes{Table: table, Batch: ctl.Limit()}
		r.N, r.Err = fx.deleteOn(workCtx, sess, table, cutoff, r.Batch, &r)
		switch r.Err {
		case "":
			ctl.Succeeded(r.Batch, r.N)
		case "57014":
			ctl.TimedOut(r.Batch)
		}
		out = append(out, r)
		if strings.HasPrefix(r.Err, "unknown") {
			break
		}
	}
	if _, err := sess.Close(cleanupCtx); err != nil {
		passErrs = append(passErrs, "close: "+err.Error())
	}
	return out, passErrs, time.Since(start)
}

// Delete outcome kinds of one table in one tick (heErrKind of heTickRes.Err).
const (
	heKindOK       = ""
	heKindUnknown  = "unknown"
	heKindTimeout  = "statement_timeout"
	heKindRollback = "other_rollback" // any other known rollback code, lock timeout 55P03 included
	heKindSkipped  = "skipped"
)

// heErrKind classifies a tick error string. Only an empty error is a confirmed count.
func heErrKind(e string) string {
	switch {
	case e == "":
		return heKindOK
	case e == "57014":
		return heKindTimeout
	case e == "skipped":
		return heKindSkipped
	case strings.HasPrefix(e, "unknown"):
		return heKindUnknown
	}
	return heKindRollback
}

// How a compressed run's tick loop ended.
const (
	heStopComplete     = "ticks_complete"
	heStopWallCap      = "wall_cap"
	heStopArrivalError = "arrival_error"
)

// heRunFacts is what one table's compressed run observed, the input of the run label.
type heRunFacts struct {
	Required, Confirmed int // ticks required; ticks that ended in a confirmed count
	Stop                string
	Unknown, Timeouts   int
	Rollbacks, Skipped  int
	Residual            int64 // rows still eligible at the probe cutoff
	PassErrors          int
	ProbeErrors         int
	NoopDeleted         int64
}

// heRunReasons lists everything that makes the run itself non-qualifying. A tick counts toward the
// required count only on a confirmed count, and every failed delete outcome, a wall-clock or error
// stop, a residual backlog, a pass error, a probe error or a probe that deleted rows disqualifies the
// run even when the required count was reached.
func heRunReasons(f heRunFacts) []string {
	r := []string{}
	if f.Confirmed < f.Required {
		r = append(r, fmt.Sprintf("ticks short: %d confirmed of %d required", f.Confirmed, f.Required))
	}
	if f.Stop != heStopComplete {
		r = append(r, "stop_reason="+f.Stop)
	}
	for _, c := range []struct {
		n    int
		what string
	}{{f.Unknown, "unknown delete outcomes"}, {f.Timeouts, "statement timeouts"}, {f.Rollbacks, "other known rollbacks (lock timeouts included)"},
		{f.Skipped, "skipped table ticks"}, {f.PassErrors, "pass errors"}, {f.ProbeErrors, "probe errors"}} {
		if c.n > 0 {
			r = append(r, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	if _, reason := heNearEmptyGate(f.Residual); reason != "" {
		r = append(r, reason)
	}
	if f.NoopDeleted > 0 {
		r = append(r, fmt.Sprintf("near-empty probes deleted %d rows", f.NoopDeleted))
	}
	return r
}

type heTabRun struct {
	spec      heSpec
	arrivals  int
	cutoff0   time.Time // the initial cutoff: the backlog is every eligible row below it
	eligible0 int64
	required  int
	delByTick []int64
	msByTick  []float64
	batchByTk []int32
	errByTick []string
	// firstShort is the first tick whose batch came back short and clean, -1 if none. A short batch
	// can precede more backlog (a lock skipped rows), so it is informational and never a drain claim.
	firstShort int
}

// kinds counts the delete outcome kinds over the observed ticks.
func (r *heTabRun) kinds() (confirmed, unknown, timeouts, rollbacks, skipped, lockTO int) {
	for _, e := range r.errByTick {
		switch heErrKind(e) {
		case heKindOK:
			confirmed++
		case heKindUnknown:
			unknown++
		case heKindTimeout:
			timeouts++
		case heKindSkipped:
			skipped++
		default:
			rollbacks++
			if e == "55P03" {
				lockTO++
			}
		}
	}
	return
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

// tableStats reads the table's statistics counters; when they cannot be read it returns only
// obs_unavailable, never zeros.
func (fx *heFx) tableStats(table store.HistoryRetentionTable) map[string]any {
	var live, dead, av, aav, ins int64
	if err := fx.pool.QueryRow(fx.ctx, `SELECT n_live_tup, n_dead_tup, autovacuum_count, autoanalyze_count, n_tup_ins
	        FROM pg_stat_user_tables WHERE relname = $1`, string(table)).Scan(&live, &dead, &av, &aav, &ins); err != nil {
		return map[string]any{"obs_unavailable": "pg_stat_user_tables: " + err.Error()}
	}
	return map[string]any{"n_live_tup": live, "n_dead_tup": dead, "autovacuum_count": av, "autoanalyze_count": aav, "n_tup_ins": ins}
}

// TestHistoryRetentionCompressedEvidenceLiveDB runs the compressed-time drain over the selected
// tables together, one candidate profile at a time (the same candidate on every selected table).
// Mode backlog starts with a 90% backlog (case b); mode churn starts with a 5% backlog and then
// keeps arrivals and expiries in step for heRequiredTicks ticks (158,334 for pr_history_events at the
// defaults), so an all-table churn run at the default UZI_EVIDENCE_MAX_MINUTES=20 is wall-capped and
// non-qualifying: raise the cap and/or narrow the tables. Arrivals per 15 s tick:
// 250 per sample table and UZI_EVIDENCE_PR_ARRIVALS (default 6) for pr_history_events. The cutoff
// advances 15 s per tick. Default autovacuum runs and no manual VACUUM is issued during the run.
// The tick budget defaults to the most any selected table needs (heRequiredTicks); a run whose
// arrival rate the batch cannot outpace is refused before anything is seeded.
func TestHistoryRetentionCompressedEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	mode := os.Getenv("UZI_EVIDENCE_MODE")
	if mode != "backlog" && mode != "churn" {
		t.Fatalf("UZI_EVIDENCE_MODE must be backlog or churn, got %q", mode)
	}
	offFrac := 0.05
	if mode == "backlog" {
		offFrac = 0.90
	}
	if os.Getenv("UZI_EVIDENCE_BACKLOG_PREFLIGHT") == "1" {
		if mode != "backlog" {
			t.Fatalf("UZI_EVIDENCE_BACKLOG_PREFLIGHT=1 needs UZI_EVIDENCE_MODE=backlog, got %q", mode)
		}
		for _, table := range fx.tables {
			for _, cand := range fx.cands {
				fx.preflight(table, cand, offFrac)
			}
		}
		return
	}
	required := map[store.HistoryRetentionTable]int{}
	defTicks := 0
	for _, table := range fx.tables {
		spec := fx.stdSpec(table)
		n, err := heRequiredTicks(spec, offFrac, mode, spec.arrivals)
		if err != nil {
			t.Fatalf("refusing to run: %v", err)
		}
		required[table] = n
		defTicks = max(defTicks, n)
	}
	ticks := heEnvInt("UZI_EVIDENCE_TICKS", defTicks)
	wall := time.Duration(heEnvInt("UZI_EVIDENCE_MAX_MINUTES", 20)) * time.Minute
	for _, cand := range fx.cands {
		fx.compressedRun(mode, cand, ticks, offFrac, wall, required)
	}
}

// preflight is the support check of one (table, candidate) before a backlog drain is attempted: on a
// freshly seeded fixture it runs one production DeleteBatch at the initial backlog cutoff (the
// production 300 ms statement and 200 ms lock timeouts) and records whether it timed out (57014) and
// what the outcome was; on a second fresh fixture it runs the rolled-back EXPLAIN (ANALYZE, BUFFERS)
// of the same ordered statement. Nothing loops. An error of the preflight itself is a run reason of
// the record, never a silent gap, and a timeout is a result, not an error.
func (fx *heFx) preflight(table store.HistoryRetentionTable, cand string, offFrac float64) {
	fx.t.Helper()
	spec := fx.stdSpec(table)
	cutoff, eligible := heCaseCutoff(spec, offFrac)
	reasons := []string{}
	fx.reseed(spec)
	fx.setIndex(table, cand)
	r := fx.oneTick(table, cutoff, heMaxBatch)
	outcome := "confirmed"
	switch heErrKind(r.Err) {
	case heKindOK:
	case heKindTimeout:
		outcome = "timed_out"
	case heKindUnknown:
		outcome = "unknown"
		reasons = append(reasons, "delete outcome unknown: "+r.Err)
	default:
		outcome = "rolled_back:" + r.Err
		reasons = append(reasons, "delete rolled back with "+r.Err)
	}
	kv := map[string]any{"table": fx.tableLabel(table), "cand": cand, "cutoff": cutoff.UTC().Format(time.RFC3339Nano),
		"rows": spec.n, "eligible_expected": eligible, "timed_out": r.Err == "57014", "outcome": outcome, "deleted": r.N,
		"open_ms": r.OpenMS, "delete_ms": r.DelMS, "close_ms": r.CloseMS, "err": r.Err}
	fx.reseed(spec)
	fx.setIndex(table, cand)
	ex, err := fx.explainE(table, cutoff, heMaxBatch)
	if err != nil {
		reasons = append(reasons, "explain: "+err.Error())
	} else {
		for k, v := range ex.kv() {
			kv[k] = v
		}
	}
	kv["run_reasons"], kv["coverage_reasons"] = reasons, fx.coverageReasons(false, nil)
	fx.log.rec("preflight", kv)
	fx.t.Logf("preflight %s %s: outcome %s timed_out %v deleted %d delete %.1f ms; run_reasons %v",
		fx.tableLabel(table), cand, outcome, r.Err == "57014", r.N, r.DelMS, reasons)
}

func (fx *heFx) compressedRun(mode, cand string, ticks int, offFrac float64, wall time.Duration, required map[store.HistoryRetentionTable]int) {
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
		cutoff0, eligible0 := heCaseCutoff(spec, offFrac)
		runs[table] = &heTabRun{spec: spec, arrivals: spec.arrivals, cutoff0: cutoff0, eligible0: eligible0, required: required[table], firstShort: -1}
		kv := fx.sizes(table, cand).kv()
		kv["table"], kv["cand"], kv["mode"], kv["after"] = fx.tableLabel(table), cand, mode, "run_start"
		kv["autovacuum_off"], kv["reloptions"] = fx.autovacuumState(table)
		kv["server_version"] = fx.serverVersion()
		kv["eligible_initial"], kv["ticks_required"] = eligible0, required[table]
		for k, v := range fx.tableStats(table) {
			kv[k] = v
		}
		fx.log.rec("sizes", kv)
	}
	state := newPassState(fx.tables)
	deadline := time.Now().Add(wall)
	// Arrivals run under the run's wall deadline too, so a stuck insert cannot outlive the cap.
	arrCtx, cancelArr := context.WithDeadline(fx.ctx, deadline)
	defer cancelArr()
	done := 0
	stop, stopDetail := heStopComplete, ""
	var passMS []float64
	var passErrs []string
	insMS := map[store.HistoryRetentionTable][]float64{}
	var slowSamples []map[string]any
tickLoop:
	for k := 0; k < ticks; k++ {
		if !time.Now().Before(deadline) {
			stop = heStopWallCap
			break
		}
		cutoffs := map[store.HistoryRetentionTable]time.Time{}
		for _, table := range fx.tables {
			r := runs[table]
			t0 := time.Now()
			if err := fx.insertRowsCtx(arrCtx, r.spec, int64(r.spec.n+k*r.arrivals), int64(r.spec.n+(k+1)*r.arrivals)); err != nil {
				stop, stopDetail = heStopArrivalError, fmt.Sprintf("arrivals of %s at tick %d: %v", heShort(table), k, err)
				if errors.Is(err, context.DeadlineExceeded) || arrCtx.Err() != nil {
					stop = heStopWallCap
				}
				break tickLoop
			}
			insMS[table] = append(insMS[table], heMS(time.Since(t0)))
			cutoffs[table] = r.cutoff0.Add(time.Duration(k+1) * heTick)
		}
		res, perrs, el := fx.passTick(state, cutoffs)
		passMS = append(passMS, heMS(el))
		for _, e := range perrs {
			passErrs = append(passErrs, fmt.Sprintf("tick %d: %s", k, e))
		}
		got := map[store.HistoryRetentionTable]heTickRes{}
		slow := false
		for _, x := range res {
			got[x.Table] = x
			slow = slow || x.DelMS > 100
		}
		if slow && len(slowSamples) < 300 {
			// What the server was doing when a batch was slow: autovacuum, checkpoints.
			sample := map[string]any{"tick": k, "pass_ms": heMS(el)}
			var vac, ckpt, workers int64
			if err := fx.pool.QueryRow(fx.ctx, `SELECT (SELECT count(*) FROM pg_stat_progress_vacuum),
			        (SELECT num_timed + num_requested FROM pg_stat_checkpointer),
			        (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'autovacuum worker')`).Scan(&vac, &ckpt, &workers); err != nil {
				sample["obs_unavailable"] = err.Error()
			} else {
				sample["vacuum_running"], sample["checkpoints"], sample["av_workers"] = vac, ckpt, workers
			}
			ms := map[string]float64{}
			for _, x := range res {
				ms[heShort(x.Table)] = x.DelMS
			}
			sample["ms"] = ms
			slowSamples = append(slowSamples, sample)
		}
		for _, table := range fx.tables {
			r := runs[table]
			x, ok := got[table]
			if !ok { // not reached this tick (deadline, earlier unknown outcome or a failed session open)
				x = heTickRes{Table: table, Err: "skipped"}
			}
			r.delByTick = append(r.delByTick, x.N)
			r.msByTick = append(r.msByTick, x.DelMS)
			r.batchByTk = append(r.batchByTk, x.Batch)
			r.errByTick = append(r.errByTick, x.Err)
			if r.firstShort < 0 && x.Err == "" && x.N < int64(x.Batch) {
				r.firstShort = k
			}
		}
		done = k + 1
	}
	fx.log.rec("slow_ticks", map[string]any{"mode": mode, "cand": cand, "samples": slowSamples})
	fx.log.rec("compressed_pass", map[string]any{"mode": mode, "cand": cand, "ticks_done": done, "ticks_planned": ticks,
		"stop_reason": stop, "stop_detail": stopDetail, "pass_errors": len(passErrs), "pass_error_sample": heSample(passErrs),
		"pass_median_ms": heQuant(passMS, 0.5), "pass_max_ms": heMaxF(passMS), "pass_p99_ms": heQuant(passMS, 0.99)})
	// Reporting sequence, one order: the loop has ended; count what is still eligible at the probe
	// cutoff; probe near-empty ticks only on a drained table; only then is a label emitted.
	probes := map[store.HistoryRetentionTable]heProbe{}
	for _, table := range fx.tables {
		probes[table] = fx.nearEmptyProbe(mode, cand, table, runs[table], done)
	}
	for _, table := range fx.tables {
		fx.reportCompressed(mode, cand, table, runs[table], heOutcome{done: done, planned: ticks, stop: stop, stopDetail: stopDetail,
			insMS: insMS[table], probe: probes[table], passErrs: passErrs})
	}
}

// heSample returns up to the first three entries of xs.
func heSample(xs []string) []string { return append([]string{}, xs[:min(len(xs), 3)]...) }

// heProbe is the pre-probe residual count and, on a drained table, the near-empty probes.
type heProbe struct {
	Residual    int64
	Valid       bool // residual 0, so the probes ran and mean what they say
	Errors      []string
	NoopDeleted int64
}

// heNearEmptyGate decides whether the near-empty probes may run: only on a drained table. The reason
// is the run reason a residual backlog adds.
func heNearEmptyGate(residual int64) (runProbes bool, reason string) {
	if residual > 0 {
		return false, fmt.Sprintf("residual backlog of %d eligible rows at the probe cutoff", residual)
	}
	return true, ""
}

// nearEmptyProbe counts the rows still eligible at the cutoff of the last tick (everything below it
// should be gone). With any left no near-empty probe runs: a probe that deletes is not near-empty.
func (fx *heFx) nearEmptyProbe(mode, cand string, table store.HistoryRetentionTable, r *heTabRun, done int) heProbe {
	fx.t.Helper()
	cut := r.cutoff0.Add(time.Duration(done) * heTick)
	var p heProbe
	p.Residual = fx.one(`SELECT count(*) FROM `+string(table)+` WHERE `+heEligibleWhere(table), cut)
	kv := map[string]any{"table": fx.tableLabel(table), "cand": cand, "mode": mode, "residual_backlog": p.Residual}
	if runProbes, _ := heNearEmptyGate(p.Residual); !runProbes {
		kv["near_empty_valid"] = false
	} else {
		p.Valid = true
		kv["near_empty_valid"] = true
		x, err := fx.explainE(table, cut, heMaxBatch)
		if err != nil {
			p.Errors = append(p.Errors, "explain: "+err.Error())
		} else {
			for a, b := range x.kv() {
				kv[a] = b
			}
		}
		var del []float64
		for i := 0; i < 5; i++ {
			x := fx.oneTick(table, cut, heMaxBatch)
			del = append(del, x.DelMS)
			p.NoopDeleted += x.N
			if x.Err != "" {
				p.Errors = append(p.Errors, x.Err)
			}
		}
		kv["noop_median_ms"], kv["noop_max_ms"], kv["noop_deleted"] = heQuant(del, 0.5), heMaxF(del), p.NoopDeleted
		kv["probe_errors"], kv["probe_error_sample"] = len(p.Errors), heSample(p.Errors)
	}
	for a, b := range fx.sizes(table, cand).kv() {
		kv[a] = b
	}
	for a, b := range fx.tableStats(table) {
		kv[a] = b
	}
	fx.log.rec("near_empty", kv)
	return p
}

// heOutcome is the run-wide part of a compressed report.
type heOutcome struct {
	done, planned int
	stop          string
	stopDetail    string
	insMS         []float64
	probe         heProbe
	passErrs      []string
}

// reportCompressed emits the label of one table. The drain claim is the residual: nothing eligible
// at the probe cutoff after the whole observed window. Cycle statistics before the first short
// batch are informational only.
func (fx *heFx) reportCompressed(mode, cand string, table store.HistoryRetentionTable, r *heTabRun, o heOutcome) {
	fx.t.Helper()
	done := o.done
	end := done // informational window: ticks before the first clean short batch
	if r.firstShort >= 0 {
		end = r.firstShort
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
	confirmed, unknown, timeouts, rollbacks, skipped, lockTO := r.kinds()
	var total int64
	for _, d := range r.delByTick {
		total += d
	}
	steady := r.msByTick
	if r.firstShort >= 0 && r.firstShort < len(steady) {
		steady = steady[r.firstShort:]
	}
	over300 := 0
	for _, m := range r.msByTick {
		if m > 300 {
			over300++
		}
	}
	covered := 1.0
	if r.required > 0 {
		covered = min(1, float64(confirmed)/float64(r.required))
	}
	kv := map[string]any{"table": fx.tableLabel(table), "cand": cand, "mode": mode, "ticks_done": done, "ticks_planned": o.planned,
		"ticks_required": r.required, "ticks_confirmed": confirmed, "window_covered_fraction": covered,
		"stop_reason": o.stop, "stop_detail": o.stopDetail,
		"arrivals_per_tick": r.arrivals, "eligible_initial": r.eligible0,
		"window_deleted": total, "window_arrivals": int64(done * r.arrivals),
		"residual_backlog": o.probe.Residual, "near_empty_valid": o.probe.Valid,
		"probe_errors": len(o.probe.Errors), "probe_error_sample": heSample(o.probe.Errors), "probe_noop_deleted": o.probe.NoopDeleted,
		"pass_errors": len(o.passErrs), "pass_error_sample": heSample(o.passErrs),
		"first_short_batch_tick": r.firstShort, "info_cycles": cycles, "info_cycles_not_exceeding": badCycles,
		"info_worst_cycle_deficit": worstDeficit, "info_pre_short_deleted": cum,
		"timeouts_57014": timeouts, "unknown": unknown, "other_rollbacks": rollbacks, "lock_timeouts_55P03": lockTO,
		"skipped": skipped, "ticks_over_300ms": over300,
		"del_median_ms": heQuant(r.msByTick, 0.5), "del_p99_ms": heQuant(r.msByTick, 0.99), "del_max_ms": heMaxF(r.msByTick),
		"steady_median_ms": heQuant(steady, 0.5), "steady_p99_ms": heQuant(steady, 0.99), "steady_max_ms": heMaxF(steady),
		"insert_median_ms": heQuant(o.insMS, 0.5), "insert_max_ms": heMaxF(o.insMS)}
	coverage := fx.coverageReasons(false, nil)
	run := heRunReasons(heRunFacts{Required: r.required, Confirmed: confirmed, Stop: o.stop, Unknown: unknown, Timeouts: timeouts,
		Rollbacks: rollbacks, Skipped: skipped, Residual: o.probe.Residual, PassErrors: len(o.passErrs),
		ProbeErrors: len(o.probe.Errors), NoopDeleted: o.probe.NoopDeleted})
	kv["coverage_reasons"], kv["run_reasons"], kv["qualifying"] = coverage, run, len(coverage) == 0 && len(run) == 0
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
	if len(r.delByTick) <= 400 {
		kv["delete_per_tick"] = r.delByTick
	}
	fx.log.rec("compressed", kv)
	fx.t.Logf("compressed %s %s %s: ticks %d/%d (required %d, confirmed %d) stop %s residual %d, steady median %.1f max %.1f ms, 57014=%d qualifying=%v",
		mode, fx.tableLabel(table), cand, done, o.planned, r.required, confirmed, o.stop, o.probe.Residual,
		heQuant(steady, 0.5), heMaxF(steady), timeouts, len(coverage) == 0 && len(run) == 0)
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

// heSmokeFacts is what a smoke run observed about itself.
type heSmokeFacts struct {
	GatePass     bool
	TickErrors   int
	UnknownLines int
	TimeoutLines int
	Ticks        int
	Seconds      int // the configured smoke length
}

// heSmokeRunReasons lists everything that makes a smoke run non-qualifying: a failed gate, any pass
// error, any unknown-outcome or repeated-timeout log line, and fewer ticks than the configured length
// allows (one per 15 s, less one for the engine's start edge).
func heSmokeRunReasons(f heSmokeFacts) []string {
	r := []string{}
	if !f.GatePass {
		r = append(r, "smoke gate failed")
	}
	for _, c := range []struct {
		n    int
		what string
	}{{f.TickErrors, "tick errors"}, {f.UnknownLines, "unknown delete outcome log lines"}, {f.TimeoutLines, "repeated statement timeout log lines"}} {
		if c.n > 0 {
			r = append(r, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	if floor := f.Seconds/int(heTick/time.Second) - 1; f.Ticks < floor {
		r = append(r, fmt.Sprintf("ticks observed %d below the floor of %d", f.Ticks, floor))
	}
	return r
}

// TestHistoryRetentionSmokeEvidenceLiveDB runs the production historyretention.Pass under a real
// sweeper.Engine (stub Sweeper, a timing decorator around the unchanged Pass) at the production
// 15 s interval, driving Run only, retention 24h, on the 1M-row fixture with the chosen indexes.
// Each table holds 30,000 expired rows (pr_history_events: ordinary, not first_merge) and no
// writer runs.
func TestHistoryRetentionSmokeEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	idx, err := heParseSmokeIndexes(os.Getenv("UZI_EVIDENCE_SMOKE_INDEXES"))
	if err != nil {
		t.Fatal(err)
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
		var dbCutoff time.Time
		if err := fx.pool.QueryRow(fx.ctx, `SELECT now() - interval '24 hours'`).Scan(&dbCutoff); err != nil {
			t.Fatalf("read the database clock: %v", err)
		}
		expired := fx.one(`SELECT count(*) FROM `+string(table)+` WHERE `+heEligibleWhere(table), dbCutoff)
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
	coverage := fx.coverageReasons(true, idx)
	run := heSmokeRunReasons(heSmokeFacts{GatePass: pass2, TickErrors: len(errTicks), Ticks: len(recs), Seconds: secs,
		UnknownLines: strings.Count(logs, "unknown delete outcome"), TimeoutLines: strings.Count(logs, "repeated statement timeouts")})
	kv["coverage_reasons"], kv["run_reasons"], kv["qualifying"] = coverage, run, len(coverage) == 0 && len(run) == 0
	av := map[string]bool{}
	for _, table := range store.HistoryRetentionTables() {
		off, _ := fx.autovacuumState(table)
		av[string(table)] = off
	}
	kv["autovacuum_off"] = av
	fx.log.rec("smoke", kv)
	fx.log.rec("smoke_slog", map[string]any{"log": truncateTail(logs, 60_000)})
	t.Logf("smoke: %d ticks, max %.0f ms, deltas %v, confirmed %d <= %d, pass=%v qualifying=%v", len(recs), maxDur, deltas, sumConfirmed, sumDelta, pass2, len(coverage) == 0 && len(run) == 0)
	if len(coverage) > 0 {
		t.Logf("smoke is diagnostic only, not qualifying evidence: %s", strings.Join(coverage, "; "))
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

// heBindingTable holds one row per table: the published, single-use binding between a cold setup and
// the one measurement allowed to use it. It lives in hrev's uzi_test_harness schema, the schema the
// evidence marker is in, and reseed deletes the table's row before it touches the fixture.
const heBindingTable = "uzi_test_harness.cold_binding"

func (fx *heFx) ensureBindingTable() {
	fx.t.Helper()
	fx.exec(`CREATE SCHEMA IF NOT EXISTS uzi_test_harness`)
	fx.exec(`CREATE TABLE IF NOT EXISTS ` + heBindingTable + ` (
	        table_name text PRIMARY KEY, setup_id uuid NOT NULL UNIQUE, candidate text NOT NULL,
	        row_count integer NOT NULL, arrivals integer NOT NULL, codex_variant text NOT NULL,
	        start_at timestamptz NOT NULL, cutoff_c timestamptz NOT NULL, cutoff_a timestamptz NOT NULL,
	        cutoff_b timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), consumed_at timestamptz)`)
}

// heColdBinding is what a cold setup publishes: the fixture parameters a measurement must match and
// the cutoffs of cases c, a and b computed from that fixture.
type heColdBinding struct {
	SetupID                   uuid.UUID
	Table                     store.HistoryRetentionTable
	Cand                      string
	Rows, Arrivals            int
	Codex                     string
	Start                     time.Time
	CutoffC, CutoffA, CutoffB time.Time
}

// heColdCheck compares the published binding with the parameters the measurement would use now: the
// table, candidate, row count, arrivals, codex variant and fixture start must all match exactly. It is
// pure and runs BEFORE the binding is consumed, so a mismatch burns nothing.
func heColdCheck(got, want heColdBinding) error {
	var bad []string
	if got.Table != want.Table {
		bad = append(bad, fmt.Sprintf("table %q, now %q", got.Table, want.Table))
	}
	if got.Cand != want.Cand {
		bad = append(bad, fmt.Sprintf("candidate %q, now %q", got.Cand, want.Cand))
	}
	if got.Rows != want.Rows {
		bad = append(bad, fmt.Sprintf("rows %d, now %d", got.Rows, want.Rows))
	}
	if got.Arrivals != want.Arrivals {
		bad = append(bad, fmt.Sprintf("arrivals %d, now %d", got.Arrivals, want.Arrivals))
	}
	if got.Codex != want.Codex {
		bad = append(bad, fmt.Sprintf("codex variant %q, now %q", got.Codex, want.Codex))
	}
	if !got.Start.Equal(want.Start) {
		bad = append(bad, fmt.Sprintf("fixture start %s, now %s", got.Start.UTC().Format(time.RFC3339Nano), want.Start.UTC().Format(time.RFC3339Nano)))
	}
	if len(bad) > 0 {
		return fmt.Errorf("the setup binding does not match this measurement: setup had %s", strings.Join(bad, "; "))
	}
	return nil
}

// cutoff returns the binding's cutoff of case c, a or b.
func (b heColdBinding) cutoff(c string) (time.Time, error) {
	switch c {
	case "a":
		return b.CutoffA, nil
	case "b":
		return b.CutoffB, nil
	case "c":
		return b.CutoffC, nil
	}
	return time.Time{}, fmt.Errorf("UZI_EVIDENCE_COLD_CASE must be a, b or c, got %q", c)
}

// requireBuiltIndex checks the post-build state of the table: the candidate's index exists and is
// valid and ready, or, for none, none of the candidate indexes exists.
func (fx *heFx) requireBuiltIndex(table store.HistoryRetentionTable, cand string) {
	fx.t.Helper()
	if cand == "none" {
		for _, c := range heCands {
			if c == "none" {
				continue
			}
			var present bool
			if err := fx.pool.QueryRow(fx.ctx, `SELECT to_regclass($1) IS NOT NULL`, heIdxName(table, c)).Scan(&present); err != nil {
				fx.t.Fatalf("index check: %v", err)
			}
			if present {
				fx.t.Fatalf("setup left index %s on %s for candidate none", heIdxName(table, c), table)
			}
		}
		return
	}
	var ok bool
	err := fx.pool.QueryRow(fx.ctx, `SELECT i.indisvalid AND i.indisready FROM pg_index i WHERE i.indexrelid = to_regclass($1)`,
		heIdxName(table, cand)).Scan(&ok)
	if err != nil || !ok {
		fx.t.Fatalf("setup index %s on %s is not valid and ready (valid_and_ready=%v err=%v)", heIdxName(table, cand), table, ok, err)
	}
}

// claimBinding reads the binding setup_id published for table over a pool of its own (so fx.pool's
// first work stays the measured tick), refuses a missing, consumed or mismatching one, and only then
// marks it consumed. The mark is a single conditional UPDATE: zero rows means someone else got there.
func (fx *heFx) claimBinding(setupID uuid.UUID, table store.HistoryRetentionTable, want heColdBinding) heColdBinding {
	fx.t.Helper()
	p, err := pgxpool.New(fx.ctx, fx.dsn)
	if err != nil {
		fx.t.Fatalf("binding pool: %v", err)
	}
	defer p.Close()
	fx.requireEvidenceDatabase(p)
	b := heColdBinding{SetupID: setupID}
	var name string
	var consumed *time.Time
	err = p.QueryRow(fx.ctx, `SELECT table_name, candidate, row_count, arrivals, codex_variant, start_at, cutoff_c, cutoff_a, cutoff_b, consumed_at
	        FROM `+heBindingTable+` WHERE setup_id = $1 AND table_name = $2`, setupID, string(table)).
		Scan(&name, &b.Cand, &b.Rows, &b.Arrivals, &b.Codex, &b.Start, &b.CutoffC, &b.CutoffA, &b.CutoffB, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		fx.t.Fatalf("refusing: no binding for setup id %s on %s; it was never published or a reseed of the table removed it. Re-run the setup", setupID, table)
	}
	if err != nil {
		fx.t.Fatalf("refusing: cannot read the binding of setup id %s (was a setup ever run?): %v", setupID, err)
	}
	b.Table = store.HistoryRetentionTable(name)
	if consumed != nil {
		fx.t.Fatalf("refusing: setup id %s was already consumed at %s. Re-run the setup", setupID, consumed.UTC().Format(time.RFC3339))
	}
	if err := heColdCheck(b, want); err != nil {
		fx.t.Fatalf("refusing: %v. The binding is left unconsumed", err)
	}
	tag, err := p.Exec(fx.ctx, `UPDATE `+heBindingTable+` SET consumed_at = now() WHERE setup_id = $1 AND table_name = $2 AND consumed_at IS NULL`,
		setupID, string(table))
	if err != nil {
		fx.t.Fatalf("refusing: claim of setup id %s failed: %v", setupID, err)
	}
	if tag.RowsAffected() != 1 {
		fx.t.Fatalf("refusing: setup id %s was consumed or removed while it was being claimed. Re-run the setup", setupID)
	}
	return b
}

// TestHistoryRetentionColdEvidenceLiveDB has two modes selected by UZI_EVIDENCE_COLD_TABLE (pr,
// anthropic, codex or worker) and UZI_EVIDENCE_COLD_SETUP. Both need exactly one candidate in
// UZI_EVIDENCE_CANDS.
//
//	UZI_EVIDENCE_COLD_SETUP=1  delete the table's binding (reseed does), reseed the table, build the
//	                           candidate index, check it is valid and ready (or absent for none),
//	                           CHECKPOINT, then publish one binding with a fresh random setup id in a
//	                           single autocommitted INSERT and log that id
//	otherwise                  needs UZI_EVIDENCE_COLD_SETUP_ID. Run EXACTLY one tick of
//	                           UZI_EVIDENCE_COLD_CASE (a, b or c), as the first work on the
//	                           measurement pool, after a shell script restarted the container. The
//	                           binding is read first, compared with this invocation's table,
//	                           candidate, rows, arrivals, codex variant and fixture start, and only a
//	                           matching, unconsumed binding is claimed (consumed_at set) before the
//	                           DELETE runs. The cutoff and candidate in the record come from the
//	                           binding. The marker check and the binding read use backends of their
//	                           own; UZI_EVIDENCE_COLD_LABEL is a free-text note carried into the record.
//
// A binding is single use and a reseed of its table (any harness phase) removes it. This is ordinary
// PostgreSQL commit semantics and nothing stronger: an interruption leaves the binding absent,
// consumed, or published and unclaimed. The first two refuse a measurement, so re-run the setup; the
// third is the state the measurement expects.
func TestHistoryRetentionColdEvidenceLiveDB(t *testing.T) {
	heRequireOptIn(t)
	table, err := heTableByShort(os.Getenv("UZI_EVIDENCE_COLD_TABLE"))
	if err != nil {
		t.Fatalf("UZI_EVIDENCE_COLD_TABLE: %v", err)
	}
	if os.Getenv("UZI_EVIDENCE_COLD_SETUP") == "1" {
		fx := newHeFx(t, true)
		if len(fx.cands) != 1 {
			t.Fatalf("cold setup needs exactly one candidate in UZI_EVIDENCE_CANDS, got %v", fx.cands)
		}
		cand := fx.cands[0]
		spec := fx.stdSpec(table)
		fx.reseed(spec) // deletes the table's binding before the fixture changes
		fx.setIndex(table, cand)
		fx.requireBuiltIndex(table, cand)
		fx.exec(`CHECKPOINT`)
		kv := fx.sizes(table, cand).kv()
		kv["table"], kv["cand"], kv["after"] = fx.tableLabel(table), cand, "cold_setup"
		fx.log.rec("sizes", kv)
		cutC, _ := heCaseCutoff(spec, 0)
		cutA, _ := heCaseCutoff(spec, 0.0205)
		cutB, _ := heCaseCutoff(spec, 0.90)
		setupID := uuid.New()
		fx.exec(`INSERT INTO `+heBindingTable+` (setup_id, table_name, candidate, row_count, arrivals, codex_variant, start_at, cutoff_c, cutoff_a, cutoff_b)
		        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			setupID, string(table), cand, spec.n, spec.arrivals, fx.codex, spec.start, cutC, cutA, cutB)
		fx.log.rec("cold_setup", map[string]any{"table": fx.tableLabel(table), "cand": cand, "setup_id": setupID.String(),
			"rows": spec.n, "arrivals": spec.arrivals, "codex": fx.codex, "start": spec.start.Format(time.RFC3339Nano)})
		t.Logf("cold setup %s cand %s published: UZI_EVIDENCE_COLD_SETUP_ID=%s", table, cand, setupID)
		return
	}
	caseName := os.Getenv("UZI_EVIDENCE_COLD_CASE")
	if caseName != "a" && caseName != "b" && caseName != "c" {
		t.Fatal("UZI_EVIDENCE_COLD_CASE must be a, b or c")
	}
	rawID := os.Getenv("UZI_EVIDENCE_COLD_SETUP_ID")
	if rawID == "" {
		t.Fatal("UZI_EVIDENCE_COLD_SETUP_ID is required for a cold measurement: run the setup (UZI_EVIDENCE_COLD_SETUP=1) and pass the setup id it logged")
	}
	setupID, err := uuid.Parse(rawID)
	if err != nil {
		t.Fatalf("UZI_EVIDENCE_COLD_SETUP_ID %q: %v", rawID, err)
	}
	fx := newHeFx(t, false) // runs the marker checks (requireEvidenceMarker, requireEvidenceDatabase)
	if len(fx.cands) != 1 {
		t.Fatalf("a cold measurement needs exactly one candidate in UZI_EVIDENCE_CANDS, got %v", fx.cands)
	}
	spec := fx.stdSpec(table)
	bind := fx.claimBinding(setupID, table, heColdBinding{Table: table, Cand: fx.cands[0], Rows: spec.n,
		Arrivals: spec.arrivals, Codex: fx.codex, Start: spec.start})
	cutoff, err := bind.cutoff(caseName)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	r := fx.oneTick(table, cutoff, heMaxBatch)
	fx.log.rec("cold", map[string]any{"table": fx.tableLabel(table), "setup_id": setupID.String(), "cand": bind.Cand,
		"cutoff": cutoff.UTC().Format(time.RFC3339Nano), "note": os.Getenv("UZI_EVIDENCE_COLD_LABEL"),
		"case": caseName, "open_ms": r.OpenMS, "delete_ms": r.DelMS, "close_ms": r.CloseMS,
		"total_ms": heMS(time.Since(t0)), "deleted": r.N, "err": r.Err})
	t.Logf("cold %s case %s setup %s: open %.1f ms delete %.1f ms deleted %d err %q", table, caseName, setupID, r.OpenMS, r.DelMS, r.N, r.Err)
}

// ---------------------------------------------------------------------------------------------
// explain cleanup

// TestHistoryRetentionExplainCleanupEvidenceLiveDB forces a failure inside explainE (a cutoff
// outside the timestamptz range is rejected by EXECUTE after the transaction and the prepared
// statement exist) on a dedicated one-connection pool, then proves on the same backend that explainE cleaned up: the
// backend is idle outside any transaction, the harness statement is gone, and a query cached before
// the failure still runs.
func TestHistoryRetentionExplainCleanupEvidenceLiveDB(t *testing.T) {
	fx := newHeFx(t, true)
	cfg, err := pgxpool.ParseConfig(fx.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(fx.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	const cached = `SELECT $1::int + 1`
	backendPID := func() int {
		var pid int
		if err := one.QueryRow(fx.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("pg_backend_pid: %v", err)
		}
		return pid
	}
	pid := backendPID()
	var got int
	if err := one.QueryRow(fx.ctx, cached, 1).Scan(&got); err != nil || got != 2 {
		t.Fatalf("cached query before the failure: %d %v", got, err)
	}

	failing := *fx
	failing.pool = one
	_, ferr := failing.explainE(store.HistoryRetentionPRHistoryEvents, time.Date(400000, 1, 1, 0, 0, 0, 0, time.UTC), heMaxBatch)
	if ferr == nil {
		t.Fatal("explainE with an out-of-range cutoff succeeded, want the forced failure")
	}
	t.Logf("forced failure: %v", ferr)

	// From the other pool: the backend is idle, not in a transaction, and its harness statement is gone.
	var state string
	var inXact bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state, xact_start IS NOT NULL FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&state, &inXact); err != nil {
		t.Fatalf("pg_stat_activity of backend %d (destroyed or never reused?): %v", pid, err)
	}
	if state != "idle" || inXact {
		t.Fatalf("backend %d state %q in_transaction %v after the failed explain, want idle outside a transaction", pid, state, inXact)
	}
	if after := backendPID(); after != pid {
		t.Fatalf("the pool handed out backend %d after the failure, want the same backend %d", after, pid)
	}
	var txid *int64
	var stmts int
	if err := one.QueryRow(fx.ctx, `SELECT txid_current_if_assigned(), (SELECT count(*) FROM pg_prepared_statements WHERE name = $1)`, heExplainStmt).Scan(&txid, &stmts); err != nil {
		t.Fatalf("session state query: %v", err)
	}
	if txid != nil || stmts != 0 {
		t.Fatalf("txid_current_if_assigned=%v harness prepared statements=%d, want null and 0", txid, stmts)
	}
	if err := one.QueryRow(fx.ctx, cached, 2).Scan(&got); err != nil || got != 3 {
		t.Fatalf("cached query after the failure: %d %v", got, err)
	}
	t.Logf("explainE cleanup verified on backend %d", pid)
}
