package store_test

// TestPoolBackendMemoryMeasurement is an OPT-IN measurement harness (issue #4591), not a
// regression test. It measures the PostgreSQL backend memory behind an api-style pgx pool
// under several pool configurations ("candidates") so an ADR can choose between them with
// numbers. It is skipped unless BOTH of these are set:
//
//	UZI_POOL_MEMORY_MEASURE=1
//	UZI_TEST_DATABASE_URL=<throwaway superuser DSN, e.g. the postgres:17 of e2e/run-store-it.sh>
//
// and it writes JSON lines to the file named by UZI_POOL_MEMORY_OUT (required). Optional
// knobs (defaults in parentheses): UZI_POOL_MEMORY_ROUNDS (20) memory rounds per candidate,
// UZI_POOL_MEMORY_LIFETIME (45s) and UZI_POOL_MEMORY_JITTER (10s) for the recycling
// candidates, UZI_POOL_MEMORY_MAX_RECYCLE_WAIT (12m) cap on one recycling candidate's run (a
// recycling candidate that hits this wait before UZI_POOL_MEMORY_ROUNDS rounds fails the
// run, so set the wait to cover the requested rounds),
// UZI_POOL_MEMORY_DISCOVER=1 (run only the breadth table once and write each entry's
// observed outcome, used to re-pin the table after a schema change), UZI_POOL_MEMORY_REPS (2)
// memory repetitions per candidate (an integer >= 1, else the test fails),
// UZI_POOL_MEMORY_PHASES (all) a comma list of memory,latency,cold,wide,stress,explain to run
// only those phases, so one run can be split into several go test invocations, and
// UZI_POOL_MEMORY_JIT (default) "default" or "off": "off" runs ALTER DATABASE <clone> SET
// jit = off on every clone right after creating it, before any measured connection opens.
// That is a measurement control only; product code and server defaults are untouched.
//
// PRIMARY METRIC: Anonymous (smaps_rollup), the process's private heap. Every headline
// number (steady state, sampled peak, the per-connection-age buckets) is keyed on Anonymous
// first, with the Private_Clean+Private_Dirty variants beside it. Private_Clean is NOT a heap
// metric: it counts file-backed pages only this backend has mapped, such as the LLVM JIT
// libraries once a backend JIT-compiles a statement (each post-round sample reads the
// backend's memory-map listing through pg_read_file and records whether any mapped path
// contains "llvm", and the first round it did). Run once with UZI_POOL_MEMORY_JIT=default and
// once with off to separate the JIT confound.
//
// Isolation: the schema and fixture are seeded ONCE into a template database named
// pm_<32 hex>_tpl on the server named by UZI_TEST_DATABASE_URL (which is also where the
// throwaway marker is checked and where CREATE/DROP DATABASE run). The 32 hex chars are a fresh
// UUID per invocation, and clones are pm_<same 32 hex>_<label>, so a run never touches a
// database that existed before it. Every CREATE/ALTER/DROP DATABASE goes through pmAdminExec,
// which rechecks the marker on its own connection and runs nothing without it. A database is
// dropped only if this run's CREATE of it succeeded; an ambiguous CREATE outcome preserves the
// database and reports the manual DROP. There is no startup cleanup: a crashed run's leftovers
// are dropped by hand, by exact prefix only (see pmLab). Measurements run on CREATE DATABASE ... TEMPLATE clones,
// each with its own observer connection, dropped afterwards. The isolation differs per phase:
//   - Memory: one clone per candidate x repetition.
//   - Latency and stress: one clone per round, shared by every candidate in that round (see
//     pmRunLatency). Later candidates in a round inherit dead tuples left by earlier
//     candidates' rolled-back writes and the heartbeat UPDATE that verifyFixture commits
//     after each candidate.
//   - Cold: a single clone shared by all candidates and repetitions.
//   - Wide: one clone per statement cache size x plan cache mode.
//   - Explain: one clone per jit mode x repetition; a capture connection runs ClaimRun on
//     the clone before the measured connection does (see pmRunExplain).
//
// The template is NOT untouched after seeding: the validation pool (store.OpenPool on the
// template DSN) reads it before the first clone, and discover mode runs the breadth table on
// it. All clones share one server, so shared_buffers, the OS page cache and the LLVM JIT
// library text are shared between them. UZI_TEST_DATABASE_URL must be a postgres:// URL.
//
// Known limits: this harness is not a production measurement.
//   - Breadth checks verify only each entry's result class (some / empty / ErrNoRows), not
//     row contents.
//   - Hot-mix and fixture checks are field-level (specific ids, flags, counts, status), not
//     full-row, and do not verify that write effects were applied as intended.
//   - Latency, stress and cold share clones as described above.
//   - The validation pool and discover mode reuse the template.
//   - Wide-phase database errors are tolerated and recorded (OtherErrors, ParseFailures)
//     instead of failing the run.
//   - Headline latency is measured under whatever UZI_POOL_MEMORY_JIT the run used (the
//     ADR's latency run used jit=off).
//   - One PostgreSQL version and one server per run.
//   - The fixture is small and synthetic, so results are synthetic observations.
//
// The server must be a disposable container whose postgres superuser may pg_read_file
// /proc/<pid>/smaps_rollup and the cgroup files: all memory numbers are read that way, from
// a SEPARATE observer connection, so the measured backend is not perturbed by its own
// measurement. The throwaway role is a superuser.
//
// plan_cache_mode: the primary workload runs at the default auto. A role- or database-level
// plan_cache_mode now fails the run (the AfterConnect check in poolConfig reads the
// backend's startup value), as does a DSN whose default_query_exec_mode or
// statement_cache_capacity would defeat the candidate's capacity label (pmCheckPoolConfig).
//
// Design (see the ADR this feeds for the interpretation):
//   - One api-style pool per candidate (MaxConns 1), built from pgxpool.ParseConfig (not
//     OpenPool; candidate A is raw pgx defaults and corresponds to the api's unchanged
//     OpenPool baseline, and B128 is a candidate only, #4591 changed no default) with the
//     candidate's StatementCacheCapacity / MaxConnLifetime / jitter set directly, plus an
//     AfterConnect hook and pgx QueryTracer that record the backend PID, connect time,
//     executions served and the distinct statement texts executed successfully per PID.
//   - Primary workload, default plan_cache_mode=auto, through the real sqlc methods: a hot
//     mix (ClaimRun, InsertRunMessage, HeartbeatWorker, ListRunsForUser,
//     ListRunUsageTotalsForRuns, SelfUsage, Ping, SchemaVersionStatus) and a curated
//     breadth table (pmBreadth) of distinct sqlc methods interleaved into the timed latency
//     loop so a capped statement cache really evicts. Every breadth and write call runs in
//     one transaction under SAVEPOINT / ROLLBACK TO SAVEPOINT, so the fixture never moves;
//     the fixture is re-verified after each round.
//   - Memory (kB, from the kernel): Private_Clean+Private_Dirty, Anonymous, Rss, Pss from
//     smaps_rollup; VmHWM (includes shared pages) from status; cgroup memory.current and
//     memory.stat anon/file. These are SEPARATE fields and are never summed together.
//     PostgreSQL-side context totals come from pg_backend_memory_contexts read through the
//     measured connection with the simple protocol (so the probe prepares nothing).
//   - Supplemental lower-bound wide-read sample: every sqlc query text, executed once per PG
//     parameter (all NULL) inside BEGIN READ ONLY, under auto and force_generic_plan. Writes
//     are refused after planning and never retained, and NULL arguments plan small, so this
//     is NOT an upper bound of plan-cache memory and NOT the decision basis.
//
// Assertions it makes (t.Fatal on a mismatch):
//   - ClaimRun returns the expected run id with status "claimed".
//   - InsertRunMessage returns inserted and generation_live both true.
//   - ListRunsForUser and ListRunUsageTotalsForRuns row counts, SelfUsage run_count, Ping,
//     SchemaVersionStatus at head, and HeartbeatWorker's returned id.
//   - Each breadth entry matches its pinned result class only (pmClassify / pmRunEntry).
//   - verifyFixture's selected aggregates: queued-run status, message counts, custody holds
//     and claim_generation.
//
// It does NOT verify complete row values or write effects. The wide phase records other
// database errors (OtherErrors / ParseFailures) rather than failing, and cleanup errors
// (rollbacks in closePool, advisory-lock and plan_cache_mode resets) are ignored.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ---------------------------------------------------------------------------------------
// Configuration

const (
	pmMinIdentities   = 160 // working-set gate: distinct statements executed per backend
	pmWarmups         = 10
	pmLatencyN        = 500
	pmLatencyRounds   = 5 // == candidate count, so every candidate leads exactly one round
	pmHotReps         = 20
	pmSampleInterval  = 100 * time.Millisecond
	pmStressBreadthK  = 40 // supplemental stress: breadth calls per iteration (> B32 cap)
	pmStressN         = 200
	pmDefaultCapacity = 512 // pgx v5.12.0 statement_cache_capacity default
)

type pmCand struct {
	Name    string        `json:"name"`
	Desc    string        `json:"desc"`
	StmtCap int           `json:"stmt_cap_configured"` // 0 = pgx default
	MaxLife time.Duration `json:"max_conn_lifetime_ns"`
	Jitter  time.Duration `json:"max_conn_lifetime_jitter_ns"`
	Health  time.Duration `json:"health_check_period_ns"`
	Recycle bool          `json:"recycle"`
}

func (c pmCand) capacity() int {
	if c.StmtCap > 0 {
		return c.StmtCap
	}
	return pmDefaultCapacity
}

func pmCandidates() []pmCand {
	life := pmEnvDuration("UZI_POOL_MEMORY_LIFETIME", 45*time.Second)
	jit := pmEnvDuration("UZI_POOL_MEMORY_JITTER", 10*time.Second)
	health := 5 * time.Second
	return []pmCand{
		{Name: "A", Desc: "pgx defaults: statement cache 512, MaxConnLifetime 1h, no jitter"},
		{Name: "B128", Desc: "statement_cache_capacity=128, otherwise defaults", StmtCap: 128},
		{Name: "B32", Desc: "statement_cache_capacity=32, otherwise defaults", StmtCap: 32},
		{Name: "C", Desc: "compressed lifetime with jitter, default cache 512", MaxLife: life, Jitter: jit, Health: health, Recycle: true},
		{Name: "D", Desc: "statement cache 128 plus compressed lifetime with jitter", StmtCap: 128, MaxLife: life, Jitter: jit, Health: health, Recycle: true},
	}
}

func pmEnvDuration(key string, def time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			panic(fmt.Sprintf("%s=%q: %v", key, s, err))
		}
		return d
	}
	return def
}

// pmEnvMinInt reads an integer knob that must be >= min; anything else is a t.Fatalf with a
// message, never a panic.
func pmEnvMinInt(t *testing.T, key string, def, minVal int) int {
	t.Helper()
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("%s=%q: not an integer (%v)", key, s, err)
	}
	if n < minVal {
		t.Fatalf("%s=%d: must be >= %d", key, n, minVal)
	}
	return n
}

func pmSortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pmJITSetting returns the validated UZI_POOL_MEMORY_JIT value: "default" or "off".
func pmJITSetting(t *testing.T) string {
	t.Helper()
	switch v := strings.TrimSpace(os.Getenv("UZI_POOL_MEMORY_JIT")); v {
	case "", "default":
		return "default"
	case "off":
		return "off"
	default:
		t.Fatalf("UZI_POOL_MEMORY_JIT=%q: want default or off", v)
		return ""
	}
}

// ---------------------------------------------------------------------------------------
// JSON-lines output

type pmOut struct {
	mu sync.Mutex
	f  *os.File
}

func (o *pmOut) emit(kind string, v any) {
	b, err := json.Marshal(map[string]any{"kind": kind, "data": v})
	if err != nil {
		panic(fmt.Sprintf("marshal %s: %v", kind, err))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.f.Write(append(b, '\n')); err != nil {
		panic(fmt.Sprintf("write %s: %v", kind, err))
	}
}

// ---------------------------------------------------------------------------------------
// Fixture

type pmFixture struct {
	User, OtherUser, Conn, Repo, Worker, Run, QueuedRun uuid.UUID
	Email                                               string
	PageIDs                                             []uuid.UUID
	WantTotals                                          int
	BaseMessages                                        int64
	NextSeq                                             int32
}

const (
	pmWantPage     = 200 // ListRunsForUser page size (ADR-1620 shape)
	pmWantRunCount = 300 // the measured user's usage-bearing runs
	pmWantMessages = 2000
)

func pmExec(ctx context.Context, t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec %.80q: %v", sql, err)
	}
}

// pmSeed builds ADR-1620's shape: the measured user and a second user with 300 completed runs
// × 4 per_leg usage legs each, 200 noise users with one usage-less run, one online worker, a
// running run with 2000 messages held by that worker at claim generation 3, and one
// claimable queued run. Then ANALYZE.
func pmSeed(ctx context.Context, t *testing.T, dsn string) *pmFixture {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("seed connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	f := &pmFixture{
		User: uuid.New(), OtherUser: uuid.New(), Conn: uuid.New(), Repo: uuid.New(),
		Worker: uuid.New(), Run: uuid.New(), QueuedRun: uuid.New(), NextSeq: pmWantMessages + 1,
	}
	f.Email = fmt.Sprintf("pm-%s@e2e", f.User)
	pmExec(ctx, t, c, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.User, f.Email)
	pmExec(ctx, t, c, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.OtherUser, fmt.Sprintf("pm-other-%s@e2e", f.OtherUser))
	pmExec(ctx, t, c,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, f.Conn, f.User, []byte{0x1})
	pmExec(ctx, t, c,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/pool-memory', 'https://forge.e2e/g/pool-memory', 'main', true)`, f.Repo, f.Conn)
	for _, owner := range []uuid.UUID{f.User, f.OtherUser} {
		pmExec(ctx, t, c,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
			 SELECT gen_random_uuid(), $1, $2, g, 't', 'd', 'completed', 'issue', now() - make_interval(mins => g + 5)
			 FROM generate_series(1, 300) g`, owner, f.Repo)
		pmExec(ctx, t, c,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, cache_read_tokens,
			                        cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis)
			 SELECT r.id, 'sess-' || e, m, e, 100 + e, 10, 5, 50, 0.001, 'claude', 'metered', 'per_leg'
			 FROM runs r
			 CROSS JOIN (VALUES ('model-a'), ('model-b')) AS models(m)
			 CROSS JOIN generate_series(0, 1) AS e
			 WHERE r.user_id = $1`, owner)
	}
	pmExec(ctx, t, c,
		`WITH u AS (
		     INSERT INTO users (id, email, password_hash)
		     SELECT gen_random_uuid(), 'pm-noise-' || g || '-' || $1::text || '@e2e', 'x'
		     FROM generate_series(1, 200) g
		     RETURNING id
		 )
		 INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 SELECT gen_random_uuid(), u.id, $2, 1, 't', 'd', 'completed', 'issue' FROM u`, f.User.String(), f.Repo)
	pmExec(ctx, t, c,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, protocol_capabilities)
		 VALUES ($1, $2, 'pm-worker', $3, 'online', now(), 4, ARRAY['recovery_archive_v1', 'repo_agent_source_v1'])`,
		f.Worker, f.User, []byte(f.Worker.String()))
	pmExec(ctx, t, c,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, worker_id, claim_generation)
		 VALUES ($1, $2, $3, 1000, 'running', 'd', 'running', 'issue', $4, 3)`, f.Run, f.User, f.Repo, f.Worker)
	pmExec(ctx, t, c,
		`INSERT INTO run_messages (run_id, seq, kind, payload)
		 SELECT $1, g, 'text', '{"text":"x"}'::jsonb FROM generate_series(1, $2::int) g`, f.Run, pmWantMessages)
	pmExec(ctx, t, c,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 1001, 'queued', 'd', 'queued', 'issue')`, f.QueuedRun, f.User, f.Repo)
	for _, tbl := range []string{"users", "runs", "run_usage", "run_messages", "workers"} {
		pmExec(ctx, t, c, "ANALYZE "+tbl)
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM run_messages`).Scan(&f.BaseMessages); err != nil {
		t.Fatalf("seed: count messages: %v", err)
	}
	return f
}

func (f *pmFixture) claimParams() store.ClaimRunParams {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return store.ClaimRunParams{
		UserID: f.User, WorkerID: pgtype.UUID{Bytes: f.Worker, Valid: true},
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		AffinityCutoff:  now, SpreadCutoff: now, BackgroundGraceCutoff: now, CrossCheckEvaluatedAt: now,
		CapabilityAware: true, WorkerCaps: []string{}, WorkerProtocolCaps: []string{"recovery_archive_v1", "repo_agent_source_v1"},
		CustodyHoldLimit: 8, RecoveryCapable: true, WorkerIdentity: f.Worker.String(),
	}
}

func (f *pmFixture) heartbeatParams() store.HeartbeatWorkerParams {
	return store.HeartbeatWorkerParams{
		ID:                f.Worker,
		StatsCpuPct:       pgtype.Float4{Float32: 12.5, Valid: true},
		StatsMemBytes:     pgtype.Int8{Int64: 3 << 30, Valid: true},
		StatsSource:       pgtype.Text{String: "cgroup", Valid: true},
		DindRegisterNonce: "",
	}
}

func (f *pmFixture) messageParams() store.InsertRunMessageParams {
	return store.InsertRunMessageParams{
		RunID: f.Run, ClaimGeneration: pgtype.Int8{Int64: 3, Valid: true}, Seq: f.NextSeq,
		Kind: "text", Payload: []byte(`{"text":"x"}`),
	}
}

// ---------------------------------------------------------------------------------------
// Per-PID bookkeeping and the tracer

type pmCtxKey int

const (
	pmSkipKey pmCtxKey = iota + 1
	pmSQLKey
	pmViaQueryKey
	pmCaptureKey
)

// pmArgTracer records the arguments of the one call made with pmCaptureKey in its context,
// so the explain phase can replay ClaimRun with exactly the bindings the sqlc method sends.
type pmArgTracer struct{ got *[]any }

func (tr pmArgTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if ctx.Value(pmCaptureKey) != nil {
		*tr.got = append([]any(nil), d.Args...)
	}
	return ctx
}

func (pmArgTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// pmSkip marks a call as measurement plumbing: the tracer neither counts it as a served
// execution nor as a statement identity.
func pmSkip(ctx context.Context) context.Context { return context.WithValue(ctx, pmSkipKey, true) }

// pmPID's Peak* fields are interval-sampler readings only, from the sampler's start: they
// exclude the baseline sample, evictionDemo and the post-round samples. HWMKB (vmhwm) mixes
// sampler and held samples.
type pmPID struct {
	PID           uint32
	ConnectedAt   time.Time
	ClosedAt      time.Time
	Executions    int64
	Idents        map[string]struct{}
	StmtCapacity  int
	PlanCacheMode string
	PeakPrivateKB int64
	PeakSampleAge float64
	PeakAnonKB    int64
	PeakAnonAge   float64
	HWMKB         int64
	// JIT is SHOW jit on this backend, read once through the measured connection.
	JIT string
	// LLVM* record the first post-round sample whose backend had an llvm-named mapping.
	LLVMSeen  bool
	LLVMRound int
	LLVMAgeS  float64
	LLVMPaths map[string]struct{}
}

type pmTracer struct{ env *pmEnv }

func (tr pmTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if ctx.Value(pmSkipKey) != nil {
		return ctx
	}
	return context.WithValue(ctx, pmSQLKey, d.SQL)
}

func (tr pmTracer) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData) {
	if ctx.Value(pmSkipKey) != nil {
		return
	}
	sql, _ := ctx.Value(pmSQLKey).(string)
	if sql == "" {
		return
	}
	tr.env.recordExec(c.PgConn().PID(), sql, d.Err == nil, ctx.Value(pmViaQueryKey) != nil)
}

// ---------------------------------------------------------------------------------------
// Memory observer: reads /proc and cgroup files through a SEPARATE superuser connection.

type pmObserver struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

type pmMem struct {
	PrivateCleanKB int64 `json:"private_clean_kb"`
	PrivateDirtyKB int64 `json:"private_dirty_kb"`
	PrivateKB      int64 `json:"private_kb"` // Private_Clean + Private_Dirty
	AnonymousKB    int64 `json:"anonymous_kb"`
	RssKB          int64 `json:"rss_kb"`
	PssKB          int64 `json:"pss_kb"`
	// PssShmemKB is smaps_rollup's Pss_Shmem when the kernel reports it. It includes a
	// proportional share of shmem pages mapped by several processes (shared_buffers) as well
	// as pages only this backend has mapped, so it is not a heap metric.
	PssShmemKB     *int64 `json:"pss_shmem_kb_includes_proportional_share_of_shared_shmem,omitempty"`
	VmHWMKB        int64  `json:"vmhwm_kb_includes_shared"`
	VmRSSKB        int64  `json:"vmrss_kb"`
	CgroupCurrentB *int64 `json:"cgroup_memory_current_bytes"`
	CgroupAnonB    *int64 `json:"cgroup_anon_bytes"`
	CgroupFileB    *int64 `json:"cgroup_file_bytes"`
	CgroupNote     string `json:"cgroup_note,omitempty"`
}

// pmParseColon parses "Key:   123 kB" lines (smaps_rollup, status).
func pmParseColon(s string) (vals map[string]int64, units map[string]string) {
	vals, units = map[string]int64{}, map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		fs := strings.Fields(line[i+1:])
		if len(fs) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fs[0], 10, 64)
		if err != nil {
			continue
		}
		k := strings.TrimSpace(line[:i])
		vals[k] = n
		if len(fs) > 1 {
			units[k] = fs[1]
		}
	}
	return vals, units
}

// pmParseSpace parses memory.stat's "key 123" lines.
func pmParseSpace(s string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		fs := strings.Fields(line)
		if len(fs) != 2 {
			continue
		}
		if n, err := strconv.ParseInt(fs[1], 10, 64); err == nil {
			m[fs[0]] = n
		}
	}
	return m
}

func pmParseMem(smaps, status string) (pmMem, error) {
	var m pmMem
	if strings.TrimSpace(smaps) == "" {
		return m, errors.New("empty smaps_rollup read")
	}
	if strings.TrimSpace(status) == "" {
		return m, errors.New("empty /proc/<pid>/status read")
	}
	sm, units := pmParseColon(smaps)
	for _, k := range []string{"Rss", "Pss", "Private_Clean", "Private_Dirty", "Anonymous", "Shared_Clean", "Shared_Dirty"} {
		if _, ok := sm[k]; !ok {
			return m, fmt.Errorf("smaps_rollup lacks %s", k)
		}
		if units[k] != "kB" {
			return m, fmt.Errorf("smaps_rollup %s unit %q, want kB", k, units[k])
		}
	}
	// Unit/parse sanity: Rss is the sum of the four Private/Shared Clean/Dirty counters.
	if d := sm["Rss"] - (sm["Private_Clean"] + sm["Private_Dirty"] + sm["Shared_Clean"] + sm["Shared_Dirty"]); d < -64 || d > 64 {
		return m, fmt.Errorf("smaps_rollup Rss %d != private+shared clean/dirty %d", sm["Rss"],
			sm["Private_Clean"]+sm["Private_Dirty"]+sm["Shared_Clean"]+sm["Shared_Dirty"])
	}
	if sm["Pss"] > sm["Rss"]+64 {
		return m, fmt.Errorf("smaps_rollup Pss %d > Rss %d", sm["Pss"], sm["Rss"])
	}
	st, sunits := pmParseColon(status)
	for _, k := range []string{"VmHWM", "VmRSS"} {
		if _, ok := st[k]; !ok || sunits[k] != "kB" {
			return m, fmt.Errorf("status lacks %s in kB", k)
		}
	}
	m.PrivateCleanKB, m.PrivateDirtyKB = sm["Private_Clean"], sm["Private_Dirty"]
	m.PrivateKB = m.PrivateCleanKB + m.PrivateDirtyKB
	m.AnonymousKB, m.RssKB, m.PssKB = sm["Anonymous"], sm["Rss"], sm["Pss"]
	m.VmHWMKB, m.VmRSSKB = st["VmHWM"], st["VmRSS"]
	if v, ok := sm["Pss_Shmem"]; ok && units["Pss_Shmem"] == "kB" {
		m.PssShmemKB = &v
	}
	return m, nil
}

// pmObserverReadTimeout bounds one pmObserver.read once it holds the observer lock: the
// sampler's reads and the memory reads of sampleHeld and the explain phase. llvmMaps, queryRow
// and exec use the caller's context without this bound. Under pgx v5 a query
// cancelled mid-flight closes the observer connection, so later reads then fail loudly.
const pmObserverReadTimeout = 30 * time.Second

// read samples one backend. raw, when non-nil, receives the three raw file bodies.
func (o *pmObserver) read(ctx context.Context, pid uint32, raw *[3]string) (pmMem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// The bound starts after o.mu is taken, so time spent waiting for another reader never
	// eats into this read's own budget. It also lets a sampler join (pmSampler.Stop) finish.
	ctx, cancel := context.WithTimeout(ctx, pmObserverReadTimeout)
	defer cancel()
	var smaps, status string
	if err := o.conn.QueryRow(ctx, `SELECT pg_read_file($1::text), pg_read_file($2::text)`,
		fmt.Sprintf("/proc/%d/smaps_rollup", pid), fmt.Sprintf("/proc/%d/status", pid)).Scan(&smaps, &status); err != nil {
		return pmMem{}, fmt.Errorf("read /proc/%d: %w", pid, err)
	}
	m, err := pmParseMem(smaps, status)
	if err != nil {
		return m, err
	}
	var cur, stat string
	if err := o.conn.QueryRow(ctx, `SELECT pg_read_file('/sys/fs/cgroup/memory.current')`).Scan(&cur); err != nil {
		m.CgroupNote = "unavailable: " + err.Error()
	} else if err := o.conn.QueryRow(ctx, `SELECT pg_read_file('/sys/fs/cgroup/memory.stat')`).Scan(&stat); err != nil {
		m.CgroupNote = "unavailable: memory.stat: " + err.Error()
	} else {
		n, perr := strconv.ParseInt(strings.TrimSpace(cur), 10, 64)
		ms := pmParseSpace(stat)
		anon, aok := ms["anon"]
		file, fok := ms["file"]
		if perr != nil || !aok || !fok {
			m.CgroupNote = fmt.Sprintf("unavailable: unparseable cgroup files (memory.current %q, anon ok=%v, file ok=%v)", strings.TrimSpace(cur), aok, fok)
		} else {
			m.CgroupCurrentB, m.CgroupAnonB, m.CgroupFileB = &n, &anon, &file
		}
	}
	// A deadline or cancel during the cgroup queries would have closed the connection anyway;
	// report it instead of returning a sample that only carries a CgroupNote.
	if err := ctx.Err(); err != nil {
		return pmMem{}, fmt.Errorf("read cgroup for /proc/%d: %w", pid, err)
	}
	if raw != nil {
		raw[0], raw[1], raw[2] = smaps, status, cur+"\n---- memory.stat ----\n"+stat
	}
	return m, nil
}

// pmLLVMPaths returns the deduplicated, sorted mapped paths whose name contains "llvm"
// (case-insensitive) from the text of a backend's memory-map listing. A line is
// "addr perms offset dev inode path"; the path is everything after the fifth field.
func pmLLVMPaths(maps string) []string {
	seen := map[string]struct{}{}
	for _, line := range strings.Split(maps, "\n") {
		fs := strings.Fields(line)
		if len(fs) < 6 {
			continue
		}
		path := strings.Join(fs[5:], " ")
		if strings.Contains(strings.ToLower(path), "llvm") {
			seen[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// llvmMaps reads the backend's memory-map listing through the observer and returns the
// llvm-named mapped paths.
func (o *pmObserver) llvmMaps(ctx context.Context, pid uint32) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var maps string
	if err := o.conn.QueryRow(ctx, `SELECT pg_read_file($1::text)`, fmt.Sprintf("/proc/%d/maps", pid)).Scan(&maps); err != nil {
		return nil, fmt.Errorf("read maps of pid %d: %w", pid, err)
	}
	if strings.TrimSpace(maps) == "" {
		return nil, fmt.Errorf("empty maps read for pid %d", pid)
	}
	return pmLLVMPaths(maps), nil
}

func (o *pmObserver) queryRow(ctx context.Context, sql string, args []any, dest ...any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.conn.QueryRow(ctx, sql, args...).Scan(dest...)
}

func (o *pmObserver) exec(ctx context.Context, sql string, args ...any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.conn.Exec(ctx, sql, args...)
	return err
}

// ---------------------------------------------------------------------------------------
// Samples

type pmCtxTotals struct {
	TotalBytes        int64 `json:"total_bytes"`
	UsedBytes         int64 `json:"used_bytes"`
	CacheMemoryBytes  int64 `json:"cache_memory_context_total_bytes"`
	TopMemoryBytes    int64 `json:"top_memory_context_total_bytes"`
	CachedPlanBytes   int64 `json:"cached_plan_total_bytes"`
	CachedPlanSrcB    int64 `json:"cached_plan_source_total_bytes"`
	CachedPlanQueryB  int64 `json:"cached_plan_query_total_bytes"`
	CachedPlanSources int64 `json:"cached_plan_source_contexts"`
}

type pmSample struct {
	Cand       string       `json:"cand"`
	Rep        int          `json:"rep,omitempty"`
	Kind       string       `json:"sample_kind"`
	Round      int          `json:"round"`
	TMs        int64        `json:"t_ms"`
	PID        uint32       `json:"pid"`
	ConnAgeS   float64      `json:"conn_age_s"`
	Executions int64        `json:"executions_served"`
	Identities int          `json:"identities_cumulative"`
	Mem        pmMem        `json:"mem"`
	LLVMMapped bool         `json:"llvm_mapped"`
	LLVMPaths  []string     `json:"llvm_paths,omitempty"`
	Retained   *int         `json:"retained_statements,omitempty"`
	Generic    *int64       `json:"generic_plans,omitempty"`
	Custom     *int64       `json:"custom_plans,omitempty"`
	WithGen    *int         `json:"statements_with_generic_plan,omitempty"`
	Ctx        *pmCtxTotals `json:"contexts,omitempty"`
	CtxFirst   *pmCtxTotals `json:"contexts_first_snapshot,omitempty"`
}

// ---------------------------------------------------------------------------------------
// Per-candidate environment

type pmEnv struct {
	t    *testing.T
	out  *pmOut
	dsn  string
	cand pmCand
	rep  int // memory repetition (1-based); 0 outside the memory phase
	// jitSetting is UZI_POOL_MEMORY_JIT (default or off); off makes AfterConnect fail closed
	// unless the measured backend really reports jit = off.
	jitSetting string
	// dbName is the clone database this environment's pool and observer are connected to.
	dbName string
	fx     *pmFixture
	obs    *pmObserver
	start  time.Time

	pool *pgxpool.Pool
	q    *store.Queries

	curPID atomic.Uint32
	mu     sync.Mutex
	pids   map[uint32]*pmPID
	order  []uint32

	cursor  int // breadth cursor, continues across rounds
	rawDir  string
	rawOnce sync.Once

	// openTx are the transactions begin() handed out and rollback() has not yet ended;
	// closePool rolls them back so a failing test never blocks in pool.Close.
	txMu   sync.Mutex
	openTx []pgx.Tx
}

func newPMEnv(t *testing.T, out *pmOut, dsn string, cand pmCand, fx *pmFixture, obs *pmObserver, rawDir string) *pmEnv {
	return &pmEnv{t: t, out: out, dsn: dsn, cand: cand, fx: fx, obs: obs, start: time.Now(), rawDir: rawDir,
		pids: map[uint32]*pmPID{}, jitSetting: pmJITSetting(t)}
}

func (e *pmEnv) pidRec(pid uint32) *pmPID {
	p := e.pids[pid]
	if p == nil {
		p = &pmPID{PID: pid, ConnectedAt: time.Now(), Idents: map[string]struct{}{}}
		e.pids[pid] = p
		e.order = append(e.order, pid)
	}
	return p
}

// recordExec counts one served execution and, when it succeeded and pgx prepares it, the
// statement identity. viaQuery marks a call made with Conn.Query, which prepares even a
// zero-argument text (only a zero-argument Exec is sent on the simple protocol).
func (e *pmEnv) recordExec(pid uint32, sql string, ok, viaQuery bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pidRec(pid)
	p.Executions++
	if ok && (viaQuery || !pmNotPrepared(sql)) {
		p.Idents[sql] = struct{}{}
	}
}

func (e *pmEnv) pidStats(pid uint32) (age float64, execs int64, idents int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pidRec(pid)
	return time.Since(p.ConnectedAt).Seconds(), p.Executions, len(p.Idents)
}

func (e *pmEnv) pidGone(pid uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pids[pid]
	return p == nil || !p.ClosedAt.IsZero()
}

// openPool builds the candidate's api-style pool: pgxpool.ParseConfig, then the candidate's
// fields set directly, MaxConns 1, with the tracer and PID-recording hooks.
//
// The memory, latency/stress, cold and wide phases use openPool; the explain and discover
// phases build their own connections and are not covered by the guards in poolConfig. The wide phase's
// SET plan_cache_mode = force_generic_plan runs after acquire, so the AfterConnect mode check
// (which sees the connection's startup state) is unaffected by it. The guards are a harness
// guard only and do not restrict production OpenPool.
func (e *pmEnv) openPool(ctx context.Context) {
	cfg, err := e.poolConfig()
	if err != nil {
		e.t.Fatalf("%s: pool config: %v", e.cand.Name, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		e.t.Fatalf("%s: new pool: %v", e.cand.Name, err)
	}
	e.pool, e.q = pool, store.New(pool)
}

// pmCheckPoolConfig fails when the parsed DSN left the pool unable to measure the candidate:
// a non-default default_query_exec_mode (exec, simple_protocol, describe_exec, cache_describe) caches no
// prepared statement, and a statement_cache_capacity in the DSN that the candidate does not
// then override would make the capacity labels not measure the statement cache.
func pmCheckPoolConfig(cfg *pgxpool.Config, cand pmCand) error {
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		return fmt.Errorf("default_query_exec_mode is %v, want cache_statement: the capacity labels would not measure the statement cache",
			cfg.ConnConfig.DefaultQueryExecMode)
	}
	if got := cfg.ConnConfig.StatementCacheCapacity; got != cand.capacity() {
		return fmt.Errorf("statement cache capacity is %d, candidate %s wants %d: the capacity labels would not measure the statement cache",
			got, cand.Name, cand.capacity())
	}
	return nil
}

// pmCheckPlanCacheMode fails unless the backend's startup plan_cache_mode is auto, so a role-
// or database-level setting cannot silently change what the primary workload measures.
func pmCheckPlanCacheMode(mode string) error {
	if mode != "auto" {
		return fmt.Errorf("plan_cache_mode is %q on the measured connection, want auto (a role- or database-level setting?)", mode)
	}
	return nil
}

// poolConfig builds the candidate's pool configuration: ParseConfig, MaxConns 1, the
// candidate's fields, the tracer and the PID-recording hooks, then pmCheckPoolConfig.
func (e *pmEnv) poolConfig() (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.MaxConns = 1
	if e.cand.StmtCap > 0 {
		cfg.ConnConfig.StatementCacheCapacity = e.cand.StmtCap
	}
	if e.cand.MaxLife > 0 {
		cfg.MaxConnLifetime = e.cand.MaxLife
		cfg.MaxConnLifetimeJitter = e.cand.Jitter
	}
	if e.cand.Health > 0 {
		cfg.HealthCheckPeriod = e.cand.Health
	}
	if err := pmCheckPoolConfig(cfg, e.cand); err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = pmTracer{env: e}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		ctx = pmSkip(ctx)
		var mode string
		if err := c.QueryRow(ctx, `SHOW plan_cache_mode`, pgx.QueryExecModeSimpleProtocol).Scan(&mode); err != nil {
			return err
		}
		if err := pmCheckPlanCacheMode(mode); err != nil {
			return err
		}
		// Effective jit setting, read once per backend on the simple protocol (prepares
		// nothing). pg_jit_available() is deliberately NOT run here: it initialises the JIT
		// provider, which would map the LLVM libraries into the measured backend.
		var jit string
		if err := c.QueryRow(ctx, `SHOW jit`, pgx.QueryExecModeSimpleProtocol).Scan(&jit); err != nil {
			return err
		}
		if e.jitSetting == "off" && jit != "off" {
			return fmt.Errorf("UZI_POOL_MEMORY_JIT=off but SHOW jit = %q on the measured connection", jit)
		}
		pid := c.PgConn().PID()
		e.mu.Lock()
		p := e.pidRec(pid)
		p.ConnectedAt = time.Now()
		p.StmtCapacity = c.Config().StatementCacheCapacity
		p.PlanCacheMode = mode
		p.JIT = jit
		e.mu.Unlock()
		e.curPID.Store(pid)
		return nil
	}
	cfg.BeforeClose = func(c *pgx.Conn) {
		pid := c.PgConn().PID()
		e.mu.Lock()
		e.pidRec(pid).ClosedAt = time.Now()
		e.mu.Unlock()
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------------------
// Breadth table support

type pmExp int

const (
	pmUnknown pmExp = iota // not yet pinned: fatal in measurement mode
	pmSome                 // no error, at least one row (or a scalar/struct value)
	pmEmpty                // no error, zero rows
	pmNoRows               // pgx.ErrNoRows
)

func (x pmExp) String() string {
	return [...]string{"unknown", "some", "empty", "norows"}[x]
}

type pmEntry struct {
	Name string
	Exp  pmExp
	Fn   func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error)
}

func pmClassify(res any, err error) (pmExp, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pmNoRows, nil
		}
		return pmUnknown, err
	}
	v := reflect.ValueOf(res)
	if v.IsValid() && (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) && v.Len() == 0 {
		return pmEmpty, nil
	}
	return pmSome, nil
}

// pmRunEntry executes one breadth entry and checks it against its pinned expectation.
func pmRunEntry(ctx context.Context, q *store.Queries, f *pmFixture, en pmEntry) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := en.Fn(cctx, q, f)
	got, cerr := pmClassify(res, err)
	if cerr != nil {
		return fmt.Errorf("breadth %s: %w", en.Name, cerr)
	}
	if got != en.Exp {
		return fmt.Errorf("breadth %s: outcome %s, pinned %s", en.Name, got, en.Exp)
	}
	return nil
}

// pmSavepoint runs fn between SAVEPOINT and ROLLBACK TO SAVEPOINT + RELEASE on tx. The savepoint
// statements use the simple protocol so they are never cached as prepared statements.
func pmSavepoint(ctx context.Context, tx pgx.Tx, fn func() error) error {
	sctx := pmSkip(ctx)
	if _, err := tx.Exec(sctx, "SAVEPOINT pm", pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(); err != nil {
		return err
	}
	if _, err := tx.Exec(sctx, "ROLLBACK TO SAVEPOINT pm", pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("rollback to savepoint: %w", err)
	}
	// ROLLBACK TO keeps the savepoint alive; RELEASE drops it so nesting stays at one level.
	if _, err := tx.Exec(sctx, "RELEASE SAVEPOINT pm", pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// begin starts a transaction on the pool and registers it, so closePool can roll it back
// if the test fails (t.Fatalf) before rollback is reached.
func (e *pmEnv) begin(ctx context.Context) pgx.Tx {
	tx, err := e.pool.Begin(pmSkip(ctx))
	if err != nil {
		e.t.Fatalf("%s: begin: %v", e.cand.Name, err)
	}
	e.track(tx)
	return tx
}

func (e *pmEnv) track(tx pgx.Tx) {
	e.txMu.Lock()
	e.openTx = append(e.openTx, tx)
	e.txMu.Unlock()
}

func (e *pmEnv) untrack(tx pgx.Tx) {
	e.txMu.Lock()
	defer e.txMu.Unlock()
	for i, x := range e.openTx {
		if x == tx {
			e.openTx = append(e.openTx[:i], e.openTx[i+1:]...)
			return
		}
	}
}

func (e *pmEnv) rollback(ctx context.Context, tx pgx.Tx) {
	e.untrack(tx)
	if err := tx.Rollback(pmSkip(ctx)); err != nil {
		e.t.Fatalf("%s: rollback: %v", e.cand.Name, err)
	}
}

// closePool ends every transaction still open (bounded to 10 s in total, errors ignored) and
// then closes the pool. pgxpool.Close waits for every acquired connection to be released,
// so a transaction left open by a failing test would otherwise hang until the test timeout.
func (e *pmEnv) closePool() {
	e.txMu.Lock()
	open := e.openTx
	e.openTx = nil
	e.txMu.Unlock()
	if len(open) > 0 {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, tx := range open {
			_ = tx.Rollback(rctx)
		}
		cancel()
	}
	e.pool.Close()
}

// breadthPass runs every breadth entry once, in tx, each under its own savepoint.
func (e *pmEnv) breadthPass(ctx context.Context) {
	tx := e.begin(ctx)
	q := store.New(tx)
	for _, en := range pmBreadth {
		if err := pmSavepoint(ctx, tx, func() error { return pmRunEntry(ctx, q, e.fx, en) }); err != nil {
			e.t.Fatalf("%s: %v", e.cand.Name, err)
		}
	}
	e.rollback(ctx, tx)
}

// ---------------------------------------------------------------------------------------
// Hot mix and the latency loop

func (e *pmEnv) checkClaim(res store.Run, err error) error {
	if err != nil {
		return fmt.Errorf("ClaimRun: %w", err)
	}
	if res.ID != e.fx.QueuedRun || res.Status != "claimed" {
		return fmt.Errorf("ClaimRun returned run %s status %q, want %s claimed", res.ID, res.Status, e.fx.QueuedRun)
	}
	return nil
}

func (e *pmEnv) checkInsert(res store.InsertRunMessageRow, err error) error {
	if err != nil {
		return fmt.Errorf("InsertRunMessage: %w", err)
	}
	if !res.Inserted || !res.GenerationLive {
		return fmt.Errorf("InsertRunMessage inserted=%v generation_live=%v, want both true", res.Inserted, res.GenerationLive)
	}
	return nil
}

// hotMix runs the hot mix reps times: the reads and Ping/SchemaVersionStatus on the pool,
// the three writes in one transaction under per-execution savepoints.
func (e *pmEnv) hotMix(ctx context.Context, reps int) {
	t, f := e.t, e.fx
	for i := 0; i < reps; i++ {
		page, err := e.q.ListRunsForUser(ctx, store.ListRunsForUserParams{
			BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, UserID: f.User})
		if err != nil || len(page) != pmWantPage {
			t.Fatalf("%s: ListRunsForUser: %d rows (want %d), err %v", e.cand.Name, len(page), pmWantPage, err)
		}
		totals, err := e.q.ListRunUsageTotalsForRuns(ctx, f.PageIDs)
		if err != nil || len(totals) != f.WantTotals {
			t.Fatalf("%s: ListRunUsageTotalsForRuns: %d rows (want %d), err %v", e.cand.Name, len(totals), f.WantTotals, err)
		}
		su, err := e.q.SelfUsage(ctx, f.User)
		if err != nil || su.RunCount != pmWantRunCount {
			t.Fatalf("%s: SelfUsage run_count %d (want %d), err %v", e.cand.Name, su.RunCount, pmWantRunCount, err)
		}
		if err := e.pool.Ping(pmSkip(ctx)); err != nil {
			t.Fatalf("%s: Ping: %v", e.cand.Name, err)
		}
		if _, head, atHead, err := store.SchemaVersionStatus(ctx, e.pool); err != nil || !atHead {
			t.Fatalf("%s: SchemaVersionStatus head %d atHead %v err %v", e.cand.Name, head, atHead, err)
		}
		tx := e.begin(ctx)
		qt := store.New(tx)
		err = pmSavepoint(ctx, tx, func() error { r, err := qt.ClaimRun(ctx, f.claimParams()); return e.checkClaim(r, err) })
		if err == nil {
			err = pmSavepoint(ctx, tx, func() error {
				w, err := qt.HeartbeatWorker(ctx, f.heartbeatParams())
				if err != nil || w.ID != f.Worker {
					return fmt.Errorf("HeartbeatWorker: id %s err %v", w.ID, err)
				}
				return nil
			})
		}
		if err == nil {
			err = pmSavepoint(ctx, tx, func() error {
				r, err := qt.InsertRunMessage(ctx, f.messageParams())
				return e.checkInsert(r, err)
			})
		}
		if err != nil {
			t.Fatalf("%s: hot mix: %v", e.cand.Name, err)
		}
		e.rollback(ctx, tx)
	}
}

type pmLat struct {
	ClaimUS  []float64 `json:"-"`
	InsertUS []float64 `json:"-"`
}

// latencyLoop is ADR-1620's timed loop: pmWarmups untimed warm-ups of each hot write, then n
// timed executions of ClaimRun and InsertRunMessage (only the method call timed), each
// under SAVEPOINT / ROLLBACK TO SAVEPOINT, with k breadth calls interleaved per iteration
// (untimed) so a capped statement cache evicts.
func (e *pmEnv) latencyLoop(ctx context.Context, n, k int, warmBreadth bool) pmLat {
	f := e.fx
	if warmBreadth {
		e.breadthPass(ctx)
	}
	tx := e.begin(ctx)
	qt := store.New(tx)
	for i := 0; i < pmWarmups; i++ {
		err := pmSavepoint(ctx, tx, func() error { r, err := qt.ClaimRun(ctx, f.claimParams()); return e.checkClaim(r, err) })
		if err == nil {
			err = pmSavepoint(ctx, tx, func() error { r, err := qt.InsertRunMessage(ctx, f.messageParams()); return e.checkInsert(r, err) })
		}
		if err != nil {
			e.t.Fatalf("%s: warm-up: %v", e.cand.Name, err)
		}
	}
	var lat pmLat
	for i := 0; i < n; i++ {
		var d1, d2 time.Duration
		err := pmSavepoint(ctx, tx, func() error {
			p := f.claimParams()
			t0 := time.Now()
			r, err := qt.ClaimRun(ctx, p)
			d1 = time.Since(t0)
			return e.checkClaim(r, err)
		})
		if err == nil {
			err = pmSavepoint(ctx, tx, func() error {
				p := f.messageParams()
				t0 := time.Now()
				r, err := qt.InsertRunMessage(ctx, p)
				d2 = time.Since(t0)
				return e.checkInsert(r, err)
			})
		}
		for j := 0; err == nil && j < k; j++ {
			en := pmBreadth[e.cursor%len(pmBreadth)]
			e.cursor++
			err = pmSavepoint(ctx, tx, func() error { return pmRunEntry(ctx, qt, f, en) })
		}
		if err != nil {
			e.t.Fatalf("%s: latency loop iteration %d: %v", e.cand.Name, i, err)
		}
		lat.ClaimUS = append(lat.ClaimUS, float64(d1.Nanoseconds())/1e3)
		lat.InsertUS = append(lat.InsertUS, float64(d2.Nanoseconds())/1e3)
	}
	e.rollback(ctx, tx)
	return lat
}

// verifyFixture re-checks, through the observer connection (not the measured pool), that
// nothing the savepoints rolled back leaked: the queued run is still queued, the message
// count and claim generation are unchanged, no custody hold exists. It also refreshes the
// worker heartbeat so time-windowed breadth queries keep their pinned outcome.
func (e *pmEnv) verifyFixture(ctx context.Context) {
	var status string
	var msgs, runMsgs, holds, gen int64
	err := e.obs.queryRow(ctx, `SELECT (SELECT status FROM runs WHERE id = $1),
	        (SELECT count(*) FROM run_messages), (SELECT count(*) FROM run_messages WHERE run_id = $2),
	        (SELECT count(*) FROM recovery_custody_holds WHERE user_id = $3),
	        (SELECT claim_generation FROM runs WHERE id = $2)`,
		[]any{e.fx.QueuedRun, e.fx.Run, e.fx.User}, &status, &msgs, &runMsgs, &holds, &gen)
	if err != nil {
		e.t.Fatalf("%s: verify fixture: %v", e.cand.Name, err)
	}
	if status != "queued" || msgs != e.fx.BaseMessages || runMsgs != pmWantMessages || holds != 0 || gen != 3 {
		e.t.Fatalf("%s: fixture moved: queued run status %q, run_messages %d (base %d), running run messages %d, custody holds %d, claim_generation %d",
			e.cand.Name, status, msgs, e.fx.BaseMessages, runMsgs, holds, gen)
	}
	if err := e.obs.exec(ctx, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, e.fx.Worker); err != nil {
		e.t.Fatalf("%s: refresh heartbeat: %v", e.cand.Name, err)
	}
}

// ---------------------------------------------------------------------------------------
// Sampling

const pmContextSQL = `SELECT coalesce(sum(total_bytes), 0)::bigint,
	coalesce(sum(used_bytes), 0)::bigint,
	coalesce(sum(total_bytes) FILTER (WHERE name = 'CacheMemoryContext'), 0)::bigint,
	coalesce(sum(total_bytes) FILTER (WHERE name = 'TopMemoryContext'), 0)::bigint,
	coalesce(sum(total_bytes) FILTER (WHERE name = 'CachedPlan'), 0)::bigint,
	coalesce(sum(total_bytes) FILTER (WHERE name = 'CachedPlanSource'), 0)::bigint,
	coalesce(sum(total_bytes) FILTER (WHERE name = 'CachedPlanQuery'), 0)::bigint,
	count(*) FILTER (WHERE name = 'CachedPlanSource')::bigint
FROM pg_backend_memory_contexts`

func (e *pmEnv) ctxTotals(ctx context.Context, c *pgx.Conn) (pmCtxTotals, error) {
	var x pmCtxTotals
	err := c.QueryRow(pmSkip(ctx), pmContextSQL, pgx.QueryExecModeSimpleProtocol).Scan(&x.TotalBytes, &x.UsedBytes,
		&x.CacheMemoryBytes, &x.TopMemoryBytes, &x.CachedPlanBytes, &x.CachedPlanSrcB, &x.CachedPlanQueryB, &x.CachedPlanSources)
	return x, err
}

// sampleHeld takes a full sample on the measured connection c, which the caller holds idle:
// two consecutive context snapshots (the second is used), the prepared-statement census,
// then the kernel memory read on the observer connection while the connection is still held.
func (e *pmEnv) sampleHeld(ctx context.Context, c *pgxpool.Conn, kind string, round int) pmSample {
	t := e.t
	pid := c.Conn().PgConn().PID()
	s := pmSample{Cand: e.cand.Name, Rep: e.rep, Kind: kind, Round: round, PID: pid}
	first, err := e.ctxTotals(ctx, c.Conn())
	if err != nil {
		t.Fatalf("%s: context totals: %v", e.cand.Name, err)
	}
	second, err := e.ctxTotals(ctx, c.Conn())
	if err != nil {
		t.Fatalf("%s: context totals: %v", e.cand.Name, err)
	}
	s.CtxFirst, s.Ctx = &first, &second
	var retained, withGen int
	var generic, custom int64
	if err := c.Conn().QueryRow(pmSkip(ctx),
		`SELECT count(*)::int, coalesce(sum(generic_plans), 0)::bigint, coalesce(sum(custom_plans), 0)::bigint,
		        (count(*) FILTER (WHERE generic_plans > 0))::int FROM pg_prepared_statements`,
		pgx.QueryExecModeSimpleProtocol).Scan(&retained, &generic, &custom, &withGen); err != nil {
		t.Fatalf("%s: pg_prepared_statements: %v", e.cand.Name, err)
	}
	s.Retained, s.Generic, s.Custom, s.WithGen = &retained, &generic, &custom, &withGen
	var raw [3]string
	mem, err := e.obs.read(ctx, pid, &raw)
	if err != nil {
		t.Fatalf("%s: memory read for pid %d: %v", e.cand.Name, pid, err)
	}
	e.saveRaw(raw)
	s.Mem = mem
	llvm, err := e.obs.llvmMaps(ctx, pid)
	if err != nil {
		t.Fatalf("%s: llvm detection: %v", e.cand.Name, err)
	}
	s.LLVMMapped, s.LLVMPaths = len(llvm) > 0, llvm
	s.ConnAgeS, s.Executions, s.Identities = e.pidStats(pid)
	s.TMs = time.Since(e.start).Milliseconds()
	// Working-set evidence, fail closed.
	capacity := e.cand.capacity()
	if retained > capacity {
		t.Fatalf("%s: pg_prepared_statements holds %d > statement cache capacity %d", e.cand.Name, retained, capacity)
	}
	if s.Identities > capacity && retained != capacity {
		t.Fatalf("%s: %d cumulative identities exceed capacity %d but pg_prepared_statements holds %d (want exactly the capacity)",
			e.cand.Name, s.Identities, capacity, retained)
	}
	e.mu.Lock()
	p := e.pidRec(pid)
	if mem.VmHWMKB > p.HWMKB {
		p.HWMKB = mem.VmHWMKB
	}
	if s.LLVMMapped {
		if !p.LLVMSeen {
			p.LLVMSeen, p.LLVMRound, p.LLVMAgeS = true, round, s.ConnAgeS
		}
		if p.LLVMPaths == nil {
			p.LLVMPaths = map[string]struct{}{}
		}
		for _, lp := range llvm {
			p.LLVMPaths[lp] = struct{}{}
		}
	}
	e.mu.Unlock()
	e.out.emit("sample", s)
	return s
}

// saveRaw keeps the first raw capture per candidate, so the parser and the kB units can be
// checked by eye against the real file bodies.
func (e *pmEnv) saveRaw(raw [3]string) {
	e.rawOnce.Do(func() {
		for i, name := range []string{"smaps_rollup", "status", "cgroup"} {
			tag := e.cand.Name
			if e.rep > 0 {
				tag = fmt.Sprintf("%s-r%d", tag, e.rep)
			}
			p := filepath.Join(e.rawDir, fmt.Sprintf("raw-%s-%s.txt", tag, name))
			if err := os.WriteFile(filepath.Clean(p), []byte(raw[i]), 0o600); err != nil { //nolint:gosec // G703: operator-supplied measurement output dir under opt-in env, fixed file names
				e.t.Fatalf("save raw capture: %v", err)
			}
		}
	})
}

func (e *pmEnv) postRoundSample(ctx context.Context, kind string, round int) pmSample {
	c, err := e.pool.Acquire(pmSkip(ctx))
	if err != nil {
		e.t.Fatalf("%s: acquire for sample: %v", e.cand.Name, err)
	}
	defer c.Release()
	return e.sampleHeld(ctx, c, kind, round)
}

// pmSamplerResult summarizes the interval sampler's own readings. The Peak* fields are
// interval-sampler readings only, taken from the sampler's start: they exclude the baseline
// sample, evictionDemo and the post-round samples (sampleHeld), so they are not a process
// high-water mark. VmHWM (HWMKB elsewhere) mixes sampler and held samples.
type pmSamplerResult struct {
	N            int     `json:"samples"`
	Errors       int     `json:"errors"`
	Dropped      int     `json:"dropped_for_recycled_pid"`
	FirstError   string  `json:"first_error,omitempty"`
	MeanMs       float64 `json:"achieved_interval_mean_ms"`
	P95Ms        float64 `json:"achieved_interval_p95_ms"`
	MaxMs        float64 `json:"achieved_interval_max_ms"`
	PeakAnonKB   int64   `json:"peak_sampled_anonymous_kb"`
	PeakAnonPID  uint32  `json:"peak_anonymous_pid"`
	PeakAnonAgeS float64 `json:"peak_anonymous_conn_age_s"`
	PeakKB       int64   `json:"peak_sampled_private_kb"`
	PeakPID      uint32  `json:"peak_private_pid"`
	PeakAgeS     float64 `json:"peak_private_conn_age_s"`
}

// pmSamplerJoinWait is how long Stop waits for the sampler goroutine to finish on its own
// before cancelling its context. It exceeds the per-read timeout in pmObserver.read, so a
// graceful Stop never cancels a healthy in-flight read when no other reader is queued on the
// observer mutex (a queued reader's wait is not bounded by that timeout).
const pmSamplerJoinWait = 35 * time.Second

type pmSampler struct {
	e      *pmEnv
	read   func(ctx context.Context, pid uint32) (pmMem, error)
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	// joinWait bounds Stop's wait for done before it cancels the goroutine's context.
	joinWait time.Duration
	// res and times are written only by the goroutine and read only after done is closed.
	res   pmSamplerResult
	times []time.Time
	// final is the cached result, set once inside once.Do.
	final pmSamplerResult
}

func (e *pmEnv) startSampler(ctx context.Context) *pmSampler {
	return newPMSampler(ctx, e, func(ctx context.Context, pid uint32) (pmMem, error) {
		return e.obs.read(ctx, pid, nil)
	}, pmSampleInterval)
}

// newPMSampler starts the sampler goroutine. read is the memory read (the observer in
// production, a fake in tests) and interval the tick period.
func newPMSampler(ctx context.Context, e *pmEnv, read func(ctx context.Context, pid uint32) (pmMem, error), interval time.Duration) *pmSampler {
	sctx, cancel := context.WithCancel(ctx)
	s := &pmSampler{e: e, read: read, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}), joinWait: pmSamplerJoinWait}
	go s.loop(sctx, interval)
	return s
}

func (s *pmSampler) stopRequested() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *pmSampler) loop(sctx context.Context, interval time.Duration) {
	e := s.e
	defer close(s.done)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		// A tick and a stop can be ready together; do not start a read after Stop.
		if s.stopRequested() {
			return
		}
		pid := e.curPID.Load()
		if pid == 0 {
			continue
		}
		now := time.Now()
		mem, err := s.read(sctx, pid)
		if err != nil {
			// A read that failed because the sampler's own context was cancelled, or whose backend
			// is gone or replaced, is not a sampling error. A read that hit its own timeout after
			// a stop request still counts as an error.
			if sctx.Err() != nil || e.pidGone(pid) || e.curPID.Load() != pid {
				s.res.Dropped++
			} else {
				s.res.Errors++
				if s.res.FirstError == "" {
					s.res.FirstError = err.Error()
				}
			}
			continue
		}
		age, execs, idents := e.pidStats(pid)
		smp := pmSample{Cand: e.cand.Name, Kind: "sampler", PID: pid, ConnAgeS: age, Executions: execs,
			Identities: idents, Mem: mem, TMs: now.Sub(e.start).Milliseconds()}
		s.times = append(s.times, now)
		s.res.N++
		e.mu.Lock()
		p := e.pidRec(pid)
		if mem.PrivateKB > p.PeakPrivateKB {
			p.PeakPrivateKB, p.PeakSampleAge = mem.PrivateKB, age
		}
		if mem.AnonymousKB > p.PeakAnonKB {
			p.PeakAnonKB, p.PeakAnonAge = mem.AnonymousKB, age
		}
		if mem.VmHWMKB > p.HWMKB {
			p.HWMKB = mem.VmHWMKB
		}
		e.mu.Unlock()
		if mem.PrivateKB > s.res.PeakKB {
			s.res.PeakKB, s.res.PeakPID, s.res.PeakAgeS = mem.PrivateKB, pid, age
		}
		if mem.AnonymousKB > s.res.PeakAnonKB {
			s.res.PeakAnonKB, s.res.PeakAnonPID, s.res.PeakAnonAgeS = mem.AnonymousKB, pid, age
		}
		e.out.emit("sampler", map[string]any{"cand": e.cand.Name, "rep": e.rep, "t_ms": smp.TMs, "pid": pid, "conn_age_s": age,
			"executions_served": execs, "identities_cumulative": idents, "private_kb": mem.PrivateKB,
			"private_clean_kb": mem.PrivateCleanKB, "private_dirty_kb": mem.PrivateDirtyKB, "anonymous_kb": mem.AnonymousKB, "rss_kb": mem.RssKB, "pss_kb": mem.PssKB,
			"vmhwm_kb_includes_shared": mem.VmHWMKB, "cgroup_memory_current_bytes": mem.CgroupCurrentB})
	}
}

// Stop is the graceful path: request a stop, wait up to joinWait for the goroutine (so a
// healthy in-flight read completes), cancel and join if it overstays, then compute and cache
// the result. Whichever of Stop and Close runs first wins; later calls return the cache.
func (s *pmSampler) Stop() pmSamplerResult {
	s.once.Do(func() { s.finish(false) })
	return s.final
}

// Close is the failure teardown: it cancels the read context immediately, joins without a
// bound, and caches the result, unless Stop already did.
func (s *pmSampler) Close() pmSamplerResult {
	s.once.Do(func() { s.finish(true) })
	return s.final
}

func (s *pmSampler) finish(cancelFirst bool) {
	if cancelFirst {
		s.cancel()
	}
	close(s.stop)
	if cancelFirst {
		<-s.done
	} else {
		select {
		case <-s.done:
		case <-time.After(s.joinWait):
			s.cancel()
			<-s.done
		}
	}
	s.cancel()
	// done is closed, so res and times are no longer written.
	var gaps []float64
	for i := 1; i < len(s.times); i++ {
		gaps = append(gaps, float64(s.times[i].Sub(s.times[i-1]).Microseconds())/1e3)
	}
	if len(gaps) > 0 {
		sum := 0.0
		for _, g := range gaps {
			sum += g
		}
		s.res.MeanMs = sum / float64(len(gaps))
		s.res.P95Ms = pmQuantile(gaps, 0.95)
		s.res.MaxMs = pmQuantile(gaps, 1)
	}
	s.final = s.res
}

// pmWithSampler runs body with a sampler from start and always closes it afterwards, so a
// t.Fatalf (runtime.Goexit) or panic in body still cancels and joins the goroutine before the
// caller's earlier defers run. body should call Stop itself on the success path.
func pmWithSampler(start func() *pmSampler, body func(s *pmSampler)) {
	s := start()
	defer s.Close()
	body(s)
}

// pmRoundLoop runs rounds until the stop rule holds. round(r) runs round r and returns the
// number of backend pids seen so far. Without recycle it stops once r >= minRounds. With
// recycle it stops once r >= minRounds and at least four recycles happened (pids-1 >= 4);
// otherwise, past maxWait it stops cleanly only when minRounds rounds are done, and returns
// an error if the deadline cut the run short of minRounds. It returns the rounds completed.
func pmRoundLoop(minRounds int, recycle bool, maxWait time.Duration, elapsed func() time.Duration, round func(r int) int) (int, error) {
	for r := 1; ; r++ {
		pids := round(r)
		if !recycle {
			if r >= minRounds {
				return r, nil
			}
			continue
		}
		if r >= minRounds && pids-1 >= 4 {
			return r, nil
		}
		if elapsed() > maxWait {
			if r < minRounds {
				return r, fmt.Errorf("recycling deadline %s reached after %d of %d requested rounds", maxWait, r, minRounds)
			}
			return r, nil
		}
	}
}

// ---------------------------------------------------------------------------------------
// Statistics helpers

func pmQuantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	idx := int(math.Ceil(q*float64(len(c)))) - 1
	if idx < 0 {
		idx = 0
	}
	return c[idx]
}

func pmMean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func pmMedianInt(xs []int64) int64 {
	c := append([]int64(nil), xs...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// ---------------------------------------------------------------------------------------
// sqlc query texts

var pmNameRE = regexp.MustCompile(`^-- name: (\w+) `)

// pmSQLConstants parses the generated *.sql.go files in the package directory with go/parser
// and returns every sqlc query-text constant (a string constant beginning "-- name:"),
// keyed by method name, plus the names in file/declaration order.
func pmSQLConstants() (map[string]string, []string, error) {
	files, err := filepath.Glob("*.sql.go")
	if err != nil || len(files) == 0 {
		return nil, nil, fmt.Errorf("no *.sql.go files in %s (err %v)", mustGetwd(), err)
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	byName := map[string]string{}
	var order []string
	for _, fn := range files {
		af, err := parser.ParseFile(fset, fn, nil, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range af.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for _, v := range vs.Values {
					s, ok, err := pmConstString(v)
					if err != nil {
						return nil, nil, err
					}
					if !ok {
						continue
					}
					m := pmNameRE.FindStringSubmatch(s)
					if m == nil {
						continue
					}
					if _, dup := byName[m[1]]; dup {
						return nil, nil, fmt.Errorf("duplicate sqlc query name %s", m[1])
					}
					byName[m[1]] = s
					order = append(order, m[1])
				}
			}
		}
	}
	return byName, order, nil
}

// pmConstString evaluates a constant string expression made of string literals joined by +
// (sqlc splices a backtick into a raw-string query as "`" + ...). ok is false for any other
// expression.
func pmConstString(x ast.Expr) (s string, ok bool, err error) {
	switch v := x.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false, nil
		}
		u, err := strconv.Unquote(v.Value)
		return u, err == nil, err
	case *ast.ParenExpr:
		return pmConstString(v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false, nil
		}
		l, lok, err := pmConstString(v.X)
		if err != nil || !lok {
			return "", false, err
		}
		r, rok, err := pmConstString(v.Y)
		if err != nil || !rok {
			return "", false, err
		}
		return l + r, true, nil
	}
	return "", false, nil
}

func mustGetwd() string { d, _ := os.Getwd(); return d }

// pmBreadth is the curated breadth table: distinct sqlc Queries methods, each with a
// production-shaped binding against the seeded fixture and a pinned expected outcome
// (some = rows or a value, empty = no rows, norows = pgx.ErrNoRows). Generated once from the
// signatures in the *.sql.go files, filtered to methods whose arguments are a fixture id
// (reads, getters, locks and deletes, some of them writes) plus zero-argument calls, then pinned from a discovery run (UZI_POOL_MEMORY_DISCOVER=1).
var pmBreadth = []pmEntry{
	{"DeleteAllCodexProviderAccountsForOwner", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteAllCodexProviderAccountsForOwner(ctx, f.User)
	}},
	{"DeleteAllRunEnvVarsForOwner", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteAllRunEnvVarsForOwner(ctx, f.User)
	}},
	{"DeleteAllUserSecretsForOwner", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteAllUserSecretsForOwner(ctx, f.User)
	}},
	{"GetUserCredentialsResetAt", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserCredentialsResetAt(ctx, f.User)
	}},
	{"LockOwnerRunsForReset", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockOwnerRunsForReset(ctx, f.User)
	}},
	{"LockOwnerSecretsForReset", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockOwnerSecretsForReset(ctx, f.User)
	}},
	{"LockOwnerWorkersForReset", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockOwnerWorkersForReset(ctx, f.User)
	}},
	{"SupersedeLiveResetLinksForTarget", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.SupersedeLiveResetLinksForTarget(ctx, f.User)
	}},
	{"ListAgentMemoryForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAgentMemoryForUser(ctx, f.User)
	}},
	{"GetAgentSourceStaged", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetAgentSourceStaged(ctx)
	}},
	{"ListAgentTemplates", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAgentTemplates(ctx)
	}},
	{"DeleteRateLimits", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteRateLimits(ctx, f.User)
	}},
	{"ListAnthropicTokensToPoll", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAnthropicTokensToPoll(ctx)
	}},
	{"ListAnthropicTokensToPollForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAnthropicTokensToPollForUser(ctx, f.User)
	}},
	{"ListAutoSelectCandidates", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAutoSelectCandidates(ctx, f.User)
	}},
	{"ListRateLimits", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListRateLimits(ctx) }},
	{"ListRateLimitsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRateLimitsForUser(ctx, f.User)
	}},
	{"ListReserveClaudeTokens", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReserveClaudeTokens(ctx, f.User)
	}},
	{"UserHasAnthropicToken", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.UserHasAnthropicToken(ctx, f.User)
	}},
	{"GetAutopilotConnectionContext", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetAutopilotConnectionContext(ctx, f.Conn)
	}},
	{"ListBrandingSlots", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListBrandingSlots(ctx)
	}},
	{"CountChatFollowUps", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountChatFollowUps(ctx, f.Run)
	}},
	{"CountPendingProposalsForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountPendingProposalsForRun(ctx, f.Run)
	}},
	{"GetChatRunClaimContext", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetChatRunClaimContext(ctx, f.Run)
	}},
	{"GetLiveChatForUser", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetLiveChatForUser(ctx, f.User)
	}},
	{"ListChatRunsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListChatRunsForUser(ctx, f.User)
	}},
	{"GetClaimCheckpointRecovery", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetClaimCheckpointRecovery(ctx, f.Run)
	}},
	{"ResolveCheckpointCoordinates", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ResolveCheckpointCoordinates(ctx, f.Run)
	}},
	{"GetCheckpointRetention", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetCheckpointRetention(ctx, f.Run)
	}},
	{"GetCheckpointRetentionBackfillNow", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetCheckpointRetentionBackfillNow(ctx)
	}},
	{"GetRunCheckpointTipForRetention", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunCheckpointTipForRetention(ctx, f.Run)
	}},
	{"LockCheckpointRetentionForConfirmation", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockCheckpointRetentionForConfirmation(ctx, f.Run)
	}},
	{"MarkCheckpointSuperseded", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.MarkCheckpointSuperseded(ctx, f.Run)
	}},
	{"RunHasOpenCustodyHold", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.RunHasOpenCustodyHold(ctx, f.Run)
	}},
	{"SetCheckpointRetentionSettlingIfUnheld", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.SetCheckpointRetentionSettlingIfUnheld(ctx, f.Run)
	}},
	{"ListCIAutofixCandidateRefs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCIAutofixCandidateRefs(ctx, f.Repo)
	}},
	{"ListCIAutofixHaltsForRepo", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCIAutofixHaltsForRepo(ctx, f.Repo)
	}},
	{"DeleteExpiredCLIAuthRequests", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteExpiredCLIAuthRequests(ctx)
	}},
	{"ListAllCLITokensForAdmin", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAllCLITokensForAdmin(ctx)
	}},
	{"ListCLITokens", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCLITokens(ctx, f.User)
	}},
	{"GetCodeCrossCheck", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetCodeCrossCheck(ctx, f.Run)
	}},
	{"ListCodeCrossChecksToSettle", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCodeCrossChecksToSettle(ctx)
	}},
	{"GetCodexAdmissionRunEligibility", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetCodexAdmissionRunEligibility(ctx, f.Run)
	}},
	{"GetRunCodexAuthContext", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunCodexAuthContext(ctx, f.Run)
	}},
	{"ClearCodexDefaults", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ClearCodexDefaults(ctx, f.User)
	}},
	{"CountCodexSecrets", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountCodexSecrets(ctx, f.User)
	}},
	{"ListCodexCredentialStatesForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCodexCredentialStatesForUser(ctx, f.User)
	}},
	{"ListStagedCodexAliases", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListStagedCodexAliases(ctx)
	}},
	{"ListStagedCodexAliasesForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListStagedCodexAliasesForUser(ctx, f.User)
	}},
	{"ListUserSecretsAll", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListUserSecretsAll(ctx, f.User)
	}},
	{"CountCodexInFlightByAccount", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountCodexInFlightByAccount(ctx, f.User)
	}},
	{"GetUserCodexAccountMode", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserCodexAccountMode(ctx, f.User)
	}},
	{"ListCodexPoolAliases", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCodexPoolAliases(ctx, f.User)
	}},
	{"GetCodexAccountRateLimitsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetCodexAccountRateLimitsForUser(ctx, f.User)
	}},
	{"ListCodexAccountRateLimits", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCodexAccountRateLimits(ctx)
	}},
	{"ListLinkedCodexAccountsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListLinkedCodexAccountsForUser(ctx, f.User)
	}},
	{"ListLinkedCodexAccountsToPoll", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListLinkedCodexAccountsToPoll(ctx)
	}},
	{"ListReserveCodexAliases", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReserveCodexAliases(ctx, f.User)
	}},
	{"HasEnabledAnthropicForJudgeAuto", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasEnabledAnthropicForJudgeAuto(ctx, f.User)
	}},
	{"LockDefaultAnthropicSecretForShare", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockDefaultAnthropicSecretForShare(ctx, f.User)
	}},
	{"CrossCheckHistoryAvailability", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CrossCheckHistoryAvailability(ctx, f.User)
	}},
	{"ListUserCrossCheckPins", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListUserCrossCheckPins(ctx, f.User)
	}},
	{"ListJudgeTriageRowsAll", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListJudgeTriageRowsAll(ctx)
	}},
	{"ListJudgeTriageRowsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListJudgeTriageRowsForUser(ctx, f.User)
	}},
	{"ListEgressProfiles", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEgressProfiles(ctx)
	}},
	{"ListFindingIssueCloseEdges", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListFindingIssueCloseEdges(ctx, f.Repo)
	}},
	{"CountFindingsForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountFindingsForRun(ctx, f.Run)
	}},
	{"AdminListEnabledRepoActivity", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.AdminListEnabledRepoActivity(ctx)
	}},
	{"AdminListReposWithPrivilege", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.AdminListReposWithPrivilege(ctx)
	}},
	{"CountActiveRunsForRepo", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountActiveRunsForRepo(ctx, f.Repo)
	}},
	{"CountBoardColumns", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountBoardColumns(ctx, f.Repo)
	}},
	{"CountOpenCustodyHoldsForRepo", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOpenCustodyHoldsForRepo(ctx, f.Repo)
	}},
	{"GetRepoByID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRepoByID(ctx, f.Repo)
	}},
	{"ListAllForgeConnections", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAllForgeConnections(ctx)
	}},
	{"ListBoardColumns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListBoardColumns(ctx, f.Repo)
	}},
	{"ListBoardFreeMRStateWatchCandidates", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListBoardFreeMRStateWatchCandidates(ctx, f.Repo)
	}},
	{"ListEnabledLinkedRepoIDs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledLinkedRepoIDs(ctx)
	}},
	{"ListEnabledRepoIDs", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledRepoIDs(ctx)
	}},
	{"ListEnabledReposByConnection", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledReposByConnection(ctx, f.Conn)
	}},
	{"ListEnabledReposForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledReposForUser(ctx, f.User)
	}},
	{"ListEnabledReposWithConnections", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledReposWithConnections(ctx)
	}},
	{"ListForgeConnectionsByUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListForgeConnectionsByUser(ctx, f.User)
	}},
	{"ListIssuesByRepo", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListIssuesByRepo(ctx, f.Repo)
	}},
	{"ListLatestRunsForRepo", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListLatestRunsForRepo(ctx, f.Repo)
	}},
	{"ListMRWatchCandidates", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMRWatchCandidates(ctx, f.Repo)
	}},
	{"RunForgeOriginClaimRefused", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.RunForgeOriginClaimRefused(ctx, f.Run)
	}},
	{"GetGithubProjectLinkByRepo", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetGithubProjectLinkByRepo(ctx, f.Repo)
	}},
	{"ListGithubProjectItems", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListGithubProjectItems(ctx, f.Repo)
	}},
	{"ListGuardrailOverrideRequestsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListGuardrailOverrideRequestsForUser(ctx, f.User)
	}},
	{"ListPendingGuardrailOverrideRequests", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListPendingGuardrailOverrideRequests(ctx)
	}},
	{"GetControllerReport", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetControllerReport(ctx)
	}},
	{"GetOpenHealthEpisode", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetOpenHealthEpisode(ctx)
	}},
	{"ListAdmins", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListAdmins(ctx) }},
	{"CountOpenHoldsForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOpenHoldsForRun(ctx, f.Run)
	}},
	{"CountHibernationRetirementCandidates", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountHibernationRetirementCandidates(ctx)
	}},
	{"GetControllerHibernationAdvertisement", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetControllerHibernationAdvertisement(ctx)
	}},
	{"GetHibernationInventoryStateForWorker", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetHibernationInventoryStateForWorker(ctx, f.Worker)
	}},
	{"GetHibernationWake", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetHibernationWake(ctx, f.Run)
	}},
	{"ListHibernationCleanupWorkerIDs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListHibernationCleanupWorkerIDs(ctx)
	}},
	{"ListWakingWorkerIDs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListWakingWorkerIDs(ctx)
	}},
	{"RunHasWakingHibernationWake", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.RunHasWakingHibernationWake(ctx, f.Run)
	}},
	{"WorkerHasPendingWakeDemand", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.WorkerHasPendingWakeDemand(ctx, f.Worker)
	}},
	{"GetWorkerIdleness", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetWorkerIdleness(ctx, f.Worker)
	}},
	{"GetWorkerLongIdleness", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetWorkerLongIdleness(ctx, f.Worker)
	}},
	{"CountEphemeralHostedWorkersForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountEphemeralHostedWorkersForUser(ctx, f.User)
	}},
	{"CountHostedWorkersForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountHostedWorkersForUser(ctx, f.User)
	}},
	{"LeaseClockNow", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.LeaseClockNow(ctx) }},
	{"LockOldestReleasableLeasedEphemeralWorker", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockOldestReleasableLeasedEphemeralWorker(ctx, f.User)
	}},
	{"EnsureInstanceIntakePauseRow", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.EnsureInstanceIntakePauseRow(ctx)
	}},
	{"GetInstanceIntakePause", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetInstanceIntakePause(ctx)
	}},
	{"ListMaintenanceActiveRuns", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMaintenanceActiveRuns(ctx)
	}},
	{"ListMaintenanceCustodyRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMaintenanceCustodyRuns(ctx)
	}},
	{"ListMaintenanceHeldRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMaintenanceHeldRuns(ctx)
	}},
	{"ListMaintenanceIdleBoundWorkers", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMaintenanceIdleBoundWorkers(ctx)
	}},
	{"ListMaintenanceQueuedRuns", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMaintenanceQueuedRuns(ctx)
	}},
	{"LockInstanceIntakePause", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockInstanceIntakePause(ctx)
	}},
	{"MaintenanceSnapshotNow", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.MaintenanceSnapshotNow(ctx)
	}},
	{"RunAdmittableForEphemeralProvision", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.RunAdmittableForEphemeralProvision(ctx, f.Run)
	}},
	{"ListIssueTrackerConnectionsByUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListIssueTrackerConnectionsByUser(ctx, f.User)
	}},
	{"CountReposLinkedToIssueTrackerConnection", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountReposLinkedToIssueTrackerConnection(ctx, f.Conn)
	}},
	{"RepoStoresIssueNumber", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.RepoStoresIssueNumber(ctx, f.Repo)
	}},
	{"DeleteJobOutputRefusalsForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteJobOutputRefusalsForRun(ctx, f.Run)
	}},
	{"NextJobOutputPostID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.NextJobOutputPostID(ctx)
	}},
	{"CountActiveJobRunsForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountActiveJobRunsForUser(ctx, f.User)
	}},
	{"GetJobOriginForRun", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetJobOriginForRun(ctx, f.Run)
	}},
	{"GetJobResultForRun", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetJobResultForRun(ctx, f.Run)
	}},
	{"ListJobFindingsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListJobFindingsForRun(ctx, f.Run)
	}},
	{"ListJobInputsForClaim", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListJobInputsForClaim(ctx, f.Run)
	}},
	{"CurrentTransactionIsolation", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CurrentTransactionIsolation(ctx)
	}},
	{"GetJudgeRunUsageForTarget", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetJudgeRunUsageForTarget(ctx, f.Run)
	}},
	{"GetJudgeSkipForTarget", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetJudgeSkipForTarget(ctx, f.Run)
	}},
	{"GetRunReviewForTarget", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunReviewForTarget(ctx, f.Run)
	}},
	{"LastJudgeEnqueuedAt", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LastJudgeEnqueuedAt(ctx, f.User)
	}},
	{"ListLatestRunLeadConfig", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListLatestRunLeadConfig(ctx, f.Run)
	}},
	{"ListRunLeadConfigHistory", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunLeadConfigHistory(ctx, f.Run)
	}},
	{"GetLatestMemoryParkRelease", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetLatestMemoryParkRelease(ctx, f.Run)
	}},
	{"HasCrossRunLiveClaim", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasCrossRunLiveClaim(ctx, f.Run)
	}},
	{"HasCrossRunSharedFinal", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasCrossRunSharedFinal(ctx, f.Run)
	}},
	{"ListCrossRunFinalCaptures", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListCrossRunFinalCaptures(ctx, f.Run)
	}},
	{"LockCrossRunInventory", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockCrossRunInventory(ctx, f.Run)
	}},
	{"LockCrossRunPublications", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockCrossRunPublications(ctx, f.Run)
	}},
	{"LockCrossRunTarget", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockCrossRunTarget(ctx, f.Run)
	}},
	{"LockCrossRunWitness", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockCrossRunWitness(ctx, f.Run)
	}},
	{"LockMergedPredecessorBinding", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockMergedPredecessorBinding(ctx, f.Repo)
	}},
	{"LockMergedPredecessorRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockMergedPredecessorRun(ctx, f.Run)
	}},
	{"LatestRunMilestoneReconciliationSession", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LatestRunMilestoneReconciliationSession(ctx, f.Run)
	}},
	{"CountModelProvidersForOwner", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountModelProvidersForOwner(ctx, f.User)
	}},
	{"GetUserClaudeProviderSource", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserClaudeProviderSource(ctx, f.User)
	}},
	{"ListModelProvidersForOwner", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListModelProvidersForOwner(ctx, f.User)
	}},
	{"DeleteMRReworkLedgerNotIn", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteMRReworkLedgerNotIn(ctx, f.Repo)
	}},
	{"ListMRReworkCandidates", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMRReworkCandidates(ctx, f.Repo)
	}},
	{"DeleteExpiredOAuthAuthorizeRequests", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteExpiredOAuthAuthorizeRequests(ctx)
	}},
	{"ListLiveOAuthGrantsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListLiveOAuthGrantsForUser(ctx, f.User)
	}},
	{"LockLiveOAuthGrantsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockLiveOAuthGrantsForUser(ctx, f.User)
	}},
	{"WorkerPauseHasStopInput", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.WorkerPauseHasStopInput(ctx, f.Run)
	}},
	{"ListFreshWorkerPendingOutcomeSince", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListFreshWorkerPendingOutcomeSince(ctx, f.Worker)
	}},
	{"PruneWorkerPendingOutcomeIdentities", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.PruneWorkerPendingOutcomeIdentities(ctx, f.Worker)
	}},
	{"ListRunPipelineStatusesForRepo", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunPipelineStatusesForRepo(ctx, f.Repo)
	}},
	{"GetPlanCrossCheckMetadata", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetPlanCrossCheckMetadata(ctx, f.Run)
	}},
	{"HasLivePlanCrossCheck", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasLivePlanCrossCheck(ctx, f.Run)
	}},
	{"HasPendingPlanCrossCheck", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasPendingPlanCrossCheck(ctx, f.Run)
	}},
	{"SupersedeUnstoredPlanApproval", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.SupersedeUnstoredPlanApproval(ctx, f.Run)
	}},
	{"CountPrDescriptionVersionsForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountPrDescriptionVersionsForRun(ctx, f.Run)
	}},
	{"FirstPrDescriptionMrIidForRun", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.FirstPrDescriptionMrIidForRun(ctx, f.Run)
	}},
	{"LatestBoundPrDescriptionVersionForRun", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LatestBoundPrDescriptionVersionForRun(ctx, f.Run)
	}},
	{"LockPrerequisiteRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockPrerequisiteRun(ctx, f.Run)
	}},
	{"ListProductSkillsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListProductSkillsForRun(ctx, f.Run)
	}},
	{"ListEnabledProducts", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledProducts(ctx)
	}},
	{"ListProducts", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListProducts(ctx) }},
	{"AdvancePublicationEpisode", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.AdvancePublicationEpisode(ctx, f.Run)
	}},
	{"CompletePublicationResume", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CompletePublicationResume(ctx, f.Run)
	}},
	{"GetActivePublicationResume", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetActivePublicationResume(ctx, f.Run)
	}},
	{"ClaimCustodyEpisodeNotice", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ClaimCustodyEpisodeNotice(ctx, f.User)
	}},
	{"ListReleasableCustodyHolds", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReleasableCustodyHolds(ctx)
	}},
	{"GetRunEnvRetiredUsage", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunEnvRetiredUsage(ctx, f.User)
	}},
	{"ListRunEnvVarCountsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunEnvVarCountsForUser(ctx, f.User)
	}},
	{"LockRepoKeyShare", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockRepoKeyShare(ctx, f.Repo)
	}},
	{"GetFetchCounters", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetFetchCounters(ctx, f.Run)
	}},
	{"GetRunEgressSnapshot", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunEgressSnapshot(ctx, f.Run)
	}},
	{"CountRunUsageLegs", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountRunUsageLegs(ctx, f.Run)
	}},
	{"CountRunUsageMessages", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountRunUsageMessages(ctx, f.Run)
	}},
	{"GetRunUsageTailState", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunUsageTailState(ctx, f.Run)
	}},
	{"ListRunUsageLegsForTail", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunUsageLegsForTail(ctx, f.Run)
	}},
	{"ListRunUsageTailMessages", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunUsageTailMessages(ctx, f.Run)
	}},
	{"AdminUsagePerUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.AdminUsagePerUser(ctx)
	}},
	{"AdminUsageTotals", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.AdminUsageTotals(ctx) }},
	{"ConsumeRunInputs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ConsumeRunInputs(ctx, f.Run)
	}},
	{"CountInProgressRunsForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountInProgressRunsForUser(ctx, f.User)
	}},
	{"CountOnlineWorkersForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersForUser(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingAgentSourceRequest", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingAgentSourceRequest(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingCIArtifacts", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingCIArtifacts(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingCIConfigGuard", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingCIConfigGuard(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingClaudeProvider", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingClaudeProvider(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingCodexHarness", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingCodexHarness(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingCodexRuntime", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingCodexRuntime(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingCustomCodex", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingCustomCodex(ctx, store.CountOnlineWorkersSatisfyingCustomCodexParams{UserID: f.User})
	}},
	{"CountOnlineWorkersSatisfyingPhaseModels", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingPhaseModels(ctx, store.CountOnlineWorkersSatisfyingPhaseModelsParams{UserID: f.User, Harness: "claude"})
	}},
	{"CountOnlineWorkersSatisfyingProtocol", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingProtocol(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingRepoAgentSource", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingRepoAgentSource(ctx, f.User)
	}},
	{"CountOnlineWorkersSatisfyingRepoAgentSourceAndRequest", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersSatisfyingRepoAgentSourceAndRequest(ctx, f.User)
	}},
	{"CountOnlineWorkersWithFreeSlotForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountOnlineWorkersWithFreeSlotForUser(ctx, f.User)
	}},
	{"CountRunReviseInputs", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountRunReviseInputs(ctx, f.Run)
	}},
	{"CountRunsAwaitingUsageRefold", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountRunsAwaitingUsageRefold(ctx)
	}},
	{"DeleteWorkerActiveRuns", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteWorkerActiveRuns(ctx, f.Worker)
	}},
	{"DeleteWorkerActiveRunsPreservingPolicyLease", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteWorkerActiveRunsPreservingPolicyLease(ctx, f.Worker)
	}},
	{"FactoryPRCounts", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.FactoryPRCounts(ctx) }},
	{"GetFollowupFinalizeBlockers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetFollowupFinalizeBlockers(ctx, f.Run)
	}},
	{"GetForgeTypeForRepo", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetForgeTypeForRepo(ctx, f.Repo)
	}},
	{"GetPlanCrossCheck", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetPlanCrossCheck(ctx, f.Run)
	}},
	{"GetPlanCrossCheckChildForHealth", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetPlanCrossCheckChildForHealth(ctx, f.Run)
	}},
	{"GetRunByID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunByID(ctx, f.Run)
	}},
	{"GetRunByIDForUpdate", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunByIDForUpdate(ctx, f.Run)
	}},
	{"GetRunClaimContext", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunClaimContext(ctx, f.Run)
	}},
	{"GetRunCredentialSwitchOpID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunCredentialSwitchOpID(ctx, f.Run)
	}},
	{"GetRunMilestoneFreezeSnapshot", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunMilestoneFreezeSnapshot(ctx, f.Run)
	}},
	{"GetRunMoveContext", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunMoveContext(ctx, f.Run)
	}},
	{"GetRunUsageTotal", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunUsageTotal(ctx, f.Run)
	}},
	{"GetWorkerByID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetWorkerByID(ctx, f.Worker)
	}},
	{"GetWorkerForUpdate", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetWorkerForUpdate(ctx, f.Worker)
	}},
	{"IterationParkHasPendingControl", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.IterationParkHasPendingControl(ctx, f.Run)
	}},
	{"LatestPlanSeqForRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LatestPlanSeqForRun(ctx, f.Run)
	}},
	{"LeadAgentDispatchFramesForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LeadAgentDispatchFramesForRun(ctx, f.Run)
	}},
	{"LeadDispatchCompletionIDsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LeadDispatchCompletionIDsForRun(ctx, f.Run)
	}},
	{"ListAllWorkers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListAllWorkers(ctx) }},
	{"ListConsumedFollowUpInputsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListConsumedFollowUpInputsForRun(ctx, f.Run)
	}},
	{"ListFollowUpInputsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListFollowUpInputsForRun(ctx, f.Run)
	}},
	{"ListPoolWaitRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListPoolWaitRuns(ctx) }},
	{"ListReplayRunInputs", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReplayRunInputs(ctx, f.Run)
	}},
	{"ListRunUsageByCredentialEpoch", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunUsageByCredentialEpoch(ctx, f.Run)
	}},
	{"ListRunUsageFrames", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunUsageFrames(ctx, f.Run)
	}},
	{"ListWaitingWorkerRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListWaitingWorkerRuns(ctx)
	}},
	{"ListWorkerPendingSince", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListWorkerPendingSince(ctx, f.Worker)
	}},
	{"ListWorkersByUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListWorkersByUser(ctx, f.User)
	}},
	{"LiveLaneFramesForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LiveLaneFramesForRun(ctx, f.Run)
	}},
	{"LockRunForInputReceipt", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockRunForInputReceipt(ctx, f.Run)
	}},
	{"MaxRunMessageSeq", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.MaxRunMessageSeq(ctx, f.Run)
	}},
	{"OldestUndispatchedTaskRun", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.OldestUndispatchedTaskRun(ctx)
	}},
	{"PRCountsPerUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.PRCountsPerUser(ctx) }},
	{"PromoteVaultLockedRecoveryWaitRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.PromoteVaultLockedRecoveryWaitRuns(ctx, f.User)
	}},
	{"ReadoptRunsFromSnapshot", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ReadoptRunsFromSnapshot(ctx, f.Worker)
	}},
	{"SelfPRCounts", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.SelfPRCounts(ctx, f.User)
	}},
	{"SupersedeExitedPlanCrossChecks", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.SupersedeExitedPlanCrossChecks(ctx)
	}},
	{"GetRunSalvage", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunSalvage(ctx, f.Run)
	}},
	{"ClaimDueSchedules", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ClaimDueSchedules(ctx)
	}},
	{"GetRunSchedule", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetRunSchedule(ctx, f.Run)
	}},
	{"ListEnabledDefaultsForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListEnabledDefaultsForUser(ctx, f.User)
	}},
	{"ListRunSchedulesForUser", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListRunSchedulesForUser(ctx, f.User)
	}},
	{"CountActiveSelfImproveRunsForRepo", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountActiveSelfImproveRunsForRepo(ctx, f.Repo)
	}},
	{"GetSemanticObservations", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetSemanticObservations(ctx, f.Run)
	}},
	{"GetSemanticPlanning", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetSemanticPlanning(ctx, f.Run)
	}},
	{"GetSemanticSnapshotBytes", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetSemanticSnapshotBytes(ctx, f.Run)
	}},
	{"GetVaultPolicy", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.GetVaultPolicy(ctx) }},
	{"ListAppSettings", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListAppSettings(ctx) }},
	{"ListAppSettingsForUpdate", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListAppSettingsForUpdate(ctx)
	}},
	{"CountRunPlanMessages", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountRunPlanMessages(ctx, f.Run)
	}},
	{"GetLatestRunQuestion", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetLatestRunQuestion(ctx, f.Run)
	}},
	{"GetSlackRunMessage", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetSlackRunMessage(ctx, f.Run)
	}},
	{"GetUserSlackLink", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserSlackLink(ctx, f.User)
	}},
	{"ListSlackNotifiableUsers", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListSlackNotifiableUsers(ctx)
	}},
	{"ListUsersForSlackLink", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListUsersForSlackLink(ctx)
	}},
	{"GetTaskReviewForTarget", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTaskReviewForTarget(ctx, f.Run)
	}},
	{"GetTerminalPreservationByRun", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTerminalPreservationByRun(ctx, f.Run)
	}},
	{"GetTerminalPreservationForShare", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTerminalPreservationForShare(ctx, f.Run)
	}},
	{"GetTerminalPreservationForUpdate", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTerminalPreservationForUpdate(ctx, f.Run)
	}},
	{"GetTerminalRetireBlocker", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTerminalRetireBlocker(ctx, f.Run)
	}},
	{"GetTerminalSweepClock", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetTerminalSweepClock(ctx)
	}},
	{"ListTerminalContributionsForRun", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListTerminalContributionsForRun(ctx, f.Run)
	}},
	{"ListToolAllowlist", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListToolAllowlist(ctx)
	}},
	{"CountEnabledCodexSecrets", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountEnabledCodexSecrets(ctx, f.User)
	}},
	{"CountMasterSealedSecrets", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountMasterSealedSecrets(ctx)
	}},
	{"ListMasterSealedSecrets", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListMasterSealedSecrets(ctx, f.User)
	}},
	{"ListSecretEnablement", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListSecretEnablement(ctx, f.User)
	}},
	{"ListUserSecretsMeta", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListUserSecretsMeta(ctx, f.User)
	}},
	{"UserHasAutoEligibleAnthropicToken", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.UserHasAutoEligibleAnthropicToken(ctx, f.User)
	}},
	{"UserHasEnabledAnthropicToken", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.UserHasEnabledAnthropicToken(ctx, f.User)
	}},
	{"ClaimVaultLockNotice", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ClaimVaultLockNotice(ctx, f.User)
	}},
	{"CountUnprotectedUsers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.CountUnprotectedUsers(ctx)
	}},
	{"DeleteUserVault", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.DeleteUserVault(ctx, f.User)
	}},
	{"GetUserVault", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserVault(ctx, f.User)
	}},
	{"GetUserVaultState", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserVaultState(ctx, f.User)
	}},
	{"ListUsersNeedingVaultLockNotice", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListUsersNeedingVaultLockNotice(ctx)
	}},
	{"LockUserVault", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockUserVault(ctx, f.User)
	}},
	{"CountUsers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.CountUsers(ctx) }},
	{"GetUserAttributionEnabled", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserAttributionEnabled(ctx, f.User)
	}},
	{"GetUserByEmail", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserByEmail(ctx, f.Email)
	}},
	{"GetUserByID", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserByID(ctx, f.User)
	}},
	{"GetUserDefaultCodexEffort", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserDefaultCodexEffort(ctx, f.User)
	}},
	{"GetUserDefaultEffort", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserDefaultEffort(ctx, f.User)
	}},
	{"GetUserDefaultHarness", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserDefaultHarness(ctx, f.User)
	}},
	{"GetUserHarnessModelDefaults", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserHarnessModelDefaults(ctx, f.User)
	}},
	{"GetUserJudgeAnthropicBinding", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserJudgeAnthropicBinding(ctx, f.User)
	}},
	{"GetUserJudgeModels", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserJudgeModels(ctx, f.User)
	}},
	{"GetUserNowSummaryEnabled", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserNowSummaryEnabled(ctx, f.User)
	}},
	{"GetUserPlanningClaude", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserPlanningClaude(ctx, f.User)
	}},
	{"GetUserSchedulePause", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserSchedulePause(ctx, f.User)
	}},
	{"GetUserSettings", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserSettings(ctx, f.User)
	}},
	{"GetUserSummaryModels", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserSummaryModels(ctx, f.User)
	}},
	{"GetUserTaskReviewSettings", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserTaskReviewSettings(ctx, f.User)
	}},
	{"GetUserUsageReserve", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetUserUsageReserve(ctx, f.User)
	}},
	{"ListUsers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListUsers(ctx) }},
	{"GetVaultReunlockDays", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetVaultReunlockDays(ctx)
	}},
	{"GetVaultMaterialTotals", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetVaultMaterialTotals(ctx, f.User)
	}},
	{"HasMasterVaultMaterial", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.HasMasterVaultMaterial(ctx, f.User)
	}},
	{"LockVaultEnvConnectionsKeyShare", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockVaultEnvConnectionsKeyShare(ctx, f.User)
	}},
	{"LockVaultEnvReposKeyShare", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.LockVaultEnvReposKeyShare(ctx, f.User)
	}},
	{"ListQueuedRunsForReserve", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListQueuedRunsForReserve(ctx, f.User)
	}},
	{"ListReserveMarkOwners", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReserveMarkOwners(ctx)
	}},
	{"ListReserveOverrideRuns", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReserveOverrideRuns(ctx, f.User)
	}},
	{"ListReservePauseOwners", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReservePauseOwners(ctx)
	}},
	{"ListReservePauseRunsForOwner", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReservePauseRunsForOwner(ctx, f.User)
	}},
	{"ListReserveWorkers", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.ListReserveWorkers(ctx, f.User)
	}},
	{"GetWorkerUpgradeSummaryForUser", pmSome, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetWorkerUpgradeSummaryForUser(ctx, f.User)
	}},
	{"GetControllerAdminSizesAdvertisement", pmNoRows, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) {
		return q.GetControllerAdminSizesAdvertisement(ctx)
	}},
	{"ListWorkerSizes", pmEmpty, func(ctx context.Context, q *store.Queries, f *pmFixture) (any, error) { return q.ListWorkerSizes(ctx) }},
}

var (
	pmSQLOnce sync.Once
	pmSQLMap  map[string]string
	pmSQLErr  error
	pmSimple  map[string]struct{}
)

// pmSimpleExecNames are the sqlc :exec methods with no parameters. pgx sends a zero-argument
// Exec over the simple protocol, so it is never prepared and never occupies a statement
// cache slot: such statements are not counted as identities.
var pmSimpleExecNames = []string{
	"DeleteAgentSourceStaged", "DeleteSharedTemplateAllocations", "OptInCheckpointCoordinates",
	"DeleteExpiredCLIAuthRequests", "EnsureInstanceIntakePauseRow", "DeleteExpiredOAuthAuthorizeRequests",
	"OptInPublicationResume", "OptInRunFences",
}

// pmSQLLoad parses the generated sqlc files once, on first use inside the opt-in test (never
// at package init, so no other store test binary pays for or can panic on the parse).
func pmSQLLoad() (map[string]string, error) {
	pmSQLOnce.Do(func() {
		pmSQLMap, _, pmSQLErr = pmSQLConstants()
		if pmSQLErr != nil {
			return
		}
		pmSimple = map[string]struct{}{}
		for _, n := range pmSimpleExecNames {
			txt, ok := pmSQLMap[n]
			if !ok {
				pmSQLErr = fmt.Errorf("zero-argument exec %s has no sqlc query text", n)
				return
			}
			pmSimple[txt] = struct{}{}
		}
	})
	return pmSQLMap, pmSQLErr
}

// pmSQLTexts maps each sqlc method name to its query text, the exact string pgx prepares.
// The test checks pmSQLLoad's error before any use, so a failure here is a harness bug.
func pmSQLTexts() map[string]string {
	m, err := pmSQLLoad()
	if err != nil {
		panic(err)
	}
	return m
}

// pmNotPrepared reports whether sql is a zero-argument sqlc Exec text, which pgx runs on the
// simple protocol without preparing it.
func pmNotPrepared(sql string) bool {
	pmSQLTexts()
	_, ok := pmSimple[sql]
	return ok
}

// ---------------------------------------------------------------------------------------
// Eviction then re-prepare

type pmEviction struct {
	Cand             string `json:"cand"`
	Rep              int    `json:"rep"`
	Capacity         int    `json:"capacity"`
	Entry            string `json:"early_statement_method"`
	DistinctExecuted int    `json:"distinct_statements_executed"`
	Exceeded         bool   `json:"working_set_exceeds_capacity"`
	PresentFirst     bool   `json:"present_after_first_execution"`
	PresentAfterFill bool   `json:"present_after_working_set_executed"`
	PresentReexec    bool   `json:"present_after_reexecution"`
	RetainedFirst    int    `json:"retained_after_first_execution"`
	RetainedAfter    int    `json:"retained_after_working_set_executed"`
	RetainedReexec   int    `json:"retained_after_reexecution"`
}

// evictionDemo executes an early breadth statement, then enough distinct others to push it
// out of a capped LRU cache, and shows through pg_prepared_statements (matched on statement
// text) that it was deallocated and is prepared again by re-execution. For an uncapped
// candidate whose cache the working set does not exceed it shows the statement stays.
func (e *pmEnv) evictionDemo(ctx context.Context) pmEviction {
	t := e.t
	capacity := e.cand.capacity()
	first := pmBreadth[0]
	text, ok := pmSQLTexts()[first.Name]
	if !ok {
		t.Fatalf("%s: no sqlc text for %s", e.cand.Name, first.Name)
	}
	tx := e.begin(ctx)
	q := store.New(tx)
	probe := func() (bool, int) {
		var present, retained int
		if err := tx.QueryRow(pmSkip(ctx),
			`SELECT (count(*) FILTER (WHERE statement = $1))::int, count(*)::int FROM pg_prepared_statements`,
			pgx.QueryExecModeSimpleProtocol, text).Scan(&present, &retained); err != nil {
			t.Fatalf("%s: eviction probe: %v", e.cand.Name, err)
		}
		return present > 0, retained
	}
	run := func(en pmEntry) {
		if err := pmSavepoint(ctx, tx, func() error { return pmRunEntry(ctx, q, e.fx, en) }); err != nil {
			t.Fatalf("%s: eviction demo: %v", e.cand.Name, err)
		}
	}
	ev := pmEviction{Cand: e.cand.Name, Capacity: capacity, Entry: first.Name}
	run(first)
	ev.PresentFirst, ev.RetainedFirst = probe()
	ev.DistinctExecuted = 1
	for _, en := range pmBreadth[1:] {
		if ev.DistinctExecuted >= capacity+8 {
			break
		}
		run(en)
		ev.DistinctExecuted++
	}
	ev.Exceeded = ev.DistinctExecuted > capacity
	ev.PresentAfterFill, ev.RetainedAfter = probe()
	run(first)
	ev.PresentReexec, ev.RetainedReexec = probe()
	e.rollback(ctx, tx)
	switch {
	case !ev.PresentFirst:
		t.Fatalf("%s: %s not prepared after its first execution", e.cand.Name, first.Name)
	case ev.Exceeded && ev.PresentAfterFill:
		t.Fatalf("%s: %s still prepared after %d distinct statements > capacity %d: no eviction", e.cand.Name, first.Name, ev.DistinctExecuted, capacity)
	case ev.Exceeded && !ev.PresentReexec:
		t.Fatalf("%s: %s not re-prepared by re-execution after eviction", e.cand.Name, first.Name)
	case !ev.Exceeded && (!ev.PresentAfterFill || !ev.PresentReexec):
		t.Fatalf("%s: %s lost although %d distinct statements do not exceed capacity %d", e.cand.Name, first.Name, ev.DistinctExecuted, capacity)
	case ev.RetainedAfter > capacity || (ev.Exceeded && ev.RetainedAfter != capacity):
		t.Fatalf("%s: retained %d after filling, capacity %d", e.cand.Name, ev.RetainedAfter, capacity)
	}
	ev.Rep = e.rep
	e.out.emit("eviction", ev)
	return ev
}

// ---------------------------------------------------------------------------------------
// Memory run per candidate

type pmRoundRec struct {
	Cand          string  `json:"cand"`
	Rep           int     `json:"rep"`
	Round         int     `json:"round"`
	DurationS     float64 `json:"duration_s"`
	ClaimMeanUS   float64 `json:"claim_run_mean_us"`
	InsertMeanUS  float64 `json:"insert_run_message_mean_us"`
	PIDsSoFar     int     `json:"backend_pids_so_far"`
	BreadthCursor int     `json:"breadth_cursor"`
}

// pmMemSummary's SampledPeakAnonKB and SampledPeakKB are interval-sampler readings only, from
// the sampler's start; they exclude the baseline sample, evictionDemo and the post-round
// samples. VmHWMMaxKB mixes sampler and held samples.
type pmMemSummary struct {
	Cand               string           `json:"cand"`
	Rep                int              `json:"rep"`
	Database           string           `json:"clone_database"`
	Desc               string           `json:"desc"`
	Rounds             int              `json:"rounds"`
	DurationS          float64          `json:"duration_s"`
	Recycles           int              `json:"recycles"`
	RecycleAtS         []float64        `json:"recycle_at_s_since_candidate_start"`
	BackendPIDs        int              `json:"backend_pids"`
	QualifiedSamples   int              `json:"steady_state_samples"`
	SteadyPrivateKB    int64            `json:"steady_private_kb_clean_plus_dirty"`
	SteadyPrivateMin   int64            `json:"steady_private_kb_min"`
	SteadyPrivateMax   int64            `json:"steady_private_kb_max"`
	SteadyAnonymousKB  int64            `json:"steady_anonymous_kb"`
	SteadyAnonMinKB    int64            `json:"steady_anonymous_kb_min"`
	SteadyAnonMaxKB    int64            `json:"steady_anonymous_kb_max"`
	SampledPeakAnonKB  int64            `json:"sampled_peak_anonymous_kb"`
	SteadyPrivCleanKB  int64            `json:"steady_private_clean_kb"`
	SteadyPrivDirtyKB  int64            `json:"steady_private_dirty_kb"`
	SteadyPssShmemKB   *int64           `json:"steady_pss_shmem_kb_includes_proportional_share_of_shared_shmem,omitempty"`
	SteadyRssKB        int64            `json:"steady_rss_kb"`
	SteadyPssKB        int64            `json:"steady_pss_kb"`
	SteadyCgroupB      *int64           `json:"steady_cgroup_memory_current_bytes"`
	CgroupNote         string           `json:"cgroup_note,omitempty"`
	SteadyCtxTotalB    int64            `json:"steady_context_total_bytes"`
	SteadyCtxUsedB     int64            `json:"steady_context_used_bytes"`
	SteadyCacheCtxB    int64            `json:"steady_cache_memory_context_bytes"`
	SteadyRetained     int              `json:"steady_retained_statements"`
	MaxRetained        int              `json:"max_retained_statements"`
	MaxIdentities      int              `json:"max_identities_per_pid"`
	SteadyGeneric      int64            `json:"steady_generic_plans"`
	SteadyCustom       int64            `json:"steady_custom_plans"`
	BaselineCtxTotalB  int64            `json:"baseline_empty_cache_context_total_bytes"`
	BytesPerStmtApprox float64          `json:"bytes_per_cached_statement_approx"`
	VmHWMMaxKB         int64            `json:"vmhwm_max_kb_includes_shared"`
	SampledPeakKB      int64            `json:"sampled_peak_private_kb"`
	Sampler            pmSamplerResult  `json:"sampler"`
	AgeBucketsPrivate  map[string]int64 `json:"median_private_kb_by_conn_age_bucket,omitempty"`
	AgeBucketsAnon     map[string]int64 `json:"median_anonymous_kb_by_conn_age_bucket,omitempty"`
	JITSetting         string           `json:"jit_setting_requested"`
	JITOnBackends      []string         `json:"show_jit_per_backend"`
	LLVMMapped         bool             `json:"llvm_mapped_any_post_round_sample"`
	LLVMPaths          []string         `json:"llvm_paths"`
	LLVMFirstRound     *int             `json:"llvm_first_round"`
	LLVMFirstConnAgeS  *float64         `json:"llvm_first_conn_age_s"`
}

func pmMedianOf(ss []pmSample, f func(pmSample) int64) int64 {
	var xs []int64
	for _, s := range ss {
		xs = append(xs, f(s))
	}
	return pmMedianInt(xs)
}

func (e *pmEnv) runMemory(ctx context.Context) pmMemSummary {
	t := e.t
	name := e.cand.Name
	e.openPool(ctx)
	defer e.closePool()
	candStart := time.Now()
	e.start = candStart
	base := e.postRoundSample(ctx, "baseline_empty_cache", 0)
	e.evictionDemo(ctx)
	// Everything from the sampler start through the candidate_summary is the body of
	// pmWithSampler, so every exit (including a t.Fatalf) cancels and joins the sampler before
	// the deferred closePool above and before the caller's clone closes the observer.
	var sum pmMemSummary
	pmWithSampler(func() *pmSampler { return e.startSampler(ctx) }, func(sampler *pmSampler) {
		minRounds := pmEnvMinInt(t, "UZI_POOL_MEMORY_ROUNDS", 20, 1)
		maxWait := pmEnvDuration("UZI_POOL_MEMORY_MAX_RECYCLE_WAIT", 12*time.Minute)
		var post []pmSample
		rounds, rerr := pmRoundLoop(minRounds, e.cand.Recycle, maxWait, func() time.Duration { return time.Since(candStart) },
			func(r int) int {
				t0 := time.Now()
				e.hotMix(ctx, pmHotReps)
				lat := e.latencyLoop(ctx, pmLatencyN, 1, r == 1)
				e.verifyFixture(ctx)
				s := e.postRoundSample(ctx, "post_round", r)
				post = append(post, s)
				e.mu.Lock()
				npids := len(e.order)
				e.mu.Unlock()
				e.out.emit("round", pmRoundRec{Cand: name, Rep: e.rep, Round: r, DurationS: time.Since(t0).Seconds(),
					ClaimMeanUS: pmMean(lat.ClaimUS), InsertMeanUS: pmMean(lat.InsertUS), PIDsSoFar: npids, BreadthCursor: e.cursor})
				return npids
			})
		if rerr != nil {
			t.Fatalf("%s: %v", name, rerr)
		}
		sres := sampler.Stop()
		if sres.N == 0 || sres.Errors > 0 {
			t.Fatalf("%s: sampler took %d samples with %d errors (first: %s)", name, sres.N, sres.Errors, sres.FirstError)
		}

		e.mu.Lock()
		sum = pmMemSummary{Cand: name, Rep: e.rep, Database: e.dbName, Desc: e.cand.Desc, Rounds: rounds, DurationS: time.Since(candStart).Seconds(),
			BackendPIDs: len(e.order), Sampler: sres, SampledPeakKB: sres.PeakKB, JITSetting: e.jitSetting}
		var pidRecs []pmPID
		llvmUnion := map[string]struct{}{}
		for _, pid := range e.order {
			p := *e.pids[pid]
			p.Idents = nil
			pidRecs = append(pidRecs, p)
			sum.JITOnBackends = append(sum.JITOnBackends, p.JIT)
			if p.LLVMSeen {
				sum.LLVMMapped = true
				if sum.LLVMFirstRound == nil || p.LLVMRound < *sum.LLVMFirstRound {
					r, a := p.LLVMRound, p.LLVMAgeS
					sum.LLVMFirstRound, sum.LLVMFirstConnAgeS = &r, &a
				}
				for lp := range p.LLVMPaths {
					llvmUnion[lp] = struct{}{}
				}
			}
			if n := len(e.pids[pid].Idents); n > sum.MaxIdentities {
				sum.MaxIdentities = n
			}
			if p.HWMKB > sum.VmHWMMaxKB {
				sum.VmHWMMaxKB = p.HWMKB
			}
		}
		e.mu.Unlock()
		sum.LLVMPaths = pmSortedKeys(llvmUnion)
		sum.Recycles = len(pidRecs) - 1
		for _, p := range pidRecs[1:] {
			sum.RecycleAtS = append(sum.RecycleAtS, p.ConnectedAt.Sub(candStart).Seconds())
		}
		for _, p := range pidRecs {
			life := time.Since(p.ConnectedAt)
			if !p.ClosedAt.IsZero() {
				life = p.ClosedAt.Sub(p.ConnectedAt)
			}
			// The peak_* fields are interval-sampler readings only (from sampler start, excluding the
			// baseline, evictionDemo and post-round samples); vmhwm mixes sampler and held samples.
			e.out.emit("pid", map[string]any{"cand": name, "rep": e.rep, "pid": p.PID, "connected_at": p.ConnectedAt, "closed_at": p.ClosedAt,
				"lifetime_s": life.Seconds(), "executions_served": p.Executions, "identities": len(e.pids[p.PID].Idents),
				"peak_sampled_anonymous_kb": p.PeakAnonKB, "peak_anonymous_at_conn_age_s": p.PeakAnonAge,
				"peak_sampled_private_kb": p.PeakPrivateKB, "peak_private_at_conn_age_s": p.PeakSampleAge, "vmhwm_kb_includes_shared": p.HWMKB,
				"statement_cache_capacity": p.StmtCapacity, "plan_cache_mode": p.PlanCacheMode, "show_jit": p.JIT,
				"llvm_mapped": p.LLVMSeen, "llvm_first_round": p.LLVMRound, "llvm_first_conn_age_s": p.LLVMAgeS, "llvm_paths": pmSortedKeys(p.LLVMPaths)})
			if p.StmtCapacity != e.cand.capacity() || p.PlanCacheMode != "auto" {
				t.Fatalf("%s: pid %d ran with statement cache capacity %d (want %d), plan_cache_mode %q (want auto)",
					name, p.PID, p.StmtCapacity, e.cand.capacity(), p.PlanCacheMode)
			}
		}

		// Steady state: post-round samples whose backend has executed the full working set,
		// the later half of them.
		var qual []pmSample
		for _, s := range post {
			if s.Identities >= pmMinIdentities {
				qual = append(qual, s)
			}
		}
		if len(qual) < 3 {
			t.Fatalf("%s: only %d post-round samples have >= %d distinct identities on their backend", name, len(qual), pmMinIdentities)
		}
		if sum.MaxIdentities < pmMinIdentities {
			t.Fatalf("%s: max identities per backend %d < %d", name, sum.MaxIdentities, pmMinIdentities)
		}
		if e.cand.Recycle && sum.Recycles < 3 {
			t.Fatalf("%s: only %d recycles in %.0fs, want >= 3", name, sum.Recycles, sum.DurationS)
		}
		steady := qual[len(qual)/2:]
		sum.QualifiedSamples = len(steady)
		sum.SteadyPrivateKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.PrivateKB })
		sum.SteadyPrivateMin, sum.SteadyPrivateMax = steady[0].Mem.PrivateKB, steady[0].Mem.PrivateKB
		for _, s := range steady {
			if s.Mem.PrivateKB < sum.SteadyPrivateMin {
				sum.SteadyPrivateMin = s.Mem.PrivateKB
			}
			if s.Mem.PrivateKB > sum.SteadyPrivateMax {
				sum.SteadyPrivateMax = s.Mem.PrivateKB
			}
		}
		sum.SteadyAnonymousKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.AnonymousKB })
		sum.SteadyAnonMinKB, sum.SteadyAnonMaxKB = steady[0].Mem.AnonymousKB, steady[0].Mem.AnonymousKB
		for _, s := range steady {
			sum.SteadyAnonMinKB = min(sum.SteadyAnonMinKB, s.Mem.AnonymousKB)
			sum.SteadyAnonMaxKB = max(sum.SteadyAnonMaxKB, s.Mem.AnonymousKB)
		}
		sum.SampledPeakAnonKB = sres.PeakAnonKB
		sum.SteadyPrivCleanKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.PrivateCleanKB })
		sum.SteadyPrivDirtyKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.PrivateDirtyKB })
		if steady[0].Mem.PssShmemKB != nil {
			v := pmMedianOf(steady, func(s pmSample) int64 {
				if s.Mem.PssShmemKB == nil {
					return 0
				}
				return *s.Mem.PssShmemKB
			})
			sum.SteadyPssShmemKB = &v
		}
		sum.SteadyRssKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.RssKB })
		sum.SteadyPssKB = pmMedianOf(steady, func(s pmSample) int64 { return s.Mem.PssKB })
		if steady[0].Mem.CgroupCurrentB != nil {
			v := pmMedianOf(steady, func(s pmSample) int64 {
				if s.Mem.CgroupCurrentB == nil {
					return 0
				}
				return *s.Mem.CgroupCurrentB
			})
			sum.SteadyCgroupB = &v
		} else {
			sum.CgroupNote = steady[0].Mem.CgroupNote
		}
		sum.SteadyCtxTotalB = pmMedianOf(steady, func(s pmSample) int64 { return s.Ctx.TotalBytes })
		sum.SteadyCtxUsedB = pmMedianOf(steady, func(s pmSample) int64 { return s.Ctx.UsedBytes })
		sum.SteadyCacheCtxB = pmMedianOf(steady, func(s pmSample) int64 { return s.Ctx.CacheMemoryBytes })
		sum.SteadyRetained = int(pmMedianOf(steady, func(s pmSample) int64 { return int64(*s.Retained) }))
		sum.SteadyGeneric = pmMedianOf(steady, func(s pmSample) int64 { return *s.Generic })
		sum.SteadyCustom = pmMedianOf(steady, func(s pmSample) int64 { return *s.Custom })
		for _, s := range post {
			if *s.Retained > sum.MaxRetained {
				sum.MaxRetained = *s.Retained
			}
		}
		sum.BaselineCtxTotalB = base.Ctx.TotalBytes
		if sum.SteadyRetained > 0 {
			sum.BytesPerStmtApprox = float64(sum.SteadyCtxTotalB-sum.BaselineCtxTotalB) / float64(sum.SteadyRetained)
		}
		if e.cand.Recycle {
			buckets := map[string][]int64{}
			anonBuckets := map[string][]int64{}
			for _, s := range post {
				b := fmt.Sprintf("%02d-%02ds", int(s.ConnAgeS)/10*10, int(s.ConnAgeS)/10*10+10)
				buckets[b] = append(buckets[b], s.Mem.PrivateKB)
				anonBuckets[b] = append(anonBuckets[b], s.Mem.AnonymousKB)
			}
			sum.AgeBucketsPrivate = map[string]int64{}
			sum.AgeBucketsAnon = map[string]int64{}
			for b, xs := range buckets {
				sum.AgeBucketsPrivate[b] = pmMedianInt(xs)
				sum.AgeBucketsAnon[b] = pmMedianInt(anonBuckets[b])
			}
		}
		e.out.emit("candidate_summary", sum)
	})
	return sum
}

// ---------------------------------------------------------------------------------------
// Latency comparison (ADR-1620's method)

type pmLatRound struct {
	Cand         string  `json:"cand"`
	Variant      string  `json:"variant"`
	Round        int     `json:"round"`
	Order        int     `json:"order_in_round"`
	N            int     `json:"n"`
	BreadthPerIt int     `json:"breadth_calls_per_iteration"`
	ClaimMeanUS  float64 `json:"claim_run_mean_us"`
	ClaimP50US   float64 `json:"claim_run_p50_us"`
	ClaimP95US   float64 `json:"claim_run_p95_us"`
	InsMeanUS    float64 `json:"insert_run_message_mean_us"`
	InsP50US     float64 `json:"insert_run_message_p50_us"`
	InsP95US     float64 `json:"insert_run_message_p95_us"`
}

type pmLatSummary struct {
	Cand            string    `json:"cand"`
	Variant         string    `json:"variant"`
	ClaimMedianMean float64   `json:"claim_run_median_round_mean_us"`
	InsMedianMean   float64   `json:"insert_run_message_median_round_mean_us"`
	ClaimRounds     []float64 `json:"claim_run_round_means_us"`
	InsRounds       []float64 `json:"insert_run_message_round_means_us"`
	ClaimP50US      float64   `json:"claim_run_pooled_p50_us"`
	ClaimP95US      float64   `json:"claim_run_pooled_p95_us"`
	InsP50US        float64   `json:"insert_run_message_pooled_p50_us"`
	InsP95US        float64   `json:"insert_run_message_pooled_p95_us"`
}

// pmRunLatency runs pmLatencyRounds rounds; each round measures every candidate on a fresh
// pool in an order rotated by the round number, so with pmLatencyRounds equal to the number
// of candidates each candidate goes first in exactly one round and last in exactly one.
// The headline is the median round's per-execution mean.
//
// Every round runs on its own fresh clone of the seeded template, so no round inherits the
// dead tuples the previous round's rolled-back writes left behind; the candidates inside one
// round share that round's clone and the rotation balances their position in it.
func pmRunLatency(ctx context.Context, lab *pmLab, cands []pmCand, variant string, n, k int) {
	t, out := lab.t, lab.out
	pooledClaim := map[string][]float64{}
	pooledIns := map[string][]float64{}
	roundClaim := map[string][]float64{}
	roundIns := map[string][]float64{}
	for r := 0; r < pmLatencyRounds; r++ {
		lab.withClone(ctx, fmt.Sprintf("lat_%s_r%d", variant, r+1), func(_, dsn string, obs *pmObserver) {
			for i := range cands {
				c := cands[(i+r)%len(cands)]
				e := newPMEnv(t, out, dsn, c, lab.fx, obs, lab.rawDir)
				e.openPool(ctx)
				t.Cleanup(e.closePool) // a t.Fatalf inside latencyLoop still rolls back and closes
				lat := e.latencyLoop(ctx, n, k, true)
				e.verifyFixture(ctx)
				e.closePool()
				rec := pmLatRound{Cand: c.Name, Variant: variant, Round: r + 1, Order: i + 1, N: n, BreadthPerIt: k,
					ClaimMeanUS: pmMean(lat.ClaimUS), ClaimP50US: pmQuantile(lat.ClaimUS, 0.5), ClaimP95US: pmQuantile(lat.ClaimUS, 0.95),
					InsMeanUS: pmMean(lat.InsertUS), InsP50US: pmQuantile(lat.InsertUS, 0.5), InsP95US: pmQuantile(lat.InsertUS, 0.95)}
				out.emit("latency_round", rec)
				pooledClaim[c.Name] = append(pooledClaim[c.Name], lat.ClaimUS...)
				pooledIns[c.Name] = append(pooledIns[c.Name], lat.InsertUS...)
				roundClaim[c.Name] = append(roundClaim[c.Name], rec.ClaimMeanUS)
				roundIns[c.Name] = append(roundIns[c.Name], rec.InsMeanUS)
			}
		})
	}
	for _, c := range cands {
		out.emit("latency_summary", pmLatSummary{Cand: c.Name, Variant: variant,
			ClaimMedianMean: pmQuantile(roundClaim[c.Name], 0.5), InsMedianMean: pmQuantile(roundIns[c.Name], 0.5),
			ClaimRounds: roundClaim[c.Name], InsRounds: roundIns[c.Name],
			ClaimP50US: pmQuantile(pooledClaim[c.Name], 0.5), ClaimP95US: pmQuantile(pooledClaim[c.Name], 0.95),
			InsP50US: pmQuantile(pooledIns[c.Name], 0.5), InsP95US: pmQuantile(pooledIns[c.Name], 0.95)})
	}
}

// ---------------------------------------------------------------------------------------
// Cold reconnect: a brand-new pool and backend, then ClaimRun with an empty statement cache.

func pmRunCold(ctx context.Context, t *testing.T, out *pmOut, dsn string, fx *pmFixture, obs *pmObserver, rawDir string, cands []pmCand, reps int) {
	for _, c := range cands {
		var connMs, firstMs, warmMs []float64
		for i := 0; i < reps; i++ {
			e := newPMEnv(t, out, dsn, c, fx, obs, rawDir)
			t0 := time.Now()
			e.openPool(ctx)
			t.Cleanup(e.closePool)
			conn, err := e.pool.Acquire(pmSkip(ctx))
			if err != nil {
				t.Fatalf("%s: cold acquire: %v", c.Name, err)
			}
			t.Cleanup(conn.Release) // runs before closePool; releasing a conn in a tx destroys it
			tConn := time.Since(t0)
			tx, err := conn.Begin(pmSkip(ctx))
			if err != nil {
				t.Fatalf("%s: cold begin: %v", c.Name, err)
			}
			q := store.New(tx)
			var first, warm time.Duration
			err = pmSavepoint(ctx, tx, func() error {
				p := fx.claimParams()
				t1 := time.Now()
				r, err := q.ClaimRun(ctx, p)
				first = time.Since(t1)
				return e.checkClaim(r, err)
			})
			if err == nil {
				err = pmSavepoint(ctx, tx, func() error {
					p := fx.claimParams()
					t1 := time.Now()
					r, err := q.ClaimRun(ctx, p)
					warm = time.Since(t1)
					return e.checkClaim(r, err)
				})
			}
			if err != nil {
				t.Fatalf("%s: cold claim: %v", c.Name, err)
			}
			if err := tx.Rollback(pmSkip(ctx)); err != nil {
				t.Fatalf("%s: cold rollback: %v", c.Name, err)
			}
			conn.Release()
			e.closePool()
			connMs = append(connMs, float64(tConn.Microseconds())/1e3)
			firstMs = append(firstMs, float64(first.Microseconds())/1e3)
			warmMs = append(warmMs, float64(warm.Microseconds())/1e3)
		}
		out.emit("cold_reconnect", map[string]any{"cand": c.Name, "reps": reps,
			"connect_ms_median": pmQuantile(connMs, 0.5), "first_claim_run_ms_median": pmQuantile(firstMs, 0.5),
			"warm_claim_run_ms_median":                 pmQuantile(warmMs, 0.5),
			"reconnect_plus_first_claim_run_ms_median": pmQuantile(addSlices(connMs, firstMs), 0.5),
			"connect_ms_all":                           connMs, "first_claim_run_ms_all": firstMs, "warm_claim_run_ms_all": warmMs})
	}
	e := newPMEnv(t, out, dsn, cands[0], fx, obs, rawDir)
	e.verifyFixture(ctx)
}

func addSlices(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] + b[i]
	}
	return out
}

// ---------------------------------------------------------------------------------------
// Supplemental lower-bound wide-read sample

type pmWideRec struct {
	Mode            string          `json:"plan_cache_mode"`
	Config          string          `json:"config"`
	Capacity        int             `json:"statement_cache_capacity"`
	QueryTexts      int             `json:"sqlc_query_texts"`
	ParseFailures   int             `json:"parse_failures"`
	ParseFailSample []string        `json:"parse_failure_sample,omitempty"`
	ExecutedOK      int             `json:"executed_ok_statements"`
	ReadOnlyBlocked int             `json:"blocked_by_read_only_transaction"`
	OtherErrors     int             `json:"other_execution_errors"`
	OtherErrSample  []string        `json:"other_error_sample,omitempty"`
	Runs            int             `json:"executions_attempted"`
	Retained        int             `json:"retained_statements"`
	Identities      int             `json:"identities_cumulative"`
	BytesPerStmt    float64         `json:"bytes_per_cached_statement_approx"`
	Baseline        pmSample        `json:"baseline"`
	Final           pmSample        `json:"final"`
	Sampler         pmSamplerResult `json:"sampler"`
	VmHWMKB         int64           `json:"vmhwm_kb_includes_shared"`
	Note            string          `json:"note"`
}

// pmRunWide executes every sqlc query text, once per PG parameter with NULL, twice each,
// on ONE held connection inside BEGIN READ ONLY with a SAVEPOINT per statement. The
// session setting (force_generic_plan) and the advisory-lock cleanup are deferred so they
// run even when the test fails.
func pmRunWide(ctx context.Context, t *testing.T, out *pmOut, dsn string, fx *pmFixture, obs *pmObserver, rawDir string,
	cand pmCand, forceGeneric bool, order []string) {
	mode := "auto"
	if forceGeneric {
		mode = "force_generic_plan"
	}
	cand.Name = "WIDE-" + cand.Name
	e := newPMEnv(t, out, dsn, cand, fx, obs, rawDir)
	e.openPool(ctx)
	defer e.closePool()
	conn, err := e.pool.Acquire(pmSkip(ctx))
	if err != nil {
		t.Fatalf("wide-read sample acquire: %v", err)
	}
	defer conn.Release()
	cleanup := func(sql string) {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = conn.Conn().Exec(pmSkip(cctx), sql, pgx.QueryExecModeSimpleProtocol)
	}
	simple := func(sql string) {
		t.Helper()
		if _, err := conn.Conn().Exec(pmSkip(ctx), sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("wide-read sample %q: %v", sql, err)
		}
	}
	defer cleanup("SELECT pg_advisory_unlock_all()")
	if forceGeneric {
		simple("SET plan_cache_mode = force_generic_plan")
		defer cleanup("RESET plan_cache_mode")
	}
	e.start = time.Now()
	base := e.sampleHeld(ctx, conn, "wide_baseline", 0)
	// Everything from the sampler start through the last observer use (sampleHeld "wide_final"
	// and the record assembly) runs inside pmWithSampler's body, so every exit, including a
	// t.Fatalf, cancels and joins the sampler before the deferred conn.Release and closePool
	// above and before the caller's clone closes the observer.
	pmWithSampler(func() *pmSampler { return e.startSampler(ctx) }, func(sampler *pmSampler) {
		rec := pmWideRec{Mode: mode, Config: cand.Desc, Capacity: cand.capacity(), QueryTexts: len(order), Baseline: base,
			Note: "NULL for every parameter inside BEGIN READ ONLY: reads build and cache their plans, writes are refused after planning; statements pgx saw fail are invalidated and deallocated, so retained counts only statements that executed. Lower-bound wide-read sample (writes are refused and never retained, NULL arguments plan small); not an upper bound and not the decision basis."}
		simple("BEGIN READ ONLY")
		defer cleanup("ROLLBACK")
		for _, name := range order {
			sql := pmSQLTexts()[name]
			simple("SAVEPOINT env")
			sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			sd, perr := conn.Conn().Prepare(pmSkip(sctx), "", sql)
			if perr != nil {
				var pe *pgconn.PgError
				if errors.As(perr, &pe) {
					rec.ParseFailures++
					if len(rec.ParseFailSample) < 10 {
						rec.ParseFailSample = append(rec.ParseFailSample, fmt.Sprintf("%s: %s %s", name, pe.Code, pe.Message))
					}
				} else {
					t.Fatalf("wide-read sample %s: prepare transport error: %v", name, perr)
				}
			} else {
				args := make([]any, len(sd.ParamOIDs))
				for rep := 0; rep < 2; rep++ {
					rec.Runs++
					rows, qerr := conn.Conn().Query(context.WithValue(sctx, pmViaQueryKey, true), sql, args...)
					if qerr == nil {
						for rows.Next() {
						}
						qerr = rows.Err()
						rows.Close()
					}
					if qerr == nil {
						if rep == 0 {
							rec.ExecutedOK++
						}
						continue
					}
					var pe *pgconn.PgError
					switch {
					case errors.As(qerr, &pe) && pe.Code == "25006":
						if rep == 0 {
							rec.ReadOnlyBlocked++
						}
					case errors.As(qerr, &pe):
						if rep == 0 {
							rec.OtherErrors++
							if len(rec.OtherErrSample) < 10 {
								rec.OtherErrSample = append(rec.OtherErrSample, fmt.Sprintf("%s: %s %s", name, pe.Code, pe.Message))
							}
						}
					default:
						cancel()
						t.Fatalf("wide-read sample %s: non-database error: %v", name, qerr)
					}
					// A failed statement aborted the transaction: recover to the savepoint.
					simple("ROLLBACK TO SAVEPOINT env")
				}
			}
			cancel()
			simple("ROLLBACK TO SAVEPOINT env")
			simple("RELEASE SAVEPOINT env")
		}
		simple("ROLLBACK")
		if rec.ExecutedOK+rec.ReadOnlyBlocked+rec.OtherErrors+rec.ParseFailures != len(order) {
			t.Fatalf("wide-read sample accounting: %d ok + %d read-only + %d other + %d parse != %d texts",
				rec.ExecutedOK, rec.ReadOnlyBlocked, rec.OtherErrors, rec.ParseFailures, len(order))
		}
		rec.Sampler = sampler.Stop()
		if rec.Sampler.Errors > 0 {
			t.Fatalf("wide-read sample sampler errors: %s", rec.Sampler.FirstError)
		}
		fin := e.sampleHeld(ctx, conn, "wide_final", 1)
		rec.Final = fin
		rec.Retained, rec.Identities = *fin.Retained, fin.Identities
		rec.VmHWMKB = fin.Mem.VmHWMKB
		if rec.Retained > 0 {
			rec.BytesPerStmt = float64(fin.Ctx.TotalBytes-base.Ctx.TotalBytes) / float64(rec.Retained)
		}
		out.emit("wide_read_sample", rec)
	})
}

// ---------------------------------------------------------------------------------------
// ClaimRun plan / execution / JIT split (phase "explain")

type pmExplainDoc struct {
	Plan struct {
		TotalCost float64 `json:"Total Cost"`
	} `json:"Plan"`
	PlanningTime  float64 `json:"Planning Time"`
	ExecutionTime float64 `json:"Execution Time"`
	JIT           *struct {
		Functions int `json:"Functions"`
		Timing    struct {
			Generation   float64 `json:"Generation"`
			Inlining     float64 `json:"Inlining"`
			Optimization float64 `json:"Optimization"`
			Emission     float64 `json:"Emission"`
			Total        float64 `json:"Total"`
		} `json:"Timing"`
	} `json:"JIT"`
}

type pmExplainExec struct {
	JIT        string  `json:"jit_clone_setting"`
	Rep        int     `json:"rep"`
	PID        uint32  `json:"pid"`
	Exec       int     `json:"execution_n"`
	WallMs     float64 `json:"client_wall_ms"`
	PlanningMs float64 `json:"planning_ms"`
	ExecMs     float64 `json:"execution_ms"`
	TotalCost  float64 `json:"plan_total_cost"`
	JITPresent bool    `json:"jit_block_present"`
	JITFuncs   int     `json:"jit_functions"`
	JITTotalMs float64 `json:"jit_timing_total_ms"`
	JITGenMs   float64 `json:"jit_generation_ms"`
	JITInlMs   float64 `json:"jit_inlining_ms"`
	JITOptMs   float64 `json:"jit_optimization_ms"`
	JITEmitMs  float64 `json:"jit_emission_ms"`
}

// pmRunExplain runs ClaimRun's exact SQL text, with the bindings the sqlc method sends,
// under EXPLAIN (ANALYZE, SUMMARY ON, FORMAT JSON) EXECUTE of a server-side PREPARE six times
// on one fresh connection of a fresh clone (a capture connection first runs ClaimRun on the
// same clone, so "fresh" applies to the measured connection only; execution 1 = first use of the backend, 2-6 =
// the same connection again; EXECUTE follows the same custom-then-generic plan cache logic
// as a pgx-prepared statement). Each execution is its own transaction, rolled back. The
// clone's jit follows lab.jit. The bindings are replayed on the simple protocol so each is
// a literal that EXECUTE coerces to the prepared parameter type.
func pmRunExplain(ctx context.Context, lab *pmLab, rep int) {
	t, out := lab.t, lab.out
	sqlText := pmSQLTexts()["ClaimRun"]
	if sqlText == "" {
		t.Fatalf("no sqlc text for ClaimRun")
	}
	lab.withClone(ctx, fmt.Sprintf("expl_%s_r%d", lab.jit, rep), func(_, dsn string, obs *pmObserver) {
		// 1. Capture ClaimRun's real bindings on a throwaway connection.
		var args []any
		ccfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("explain: parse config: %v", err)
		}
		ccfg.Tracer = pmArgTracer{got: &args}
		cc, err := pgx.ConnectConfig(ctx, ccfg)
		if err != nil {
			t.Fatalf("explain: capture connect: %v", err)
		}
		ctx0, err := cc.Begin(ctx)
		if err != nil {
			t.Fatalf("explain: capture begin: %v", err)
		}
		run, err := store.New(ctx0).ClaimRun(context.WithValue(ctx, pmCaptureKey, true), lab.fx.claimParams())
		if err != nil || run.ID != lab.fx.QueuedRun || run.Status != "claimed" {
			t.Fatalf("explain: capture ClaimRun run %s status %q err %v", run.ID, run.Status, err)
		}
		_ = ctx0.Rollback(ctx)
		_ = cc.Close(ctx)
		if len(args) == 0 {
			t.Fatalf("explain: captured no ClaimRun bindings")
		}

		// 2. Does pg_jit_available() itself map LLVM into a backend? (Why the harness never
		// runs it on a measured connection.)
		pc, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("explain: probe connect: %v", err)
		}
		ppid := pc.PgConn().PID()
		before, err := obs.llvmMaps(ctx, ppid)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var avail bool
		if err := pc.QueryRow(ctx, `SELECT pg_jit_available()`, pgx.QueryExecModeSimpleProtocol).Scan(&avail); err != nil {
			t.Fatalf("explain: pg_jit_available: %v", err)
		}
		after, err := obs.llvmMaps(ctx, ppid)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		_ = pc.Close(ctx)
		out.emit("jit_available_probe", map[string]any{"jit_clone_setting": lab.jit, "rep": rep, "pg_jit_available": avail,
			"llvm_mapped_before": len(before) > 0, "llvm_mapped_after": len(after) > 0, "llvm_paths_after": after})

		// 3. The measured connection.
		xc, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("explain: connect: %v", err)
		}
		defer func() { _ = xc.Close(ctx) }()
		pid := xc.PgConn().PID()
		var showJIT string
		if err := xc.QueryRow(ctx, `SHOW jit`, pgx.QueryExecModeSimpleProtocol).Scan(&showJIT); err != nil {
			t.Fatalf("explain: SHOW jit: %v", err)
		}
		if lab.jit == "off" && showJIT != "off" {
			t.Fatalf("explain: clone jit off but SHOW jit = %q", showJIT)
		}
		memBefore, err := obs.read(ctx, pid, nil)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		llvmBefore, err := obs.llvmMaps(ctx, pid)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		if _, err := xc.Exec(ctx, `PREPARE pmclaim AS `+sqlText, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("explain: PREPARE: %v", err)
		}
		ph := make([]string, len(args))
		for i := range ph {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		explain := `EXPLAIN (ANALYZE, SUMMARY ON, FORMAT JSON) EXECUTE pmclaim(` + strings.Join(ph, ",") + `)`
		qargs := append([]any{pgx.QueryExecModeSimpleProtocol}, args...)
		var execs []pmExplainExec
		for n := 1; n <= 6; n++ {
			tx, err := xc.Begin(ctx)
			if err != nil {
				t.Fatalf("explain: begin: %v", err)
			}
			t0 := time.Now()
			var raw []byte
			err = tx.QueryRow(ctx, explain, qargs...).Scan(&raw)
			wall := time.Since(t0)
			_ = tx.Rollback(ctx)
			if err != nil {
				t.Fatalf("explain: execution %d: %v", n, err)
			}
			var docs []pmExplainDoc
			if err := json.Unmarshal(raw, &docs); err != nil || len(docs) != 1 {
				t.Fatalf("explain: execution %d: unparseable EXPLAIN JSON (%d docs, err %v): %.200s", n, len(docs), err, raw)
			}
			d := docs[0]
			ex := pmExplainExec{JIT: lab.jit, Rep: rep, PID: pid, Exec: n, WallMs: float64(wall.Microseconds()) / 1e3,
				PlanningMs: d.PlanningTime, ExecMs: d.ExecutionTime, TotalCost: d.Plan.TotalCost}
			if d.JIT != nil {
				ex.JITPresent, ex.JITFuncs, ex.JITTotalMs = true, d.JIT.Functions, d.JIT.Timing.Total
				ex.JITGenMs, ex.JITInlMs, ex.JITOptMs, ex.JITEmitMs = d.JIT.Timing.Generation, d.JIT.Timing.Inlining, d.JIT.Timing.Optimization, d.JIT.Timing.Emission
			}
			if d.ExecutionTime <= 0 {
				t.Fatalf("explain: execution %d reports Execution Time %v", n, d.ExecutionTime)
			}
			execs = append(execs, ex)
			out.emit("explain_exec", ex)
		}
		var generic, custom int64
		if err := xc.QueryRow(ctx, `SELECT generic_plans, custom_plans FROM pg_prepared_statements WHERE name = 'pmclaim'`,
			pgx.QueryExecModeSimpleProtocol).Scan(&generic, &custom); err != nil {
			t.Fatalf("explain: pg_prepared_statements: %v", err)
		}
		memAfter, err := obs.read(ctx, pid, nil)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		llvmAfter, err := obs.llvmMaps(ctx, pid)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		out.emit("explain_run", map[string]any{"jit_clone_setting": lab.jit, "rep": rep, "pid": pid, "show_jit": showJIT,
			"generic_plans": generic, "custom_plans": custom, "executions": len(execs),
			"anonymous_kb_before": memBefore.AnonymousKB, "anonymous_kb_after": memAfter.AnonymousKB,
			"private_clean_kb_before": memBefore.PrivateCleanKB, "private_clean_kb_after": memAfter.PrivateCleanKB,
			"llvm_mapped_before": len(llvmBefore) > 0, "llvm_mapped_after": len(llvmAfter) > 0, "llvm_paths_after": llvmAfter})
	})
}

// ---------------------------------------------------------------------------------------
// Orchestration

// pmRequireThrowawayDB refuses to run against a database that lacks the throwaway marker
// seeded by e2e/test-database-marker.sql, so the harness (which seeds rows and reads /proc
// through pg_read_file) never touches a retained database.
func pmRequireThrowawayDB(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("marker check connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	if err := pmCheckMarker(ctx, c); err != nil {
		t.Fatalf("refusing to run: %v; initialise a throwaway database with e2e/test-database-marker.sql", err)
	}
}

// pmCheckMarker errors unless the connected database carries exactly one throwaway marker row.
func pmCheckMarker(ctx context.Context, c *pgx.Conn) error {
	var n int
	if err := c.QueryRow(ctx,
		`SELECT count(*)::int FROM uzi_test_harness.database_marker WHERE marker = 'recovery-reset-v1'`).Scan(&n); err != nil {
		return fmt.Errorf("uzi_test_harness.database_marker unreadable: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("uzi_test_harness.database_marker lacks recovery-reset-v1 (count %d)", n)
	}
	return nil
}

// pmLab owns the cloning of the seeded template. adminDSN is the maintenance database the
// marker was checked on; CREATE and DROP DATABASE run there on a superuser connection.
//
// Every invocation names its databases from its own random prefix (template prefix+"_tpl",
// clones prefix+"_"+label) and drops only databases whose CREATE it saw succeed, so it can
// never drop a database it did not create. There is no startup cleanup. After a crashed run,
// drop its leftovers by hand (never automatically), and only by that invocation's exact
// database_prefix from its meta record, once the invocation is confirmed stopped. The query
// below inventories every invocation's databases, active runs included; it is never a
// deletion list. If the prefix or stopped state is unknown, drop nothing.
//
//	SELECT datname FROM pg_database WHERE datname ~ '^pm_[0-9a-f]{32}_'
//
// The template is NOT untouched after seeding: the validation pool and discover mode still
// connect to it, and clones are per phase as described in the file header.
type pmLab struct {
	t        *testing.T
	out      *pmOut
	adminDSN string
	fx       *pmFixture
	rawDir   string
	// jit is "default" (clones keep the server's jit setting) or "off" (every clone gets
	// ALTER DATABASE ... SET jit = off before anything connects to it).
	jit string
	// prefix is "pm_" plus 32 hex chars, unique per invocation; templateName is the owned
	// template database name.
	prefix       string
	templateName string
	// exec runs one admin statement; every construction sets it (pmAdminExec, or a test seam).
	// It is a plain func value so a copy of the lab (el := *lab) stays safe.
	exec func(ctx context.Context, adminDSN, sql string) error
}

func (l *pmLab) run(ctx context.Context, sql string) error {
	return l.exec(ctx, l.adminDSN, sql)
}

// drop issues DROP DATABASE IF EXISTS ... WITH (FORCE) through the lab's exec seam.
func (l *pmLab) drop(name string) error {
	return pmDropDBWith(l.run, name)
}

// create runs a CREATE DATABASE statement for name. A nil return means this call owns the
// database. An ambiguous outcome (the statement may have been applied) preserves the database
// and returns an error naming the manual drop; a definite failure drops nothing.
func (l *pmLab) create(ctx context.Context, name, sql string) error {
	err := l.run(ctx, sql)
	created, ambiguous := pmCreateOutcome(err)
	switch {
	case created:
		return nil
	case ambiguous:
		return fmt.Errorf("creation outcome ambiguous; %s preserved, not dropped; if it exists and is unwanted run: DROP DATABASE %s WITH (FORCE): %w",
			name, pgx.Identifier{name}.Sanitize(), err)
	default:
		return fmt.Errorf("create database %s: %w", name, err)
	}
}

// pmIdentRune reports whether r is allowed in a database name suffix ([a-z0-9_]).
func pmIdentRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
}

// pmDBName joins prefix and suffix, lowercasing the suffix. It rejects characters outside
// [a-z0-9_] and any name over PostgreSQL's 63-byte identifier limit (never truncates).
func pmDBName(prefix, suffix string) (string, error) {
	suffix = strings.ToLower(suffix)
	for _, r := range suffix {
		if !pmIdentRune(r) {
			return "", fmt.Errorf("database name suffix %q has invalid character %q (allowed [a-z0-9_])", suffix, r)
		}
	}
	name := prefix + suffix
	if len(name) > 63 {
		return "", fmt.Errorf("database name %q is %d bytes, over the 63-byte limit", name, len(name))
	}
	return name, nil
}

// pmCreateOutcome decides ownership after a CREATE DATABASE attempt. nil means created. A
// connect- or marker-stage pmAdminErr means nothing ran. At exec stage (or for any error that is
// not a pmAdminErr), a PgError with unlocalized severity ERROR means the statement was rejected
// and nothing was created; anything else (FATAL, PANIC, a lost connection, a context error) is
// ambiguous: the server may have applied it.
func pmCreateOutcome(err error) (created, ambiguous bool) {
	if err == nil {
		return true, false
	}
	var ae *pmAdminErr
	if errors.As(err, &ae) && (ae.Stage == pmStageConnect || ae.Stage == pmStageMarker) {
		return false, false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.SeverityUnlocalized == "ERROR" {
		return false, false
	}
	return false, true
}

// newPMLab builds one invocation's lab: a fresh uuid prefix, the template name derived from it,
// and the marker-checked pmAdminExec. Every invocation calls it once, so no two invocations
// share a template or clone name.
func newPMLab(t *testing.T, out *pmOut, adminDSN, rawDir, jit string) (*pmLab, error) {
	prefix := "pm_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	tpl, err := pmDBName(prefix, "_tpl")
	if err != nil {
		return nil, err
	}
	return &pmLab{t: t, out: out, adminDSN: adminDSN, rawDir: rawDir, jit: jit,
		prefix: prefix, templateName: tpl, exec: pmAdminExec}, nil
}

// pmDSNForDB returns dsn with its database name replaced. Only URL-form DSNs are supported.
// A dbname or database query parameter is refused: pgx copies query parameters over the path
// database, which would silently point every "clone" connection back at the same database.
func pmDSNForDB(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("UZI_TEST_DATABASE_URL must be a postgres:// URL to derive per-clone DSNs (err %v)", err)
	}
	for k := range u.Query() {
		if lk := strings.ToLower(k); lk == "dbname" || lk == "database" {
			t.Fatalf("UZI_TEST_DATABASE_URL carries a %q query parameter, which overrides the path database and defeats per-clone isolation; put the database in the URL path", k)
		}
	}
	u.Path, u.RawPath = "/"+db, ""
	return u.String()
}

// Stages of pmAdminExec at which an error can arise.
const (
	pmStageConnect = "connect"
	pmStageMarker  = "marker"
	pmStageExec    = "exec"
)

// pmAdminErr is the error pmAdminExec returns, carrying the stage that failed so callers can
// tell "nothing ran" (connect, marker) from "the statement may have run" (exec).
type pmAdminErr struct {
	Stage string
	Err   error
}

func (e *pmAdminErr) Error() string { return "admin " + e.Stage + ": " + e.Err.Error() }
func (e *pmAdminErr) Unwrap() error { return e.Err }

// pmAdminExec runs one statement (simple protocol, so CREATE/DROP DATABASE are legal) on a
// short-lived superuser connection to the maintenance database. It rechecks the throwaway
// marker on that same connection first and runs nothing if the marker is missing.
func pmAdminExec(ctx context.Context, adminDSN, sql string) error {
	c, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return &pmAdminErr{Stage: pmStageConnect, Err: err}
	}
	defer func() { _ = c.Close(ctx) }()
	if err := pmCheckMarker(ctx, c); err != nil {
		return &pmAdminErr{Stage: pmStageMarker, Err: err}
	}
	if _, err := c.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		return &pmAdminErr{Stage: pmStageExec, Err: fmt.Errorf("%.80q: %w", sql, err)}
	}
	return nil
}

func pmDropDB(adminDSN, name string) error {
	return pmDropDBWith(func(ctx context.Context, sql string) error { return pmAdminExec(ctx, adminDSN, sql) }, name)
}

func pmDropDBWith(run func(ctx context.Context, sql string) error, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return run(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// withClone is clone with any error turned into t.Fatalf.
func (l *pmLab) withClone(ctx context.Context, label string, fn func(name, dsn string, obs *pmObserver)) {
	l.t.Helper()
	if err := l.clone(ctx, label, fn); err != nil {
		l.t.Fatalf("%v", err)
	}
}

// clone creates database <prefix>_<label> from the lab's template, hands fn its real name, a DSN
// plus an observer connection to the same clone, then closes the observer and drops the clone.
// The drop is registered only when this call's CREATE is known to have succeeded.
func (l *pmLab) clone(ctx context.Context, label string, fn func(name, dsn string, obs *pmObserver)) error {
	name, err := pmDBName(l.prefix, "_"+label)
	if err != nil {
		return fmt.Errorf("clone name: %w", err)
	}
	if err := l.create(ctx, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{l.templateName}.Sanitize()); err != nil {
		return err
	}
	defer func() {
		if err := l.drop(name); err != nil {
			l.t.Errorf("drop clone %s: %v", name, err)
		}
	}()
	if l.jit == "off" {
		if err := l.run(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" SET jit = off"); err != nil {
			return fmt.Errorf("disable jit on clone %s: %w", name, err)
		}
	}
	dsn := pmDSNForDB(l.t, l.adminDSN, name)
	oc, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("observer connect to clone %s: %w", name, err)
	}
	defer func() { _ = oc.Close(ctx) }()
	l.out.emit("clone", map[string]any{"database": name, "label": label, "created_at": time.Now().UTC(), "jit": l.jit})
	fn(name, dsn, &pmObserver{conn: oc})
	return nil
}

// pmPhaseSet parses UZI_POOL_MEMORY_PHASES, a comma list of memory,latency,cold,wide,stress,
// explain (default: all), so one run can be split into several go test invocations.
func pmPhaseSet(t *testing.T) map[string]bool {
	t.Helper()
	known := []string{"memory", "latency", "cold", "wide", "stress", "explain"}
	set := map[string]bool{}
	raw := strings.TrimSpace(os.Getenv("UZI_POOL_MEMORY_PHASES"))
	if raw == "" {
		for _, k := range known {
			set[k] = true
		}
		return set
	}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if !slices.Contains(known, p) {
			t.Fatalf("UZI_POOL_MEMORY_PHASES: unknown phase %q (known: %s)", p, strings.Join(known, ","))
		}
		set[p] = true
	}
	return set
}

func TestPoolBackendMemoryMeasurement(t *testing.T) {
	if os.Getenv("UZI_POOL_MEMORY_MEASURE") != "1" {
		t.Skip("UZI_POOL_MEMORY_MEASURE=1 not set: opt-in pool memory measurement harness")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; the measurement needs a throwaway superuser Postgres")
	}
	outPath := os.Getenv("UZI_POOL_MEMORY_OUT")
	if outPath == "" {
		t.Fatal("UZI_POOL_MEMORY_OUT must name the JSON-lines output file")
	}
	if _, err := pmSQLLoad(); err != nil {
		t.Fatalf("parse sqlc query texts: %v", err)
	}
	if len(pmBreadth) < pmMinIdentities {
		t.Fatalf("breadth table has %d entries, need >= %d", len(pmBreadth), pmMinIdentities)
	}
	seen := map[string]bool{}
	for _, en := range pmBreadth {
		if seen[en.Name] {
			t.Fatalf("duplicate breadth entry %s", en.Name)
		}
		seen[en.Name] = true
		if _, ok := pmSQLTexts()[en.Name]; !ok {
			t.Fatalf("breadth entry %s has no sqlc query text", en.Name)
		}
	}
	ctx := context.Background()
	pmRequireThrowawayDB(ctx, t, dsn)
	outPath = filepath.Clean(outPath)
	f, err := os.Create(outPath) //nolint:gosec // G304: path is the operator-supplied measurement output under opt-in env
	if err != nil {
		t.Fatalf("create output: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close output: %v", err)
		}
	}()
	out := &pmOut{f: f}
	rawDir := filepath.Dir(outPath)

	phases := pmPhaseSet(t)

	// Seed ONCE into the template database. Measurements below run on clones (shared per
	// phase as described in the header), but the validation pool just below and discover
	// mode still connect to the template. The marker was checked on the maintenance database above;
	// a marker there is the evidence that this instance is a throwaway.
	adminDSN := dsn
	lab, err := newPMLab(t, out, adminDSN, rawDir, pmJITSetting(t))
	if err != nil {
		t.Fatalf("template name: %v", err)
	}
	tplName := lab.templateName
	// The template and every clone are named from this invocation's own prefix, so no existing
	// database can collide with them and nothing is dropped unless this run created it.
	// TEMPLATE template0: a bare CREATE DATABASE copies template1, which fails (55006) while any
	// other session is connected to template1.
	if err := lab.create(ctx, tplName, "CREATE DATABASE "+pgx.Identifier{tplName}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		if err := lab.drop(tplName); err != nil {
			t.Errorf("drop template: %v", err)
		}
	})
	tdsn := pmDSNForDB(t, adminDSN, tplName)
	if err := store.Migrate(ctx, tdsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fx := pmSeed(ctx, t, tdsn)

	// Validate the fixture's read shapes once through a plain pool (before the first clone).
	vp, err := store.OpenPool(ctx, tdsn)
	if err != nil {
		t.Fatalf("open validation pool: %v", err)
	}
	vq := store.New(vp)
	page, err := vq.ListRunsForUser(ctx, store.ListRunsForUserParams{
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, UserID: fx.User})
	if err != nil || len(page) != pmWantPage {
		t.Fatalf("fixture: ListRunsForUser %d rows (want %d), err %v", len(page), pmWantPage, err)
	}
	for _, r := range page {
		fx.PageIDs = append(fx.PageIDs, r.Run.ID)
	}
	totals, err := vq.ListRunUsageTotalsForRuns(ctx, fx.PageIDs)
	if err != nil || len(totals) == 0 {
		t.Fatalf("fixture: ListRunUsageTotalsForRuns %d rows, err %v", len(totals), err)
	}
	fx.WantTotals = len(totals)
	su, err := vq.SelfUsage(ctx, fx.User)
	if err != nil || su.RunCount != pmWantRunCount {
		t.Fatalf("fixture: SelfUsage run_count %d (want %d), err %v", su.RunCount, pmWantRunCount, err)
	}
	vp.Close()

	if os.Getenv("UZI_POOL_MEMORY_DISCOVER") == "1" {
		pmDiscover(ctx, t, out, tdsn, fx)
		return
	}

	var ver, sb, maxConn, autovac, cgMax string
	mc, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("meta connect: %v", err)
	}
	for _, x := range []struct {
		sql string
		dst *string
	}{{"SHOW server_version", &ver}, {"SHOW shared_buffers", &sb}, {"SHOW max_connections", &maxConn}, {"SHOW autovacuum", &autovac}} {
		if err := mc.QueryRow(ctx, x.sql, pgx.QueryExecModeSimpleProtocol).Scan(x.dst); err != nil {
			t.Fatalf("%s: %v", x.sql, err)
		}
	}
	if err := mc.QueryRow(ctx, `SELECT pg_read_file('/sys/fs/cgroup/memory.max')`, pgx.QueryExecModeSimpleProtocol).Scan(&cgMax); err != nil {
		cgMax = "unavailable: " + err.Error()
	}
	_ = mc.Close(ctx)
	// Server-level JIT facts, read on a separate maintenance connection (never a measured
	// one): pg_jit_available() initialises the JIT provider, so it runs only here.
	var jitSrv, jitAbove, jitInlineAbove, jitOptAbove string
	var jitAvail bool
	jc, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("jit meta connect: %v", err)
	}
	for _, x := range []struct {
		sql string
		dst *string
	}{{"SHOW jit", &jitSrv}, {"SHOW jit_above_cost", &jitAbove}, {"SHOW jit_inline_above_cost", &jitInlineAbove}, {"SHOW jit_optimize_above_cost", &jitOptAbove}} {
		if err := jc.QueryRow(ctx, x.sql, pgx.QueryExecModeSimpleProtocol).Scan(x.dst); err != nil {
			t.Fatalf("%s: %v", x.sql, err)
		}
	}
	if err := jc.QueryRow(ctx, `SELECT pg_jit_available()`, pgx.QueryExecModeSimpleProtocol).Scan(&jitAvail); err != nil {
		t.Fatalf("pg_jit_available: %v", err)
	}
	_ = jc.Close(ctx)
	lab.fx = fx
	reps := pmEnvMinInt(t, "UZI_POOL_MEMORY_REPS", 2, 1)
	cands := pmCandidates()
	if len(cands) != pmLatencyRounds {
		t.Fatalf("%d candidates but pmLatencyRounds %d: latency rotation would not lead each candidate once", len(cands), pmLatencyRounds)
	}
	out.emit("meta", map[string]any{
		"started_at": time.Now().UTC(), "server_version": ver, "shared_buffers": sb, "max_connections": maxConn,
		"autovacuum": autovac, "phases": phases, "memory_reps": reps, "template_database": lab.templateName, "database_prefix": lab.prefix,
		"jit_requested": lab.jit, "server_show_jit": jitSrv, "server_pg_jit_available": jitAvail, "server_jit_above_cost": jitAbove,
		"server_jit_inline_above_cost": jitInlineAbove, "server_jit_optimize_above_cost": jitOptAbove,
		"cgroup_memory_max": strings.TrimSpace(cgMax), "go_version": runtime.Version(), "num_cpu": runtime.NumCPU(),
		"breadth_entries": len(pmBreadth), "sqlc_query_texts": len(pmSQLTexts()), "candidates": cands,
		"latency_n": pmLatencyN, "latency_rounds": pmLatencyRounds, "hot_reps_per_round": pmHotReps,
		"sampler_interval_target_ms": pmSampleInterval.Milliseconds(),
		"fixture": map[string]any{"page_rows": pmWantPage, "totals_rows": fx.WantTotals, "self_usage_run_count": pmWantRunCount,
			"run_messages": pmWantMessages, "base_messages": fx.BaseMessages},
	})

	// Phase 1: memory. Each candidate and repetition runs alone on its own fresh clone with
	// its own observer. Odd repetitions run in candidate order, even ones in reverse, so
	// position in the sequence cannot masquerade as a candidate effect.
	if phases["memory"] {
		for rep := 1; rep <= reps; rep++ {
			order := slices.Clone(cands)
			if rep%2 == 0 {
				slices.Reverse(order)
			}
			for _, c := range order {
				lab.withClone(ctx, fmt.Sprintf("mem_%s_r%d", c.Name, rep), func(name, cdsn string, obs *pmObserver) {
					e := newPMEnv(t, out, cdsn, c, fx, obs, rawDir)
					e.rep = rep
					e.dbName = name
					e.runMemory(ctx)
				})
			}
		}
	}
	// Phase 2: latency comparison (each round on a fresh clone), then the supplemental
	// stress variant.
	if phases["latency"] {
		pmRunLatency(ctx, lab, cands, "headline_k1", pmLatencyN, 1)
	}
	if phases["stress"] {
		pmRunLatency(ctx, lab, cands, "supplemental_stress", pmStressN, pmStressBreadthK)
	}
	// Phase 3: cold reconnect, on one fresh clone.
	if phases["cold"] {
		lab.withClone(ctx, "cold", func(_, cdsn string, obs *pmObserver) {
			pmRunCold(ctx, t, out, cdsn, fx, obs, rawDir, cands, 7)
		})
	}
	// Phase 4: supplemental lower-bound wide-read sample, each run on a fresh clone.
	if phases["wide"] {
		_, order, err := pmSQLConstants()
		if err != nil {
			t.Fatalf("enumerate sqlc constants: %v", err)
		}
		for _, c := range []pmCand{
			{Name: "cap4096", Desc: "statement cache 4096 (holds every sqlc statement)", StmtCap: 4096},
			{Name: "default512", Desc: "pgx default statement cache 512"},
		} {
			for _, generic := range []bool{false, true} {
				lab.withClone(ctx, fmt.Sprintf("wide_%s_%t", c.Name, generic), func(_, cdsn string, obs *pmObserver) {
					pmRunWide(ctx, t, out, cdsn, fx, obs, rawDir, c, generic, order)
				})
			}
		}
	}
	// Phase 5: ClaimRun plan/execution/JIT split, once with the server's jit and once with
	// jit off on the clone, regardless of UZI_POOL_MEMORY_JIT.
	if phases["explain"] {
		for _, mode := range []string{"default", "off"} {
			el := *lab
			el.jit = mode
			for rep := 1; rep <= 2; rep++ {
				pmRunExplain(ctx, &el, rep)
			}
		}
	}
	out.emit("done", map[string]any{"finished_at": time.Now().UTC()})
}

// pmDiscover runs each breadth entry once and records its observed outcome, without
// enforcing the pinned expectation; used to (re)pin the table.
func pmDiscover(ctx context.Context, t *testing.T, out *pmOut, dsn string, fx *pmFixture) {
	p, err := store.OpenPool(ctx, dsn, store.WithMaxConns(1))
	if err != nil {
		t.Fatalf("discover pool: %v", err)
	}
	defer p.Close()
	for _, en := range pmBreadth {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatalf("discover begin: %v", err)
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, qerr := en.Fn(cctx, store.New(tx), fx)
		cancel()
		got, cerr := pmClassify(res, qerr)
		rec := map[string]any{"name": en.Name, "outcome": got.String()}
		if cerr != nil {
			rec["outcome"], rec["error"] = "error", cerr.Error()
		}
		out.emit("discover", rec)
		_ = tx.Rollback(ctx)
	}
}
