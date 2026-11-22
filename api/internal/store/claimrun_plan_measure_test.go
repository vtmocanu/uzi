package store_test

// TestClaimRunPlanMeasurement is an OPT-IN measurement harness (issue #4602), not a regression
// test. It measures the per-call cost of ClaimRun's query plan when the claim queue is idle, and
// whether two candidate mitigations remove it:
//
//	C1  force a generic plan for the one ClaimRun statement (transaction-local set_config, restored
//	    on success or pgx.ErrNoRows, never restored on another error)
//	C2  skip ClaimRun while the owner has no queued, non-chat, ordinary-lane run (an EXISTS read)
//
// The candidate SQL is inline here and no production helper exists for it, so this file compiles
// against a base revision that has neither. It is skipped unless BOTH of these are set:
//
//	UZI_CLAIMRUN_PLAN_MEASURE=1
//	UZI_TEST_DATABASE_URL=<throwaway superuser DSN carrying the e2e/test-database-marker.sql marker>
//
// and writes JSON lines to the file named by UZI_CLAIMRUN_PLAN_OUT (required when enabled). The
// name deliberately does not end in LiveDB, so e2e/run-store-it.sh (-run 'LiveDB$') never selects
// it, and plain `go test ./...` skips it.
//
// Knobs:
//
//	UZI_CLAIMRUN_PLAN_STAGES    required. A comma list of P0,T1,T2,T3,T4,T5,R (the pre-registered
//	                            table; R is the fallback reserve), plus two diagnostics outside it:
//	                            F, a cheap profile-fixture feasibility check (one cold call per
//	                            profile and scenario, no timing claim), and S, the plan-choice
//	                            scale diagnostic (claimrun_plan_scale_test.go).
//	                            E is the end-to-end stage (claimrun_plan_service_test.go): the real
//	                            workersvc Service.Claim, run unchanged on a base revision and a fix
//	                            revision (UZI_CLAIMRUN_PLAN_E_LABEL names the run).
//	                            Stages always run in the order P0,F,S,T1,T2,T3,T4,T5,R,E.
//	UZI_CLAIMRUN_PLAN_WINNER    C1|C2|C1C2, required for T4, T5 and R.
//	UZI_CLAIMRUN_PLAN_RUNNERUP  C1|C2|C1C2, required for R, and different from the winner.
//	UZI_CLAIMRUN_PLAN_N         timed calls per cell. Default 100, minimum 50. P0 always uses 20.
//	UZI_CLAIMRUN_PLAN_ISOLATED  "1" adds the isolated-statement pass (default off). It runs on the
//	                            cell's clone after the segment phase, so it inherits that phase's
//	                            rolled-back-write residue and doubles the cell's ClaimRun executions;
//	                            its numbers are indicative only. The #4602 record ran with it on.
//	UZI_CLAIMRUN_PLAN_BACKLOG   queued claimable runs scenario (d) adds (default 300; 2000 made one
//	                            ClaimRun take about 1.3 s on the pilot host).
//	UZI_CLAIMRUN_PLAN_SCALES    stage S only: a comma list of completed-run history sizes
//	                            (default 1000,10000,50000,150000). Stage S is a deterministic
//	                            plan-choice diagnostic with no timed N-call loop; see runScale.
//	UZI_CLAIMRUN_PLAN_CLONE_ANALYZE  "0" skips the ANALYZE of runs/workers/cross_checks that
//	                            runs in each clone after its scenario mutation (default on; the
//	                            template itself is ANALYZEd, then autovacuum is disabled per table).
//
// Isolation. The schema and fixture are seeded once into a template database pm_<uuid>_tpl
// (pmLab: per-invocation names, the marker re-checked on every DDL connection, a CREATE whose
// outcome is ambiguous is preserved, and only databases this invocation created are dropped).
// Every cell runs on its own fresh clone. Autovacuum is disabled per table in the template, so a
// clone's dead tuples come only from its own scenario and calls.
//
// The ClaimRun segment (one call), in this order:
//
//	1  begin READ COMMITTED
//	2  GetWorkerForUpdate
//	3  [C2] SELECT EXISTS (...); false skips to the commit and counts as idle
//	4  SAVEPOINT
//	5  OptInRunFences
//	6  [C1] capture the prior plan_cache_mode and force a generic plan when it is auto
//	7  the sqlc ClaimRun
//	8  [C1] restore the prior mode on success AND on pgx.ErrNoRows (never on another error)
//	9  claimed: ROLLBACK TO + RELEASE; idle: ROLLBACK TO
//	10 commit
//
// The bindings are the real ones: one untimed Service.Claim (ClaimCrossCheck for the cross-check
// profile) on a separate capture clone, traced with pmArgTracer, then replayed through the sqlc
// method. A segment timing is a segment timing, not an end-to-end Service.Claim latency.
//
// Everything verified fails the run closed: an idle cell must get pgx.ErrNoRows, a claimable cell
// must get the queued run back with status claimed and the expected open custody hold (checked
// inside the savepoint, before ROLLBACK TO), the replayed arguments must equal the captured ones,
// the connection must keep one backend PID, the session plan_cache_mode must read the cell's mode
// after every phase, and nothing a call wrote may survive in the clone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ---------------------------------------------------------------------------------------
// Candidate SQL (inline, exactly as the fix would send it)

const (
	cpC2SQL        = `SELECT EXISTS (SELECT 1 FROM runs WHERE user_id = $1 AND status = 'queued' AND kind <> 'chat' AND NOT cross_check_lane)`
	cpC1SetSQL     = `SELECT s.prior, CASE WHEN s.prior = 'auto' THEN set_config('plan_cache_mode', 'force_generic_plan', true) END FROM (SELECT current_setting('plan_cache_mode') AS prior OFFSET 0) s`
	cpC1RestoreSQL = `SELECT set_config('plan_cache_mode', $1, true)`
)

const (
	cpWarmups      = 10
	cpPilotN       = 20
	cpMinN         = 50
	cpBacklogDef   = 300 // scenario (d): queued claimable runs on top of the seeded one (UZI_CLAIMRUN_PLAN_BACKLOG)
	cpMissingCap   = "cpm_missing_cap"
	cpT3Executions = 12
	cpMidUsers     = 5
	cpMidHistory   = 40
)

// ---------------------------------------------------------------------------------------
// Configuration

type cpCand struct {
	name   string
	c1, c2 bool
}

var (
	cpBase = cpCand{name: "base"}
	cpC1   = cpCand{name: "c1", c1: true}
	cpC2   = cpCand{name: "c2", c2: true}
	cpBoth = cpCand{name: "c1c2", c1: true, c2: true}
)

func cpParseCand(s string) (cpCand, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "C1":
		return cpC1, true
	case "C2":
		return cpC2, true
	case "C1C2":
		return cpBoth, true
	}
	return cpCand{}, false
}

type cpConfig struct {
	stages       []string
	n            int
	winner       cpCand
	runnerUp     cpCand
	isolated     bool
	cloneAnalyze bool
	backlog      int
	scales       []int
	out          string
}

var cpStageOrder = []string{"P0", "F", "S", "T1", "T2", "T3", "T4", "T5", "R", "E"}

func cpLoadConfig(t *testing.T) cpConfig {
	t.Helper()
	var cfg cpConfig
	raw := strings.TrimSpace(os.Getenv("UZI_CLAIMRUN_PLAN_STAGES"))
	if raw == "" {
		t.Fatal("UZI_CLAIMRUN_PLAN_STAGES is required (a comma list of P0,F,S,T1,T2,T3,T4,T5,R,E)")
	}
	want := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.ToUpper(strings.TrimSpace(s))
		if !slices.Contains(cpStageOrder, s) {
			t.Fatalf("UZI_CLAIMRUN_PLAN_STAGES: unknown stage %q (known: %s)", s, strings.Join(cpStageOrder, ","))
		}
		want[s] = true
	}
	for _, s := range cpStageOrder {
		if want[s] {
			cfg.stages = append(cfg.stages, s)
		}
	}
	cfg.n = pmEnvMinInt(t, "UZI_CLAIMRUN_PLAN_N", 100, 1)
	for _, s := range cfg.stages {
		if s != "P0" && s != "F" && s != "S" && cfg.n < cpMinN {
			t.Fatalf("UZI_CLAIMRUN_PLAN_N=%d: stage %s needs at least %d timed calls (only P0 may use fewer)", cfg.n, s, cpMinN)
		}
	}
	needWinner := want["T4"] || want["T5"] || want["R"]
	if needWinner {
		w, ok := cpParseCand(os.Getenv("UZI_CLAIMRUN_PLAN_WINNER"))
		if !ok {
			t.Fatal("UZI_CLAIMRUN_PLAN_WINNER (C1|C2|C1C2) is required for T4, T5 and R")
		}
		cfg.winner = w
	}
	if want["R"] {
		r, ok := cpParseCand(os.Getenv("UZI_CLAIMRUN_PLAN_RUNNERUP"))
		if !ok {
			t.Fatal("UZI_CLAIMRUN_PLAN_RUNNERUP (C1|C2|C1C2) is required for R")
		}
		if r.name == cfg.winner.name {
			t.Fatalf("UZI_CLAIMRUN_PLAN_RUNNERUP must differ from the winner (%s)", r.name)
		}
		cfg.runnerUp = r
	}
	cfg.isolated = strings.TrimSpace(os.Getenv("UZI_CLAIMRUN_PLAN_ISOLATED")) == "1"
	cfg.cloneAnalyze = strings.TrimSpace(os.Getenv("UZI_CLAIMRUN_PLAN_CLONE_ANALYZE")) != "0"
	cfg.backlog = pmEnvMinInt(t, "UZI_CLAIMRUN_PLAN_BACKLOG", cpBacklogDef, 1)
	cfg.scales = cpParseScales(t)
	cfg.out = os.Getenv("UZI_CLAIMRUN_PLAN_OUT")
	if cfg.out == "" {
		t.Fatal("UZI_CLAIMRUN_PLAN_OUT must name the JSON-lines output file")
	}
	return cfg
}

// ---------------------------------------------------------------------------------------
// Tracer: round trips, prepares, ClaimRun executions, ClaimRun argument capture

type cpTracer struct {
	claimSQL   string
	starts     atomic.Int64 // statement starts == round trips for a warm statement
	prepares   atomic.Int64 // extra round trips a cold statement pays
	claimExecs atomic.Int64
	capMu      sync.Mutex
	captured   *[]any // non-nil: record the next ClaimRun's arguments (through pmArgTracer)
}

func (tr *cpTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if ctx.Value(pmSkipKey) != nil {
		return ctx
	}
	tr.starts.Add(1)
	if d.SQL == tr.claimSQL {
		tr.claimExecs.Add(1)
		tr.capMu.Lock()
		if tr.captured != nil {
			pmArgTracer{got: tr.captured}.TraceQueryStart(context.WithValue(ctx, pmCaptureKey, true), conn, d)
		}
		tr.capMu.Unlock()
	}
	return ctx
}

func (*cpTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *cpTracer) TracePrepareStart(ctx context.Context, _ *pgx.Conn, _ pgx.TracePrepareStartData) context.Context {
	if ctx.Value(pmSkipKey) == nil {
		tr.prepares.Add(1)
	}
	return ctx
}

func (*cpTracer) TracePrepareEnd(context.Context, *pgx.Conn, pgx.TracePrepareEndData) {}

func (tr *cpTracer) setCapture(dst *[]any) {
	tr.capMu.Lock()
	tr.captured = dst
	tr.capMu.Unlock()
}

// ---------------------------------------------------------------------------------------
// Fixture overlay

// cpFx holds the ids the idle-fleet overlay added on top of pmSeed.
type cpFx struct {
	Fresh, FreshConn, FreshRepo, FreshWorker, FreshQueued, FreshOldRun uuid.UUID
	EphWorker, CCWorker                                                uuid.UUID
	Peers                                                              []uuid.UUID
	Mid                                                                []uuid.UUID
	OnlineWorkers, Users                                               int
}

// cpOverlay adds the idle-fleet shape to the pmSeed template: 8 fleet users (the heavy-history
// pmSeed user, the pmSeed second user, five mid-history users and one fresh user), 12 online
// workers, completed history for every user but the fresh one, then the per-profile claimants.
func cpOverlay(ctx context.Context, t *testing.T, dsn string, f *pmFixture) *cpFx {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("overlay connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	cx := &cpFx{
		Fresh: uuid.New(), FreshConn: uuid.New(), FreshRepo: uuid.New(), FreshWorker: uuid.New(),
		FreshQueued: uuid.New(), FreshOldRun: uuid.New(), EphWorker: uuid.New(), CCWorker: uuid.New(),
	}
	exec := func(sql string, args ...any) { pmExec(ctx, t, c, sql, args...) }
	addUser := func(id uuid.UUID, tag string) {
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, fmt.Sprintf("cp-%s-%s@e2e", tag, id))
		cx.Users++
	}
	addWorker := func(id, user uuid.UUID, name string, caps []string) {
		exec(`INSERT INTO workers (id, user_id, name, token_hash, status, online_since, last_heartbeat_at, max_concurrent_runs, protocol_capabilities)
		      VALUES ($1, $2, $3, $4, 'online', now(), now(), 4, $5)`, id, user, name, []byte(id.String()), caps)
		cx.OnlineWorkers++
	}
	history := func(user, repo uuid.UUID, n int) {
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		      SELECT gen_random_uuid(), $1, $2, g, 't', 'd', 'completed', 'issue', now() - make_interval(mins => g + 5)
		      FROM generate_series(1, $3::int) g`, user, repo, n)
	}
	recovery := []string{"recovery_archive_v1", "repo_agent_source_v1"}

	// The two pmSeed users and pmSeed's worker are fleet members already.
	cx.Users += 2
	cx.OnlineWorkers++ // pmSeed's worker
	for i := 1; i <= 2; i++ {
		id := uuid.New()
		addWorker(id, f.User, fmt.Sprintf("cp-peer-%d", i), recovery)
		cx.Peers = append(cx.Peers, id)
	}
	addWorker(uuid.New(), f.OtherUser, "cp-other-worker", recovery)
	for i := 1; i <= cpMidUsers; i++ {
		id := uuid.New()
		addUser(id, fmt.Sprintf("mid%d", i))
		history(id, f.Repo, cpMidHistory)
		addWorker(uuid.New(), id, fmt.Sprintf("cp-mid-worker-%d", i), recovery)
		cx.Mid = append(cx.Mid, id)
	}

	// The fresh user: its own forge connection and repository, one completed run (the one the
	// ephemeral claimant was bound to) and one claimable queued run for the same issue, so the
	// ephemeral lease's repository and branch identity admits it.
	addUser(cx.Fresh, "fresh")
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot-fresh', 2, $3)`, cx.FreshConn, cx.Fresh, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 2, 'g/pool-memory-fresh', 'https://forge.e2e/g/pool-memory-fresh', 'main', true)`, cx.FreshRepo, cx.FreshConn)
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
	      VALUES ($1, $2, $3, 7, 'old', 'd', 'completed', 'issue')`, cx.FreshOldRun, cx.Fresh, cx.FreshRepo)
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
	      VALUES ($1, $2, $3, 7, 'queued', 'd', 'queued', 'issue')`, cx.FreshQueued, cx.Fresh, cx.FreshRepo)
	exec(`UPDATE users SET weekly_reserve_pct = 90, weekly_reserve_pause_running = false WHERE id = $1`, cx.Fresh)
	addWorker(cx.FreshWorker, cx.Fresh, "cp-fresh-worker", []string{"repo_agent_source_v1"})

	// P4's ephemeral claimant: bound to the fresh user's old run, with a live lease on that run's
	// repository and branch (agent/issue-7), and no open custody hold.
	exec(`INSERT INTO workers (id, user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral,
	                           ephemeral_run_id, status, online_since, last_heartbeat_at, protocol_capabilities,
	                           snapshot_register_nonce, snapshot_epoch, max_concurrent_runs,
	                           lease_since, lease_repo_id, lease_branch)
	      VALUES ($1, $2, 'cp-ephemeral', $3, 'base', 'hosted', 'm', false, true, $4, 'online', now(), now(), $5,
	              'nonce-A', 0, 1, now(), $6, 'agent/issue-7')`,
		cx.EphWorker, cx.Fresh, []byte(cx.EphWorker.String()), cx.FreshOldRun, recovery, cx.FreshRepo)
	cx.OnlineWorkers++
	exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, cx.FreshOldRun, cx.EphWorker)

	// P3's claimant: a dedicated cross-check lane worker of the heavy user.
	addWorker(cx.CCWorker, f.User, "cp-cross-check", append(slices.Clone(recovery),
		"cross_check_v1", "cross_check_lane_v1", "cross_check_codex_lead_v1"))
	exec(`UPDATE workers SET max_cross_check_slots = 2 WHERE id = $1`, cx.CCWorker)

	// Queued runs are old enough to bypass the fleet-aware spread, as a long-idle queue's rows are.
	exec(`UPDATE runs SET created_at = now() - interval '2 hours', updated_at = now() - interval '2 hours' WHERE status = 'queued'`)

	exec(`ANALYZE`)
	// Autovacuum off per table, so a clone's dead tuples come only from its own work.
	exec(`DO $$
	      DECLARE r record;
	      BEGIN
	        FOR r IN SELECT format('%I.%I', n.nspname, c.relname) AS rel
	                 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	                 WHERE c.relkind = 'r' AND n.nspname = 'public'
	        LOOP
	          EXECUTE 'ALTER TABLE ' || r.rel || ' SET (autovacuum_enabled = false)';
	        END LOOP;
	      END $$`)
	return cx
}

// ---------------------------------------------------------------------------------------
// Profiles and bindings

type cpProfile struct {
	name       string
	desc       string
	user       uuid.UUID
	worker     uuid.UUID
	queued     uuid.UUID // the seeded claimable run for scenario (c); uuid.Nil: created per clone
	laneWant   string    // the lane the real Service call is expected to send
	laneSet    *string   // an explicit override applied after capture (recorded)
	custody    int32
	reserve    bool
	recovery   bool
	crossCheck bool
	ephemeral  bool
	expectHold bool // (c)/(d) open custody holds expected for the claimed run
}

func cpMakeProfiles(f *pmFixture, cx *cpFx) map[string]*cpProfile {
	empty := ""
	return map[string]*cpProfile{
		"p1": {name: "p1", desc: "lane '', reserve off, custody 8, recovery-capable, heavy-history user",
			user: f.User, worker: f.Worker, queued: f.QueuedRun, laneWant: "run", laneSet: &empty,
			custody: 8, recovery: true, expectHold: true},
		"p2": {name: "p2", desc: "lane run, reserve on, custody 1, not recovery-capable, fresh user",
			user: cx.Fresh, worker: cx.FreshWorker, queued: cx.FreshQueued, laneWant: "run",
			custody: 1, reserve: true, recovery: false, expectHold: false},
		"p3": {name: "p3", desc: "lane cross_check (plan-stage child), reserve off, custody 0, recovery-capable, heavy-history user",
			user: f.User, worker: cx.CCWorker, laneWant: "cross_check", crossCheck: true,
			custody: 0, recovery: true, expectHold: false},
		"p4": {name: "p4", desc: "lane run, reserve on, custody 8, ephemeral worker with a live lease, fresh user",
			user: cx.Fresh, worker: cx.EphWorker, queued: cx.FreshQueued, laneWant: "run",
			custody: 8, reserve: true, recovery: true, ephemeral: true, expectHold: true},
	}
}

type cpCustody int32

func (c cpCustody) CustodyHoldLimit(context.Context) (int32, error) { return int32(c), nil }

type cpEphSettings struct{ lease time.Duration }

func (cpEphSettings) EphemeralWorkersEnabled(context.Context) (bool, error) { return true, nil }
func (c cpEphSettings) EphemeralLease(context.Context) (time.Duration, error) {
	return c.lease, nil
}

// cpBinding is one profile's ClaimRun parameters, captured from a real Service call.
type cpBinding struct {
	params store.ClaimRunParams
	args   []any // exactly the arguments the capture call sent (before any recorded override)
}

func cpParamsFromArgs(args []any) (store.ClaimRunParams, error) {
	var p store.ClaimRunParams
	v := reflect.ValueOf(&p).Elem()
	if v.NumField() != len(args) {
		return p, fmt.Errorf("ClaimRunParams has %d fields but the capture holds %d arguments", v.NumField(), len(args))
	}
	for i, a := range args {
		if a == nil {
			continue
		}
		av := reflect.ValueOf(a)
		if !av.Type().AssignableTo(v.Field(i).Type()) {
			return p, fmt.Errorf("argument %d (%s) is %s, field is %s", i, v.Type().Field(i).Name, av.Type(), v.Field(i).Type())
		}
		v.Field(i).Set(av)
	}
	return p, nil
}

func cpArgsOf(p store.ClaimRunParams) []any {
	v := reflect.ValueOf(p)
	out := make([]any, v.NumField())
	for i := range out {
		out[i] = v.Field(i).Interface()
	}
	return out
}

// ---------------------------------------------------------------------------------------
// Environment

type cpEnv struct {
	t        *testing.T
	out      *pmOut
	lab      *pmLab
	fx       *pmFixture
	cx       *cpFx
	cfg      cpConfig
	claimSQL string
	profiles map[string]*cpProfile
	bind     map[string]*cpBinding
	results  []cpCellRes
}

type cpCell struct {
	Stage string
	Cand  cpCand
	Mode  string
	Scen  string
	Prof  string
	Rep   int
	N     int
	Warm  int
	Label string
	// T3 marks a server-split cell (EXPLAIN ANALYZE executions) rather than a timed segment cell.
	T3 bool
}

var cpModeShort = map[string]string{"auto": "auto", "force_generic_plan": "gen", "force_custom_plan": "cus"}

func cpLabel(stage string, cand cpCand, scen, tag string, rep int) string {
	return strings.ToLower(fmt.Sprintf("%s_%s_%s_%s_r%d", stage, cand.name, scen, tag, rep))
}

func cpRotate[T any](xs []T, k int) []T {
	n := len(xs)
	out := make([]T, n)
	for i := range xs {
		out[i] = xs[((i+k)%n+n)%n]
	}
	return out
}

func (e *cpEnv) binding(ctx context.Context, p *cpProfile) *cpBinding {
	if b, ok := e.bind[p.name]; ok {
		return b
	}
	t := e.t
	var args []any
	e.lab.withClone(ctx, "cap_"+p.name, func(_, dsn string, obs *pmObserver) {
		// No binding reads this seeded run's columns (it has no reserve exemption, so
		// ReserveOverrideEnded stays empty). Capture with exactly one queued run this
		// worker can never claim (a required capability it lacks): the owner's queue is non-empty,
		// so the #4602 idle precheck (HasQueuedRunForUser) passes on a fixed tree and ClaimRun
		// executes authoritatively on base and fix alike, yet claims nothing.
		if err := obs.exec(ctx, `DELETE FROM runs WHERE user_id = $1 AND status = 'queued'`, p.user); err != nil {
			t.Fatalf("capture %s: idle the queue: %v", p.name, err)
		}
		capRepo := e.fx.Repo
		if p.user == e.cx.Fresh {
			capRepo = e.cx.FreshRepo
		}
		if err := obs.exec(ctx, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, required_capabilities, created_at, updated_at)
		    VALUES ($1, $2, $3, 300000, 'q', 'd', 'queued', 'issue', $4, now() - interval '2 hours', now() - interval '2 hours')`,
			uuid.New(), p.user, capRepo, []string{cpMissingCap}); err != nil {
			t.Fatalf("capture %s: seed the ineligible queued run: %v", p.name, err)
		}
		cpRefresh(ctx, t, obs)
		tr := &cpTracer{claimSQL: e.claimSQL}
		tr.setCapture(&args)
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("capture %s: parse: %v", p.name, err)
		}
		cfg.MaxConns = 1
		cfg.ConnConfig.Tracer = tr
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("capture %s: pool: %v", p.name, err)
		}
		defer pool.Close()
		q := store.New(pool)
		// Production values (config.go defaults) for every window the claim reads; the box and
		// vault stay nil because an idle claim never opens a credential.
		svc := workersvc.New(q, nil, workersvc.Params{
			CustodyHoldLimit:        cpCustody(p.custody),
			WorkerHeartbeatStale:    45 * time.Second,
			WorkerHeartbeatInterval: 15 * time.Second,
			WorkerAffinityGrace:     2 * time.Minute,
			WorkerAffinityCeiling:   2 * time.Hour,
			WorkerStaleRequeueGrace: 10 * time.Minute,
			WorkerSpreadGrace:       9 * time.Second,
			WorkerBackgroundGrace:   15 * time.Minute,
		})
		svc.SetTxBeginner(pool)
		if p.ephemeral {
			svc.SetEphemeralSettings(cpEphSettings{lease: time.Hour})
		}
		wkr, err := q.GetWorkerByID(ctx, p.worker)
		if err != nil {
			t.Fatalf("capture %s: load worker: %v", p.name, err)
		}
		var payload *workersvc.ClaimPayload
		if p.crossCheck {
			payload, err = svc.ClaimCrossCheck(ctx, wkr, nil)
		} else {
			payload, err = svc.Claim(ctx, wkr, nil)
		}
		if err != nil || payload != nil {
			t.Fatalf("capture %s: Service claim with only an ineligible queued run returned payload=%v err=%v", p.name, payload != nil, err)
		}
		if n := tr.claimExecs.Load(); n != 1 {
			t.Fatalf("capture %s: expected exactly one ClaimRun execution, saw %d", p.name, n)
		}
	})
	if len(args) == 0 {
		t.Fatalf("capture %s: no ClaimRun bindings captured", p.name)
	}
	params, err := cpParamsFromArgs(args)
	if err != nil {
		t.Fatalf("capture %s: %v", p.name, err)
	}
	if params.CustodyHoldLimit != p.custody || params.RecoveryCapable != p.recovery || params.ReserveOn != p.reserve ||
		params.IsEphemeral != p.ephemeral || params.Lane != p.laneWant || params.WorkerID.Bytes != p.worker || params.UserID != p.user {
		t.Fatalf("capture %s: the real claim sent lane=%q custody=%d recovery=%v reserve=%v ephemeral=%v worker=%s user=%s, profile wants lane=%q custody=%d recovery=%v reserve=%v ephemeral=%v",
			p.name, params.Lane, params.CustodyHoldLimit, params.RecoveryCapable, params.ReserveOn, params.IsEphemeral,
			uuid.UUID(params.WorkerID.Bytes), params.UserID, p.laneWant, p.custody, p.recovery, p.reserve, p.ephemeral)
	}
	if p.ephemeral && (!params.LeaseSince.Valid || !params.LeaseRepoID.Valid || !params.LeaseBranch.Valid || !params.EphemeralLease.Valid || !params.LeaseAt.Valid) {
		t.Fatalf("capture %s: ephemeral profile captured without its lease parameters: %+v", p.name, params)
	}
	b := &cpBinding{params: params, args: args}
	if p.laneSet != nil {
		b.params.Lane = *p.laneSet
	}
	e.bind[p.name] = b
	e.out.emit("capture", map[string]any{"profile": p.name, "desc": p.desc, "captured_lane": params.Lane, "lane_override": p.laneSet,
		"args": len(args), "custody_hold_limit": params.CustodyHoldLimit, "recovery_capable": params.RecoveryCapable,
		"reserve_on": params.ReserveOn, "is_ephemeral": params.IsEphemeral, "lease_valid": params.EphemeralLease.Valid,
		"claim_params_via": "real workersvc Service claim, traced with pmArgTracer"})
	return b
}

// cpRefresh refreshes every fixture heartbeat (and the live ephemeral lease), as the hot path of
// pool_memory_measure_test.go's verifyFixture does, so a clone's workers are online at its start.
func cpRefresh(ctx context.Context, t *testing.T, obs *pmObserver) {
	t.Helper()
	if err := obs.exec(ctx, `UPDATE workers SET last_heartbeat_at = now() WHERE status = 'online'`); err != nil {
		t.Fatalf("refresh heartbeats: %v", err)
	}
	if err := obs.exec(ctx, `UPDATE workers SET lease_since = now() WHERE ephemeral AND lease_since IS NOT NULL`); err != nil {
		t.Fatalf("refresh lease: %v", err)
	}
}

// ---------------------------------------------------------------------------------------
// Per-clone scenario setup, warming, statistics

type cpRunner struct {
	env       *cpEnv
	cell      cpCell
	prof      *cpProfile
	bind      *cpBinding
	params    store.ClaimRunParams
	dsn       string
	obs       *pmObserver
	expectRun uuid.UUID // scenario (c): the run the claim must return
	locker    *pgx.Conn
	lockerTx  pgx.Tx
}

var cpWarmTables = []string{"runs", "workers", "users", "recovery_custody_holds", "forge_origin_cooldowns", "cross_checks",
	"authorized_forge_origins", "repos", "agent_templates", "agent_template_allocations", "run_publication_resume",
	"run_milestone_reconciliations", "worker_active_runs", "semantic_planning", "user_cross_check_pins", "run_user_inputs"}

func (r *cpRunner) prepare(ctx context.Context) {
	t, obs, p := r.env.t, r.obs, r.prof
	cpRefresh(ctx, t, obs)
	ex := func(sql string, args ...any) {
		if err := obs.exec(ctx, sql, args...); err != nil {
			t.Fatalf("scenario %s/%s: %.80q: %v", r.cell.Prof, r.cell.Scen, sql, err)
		}
	}
	insertQueued := func(id uuid.UUID, iid int64, caps []string) {
		repo := r.env.fx.Repo
		if p.user == r.env.cx.Fresh {
			repo = r.env.cx.FreshRepo
		}
		ex(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, required_capabilities, created_at, updated_at)
		    VALUES ($1, $2, $3, $4, 'q', 'd', 'queued', 'issue', $5, now() - interval '2 hours', now() - interval '2 hours')`,
			id, p.user, repo, iid, caps)
	}
	switch r.cell.Scen {
	case "a":
		ex(`DELETE FROM runs WHERE user_id = $1 AND status = 'queued'`, p.user)
	case "b":
		ex(`DELETE FROM runs WHERE user_id = $1 AND status = 'queued'`, p.user)
		insertQueued(uuid.New(), 200000, []string{cpMissingCap})
	case "c", "d", "e":
		if p.crossCheck {
			if r.cell.Scen != "c" {
				t.Fatalf("scenario %s is not defined for the cross-check profile", r.cell.Scen)
			}
			lead, child := r.env.fx.Run, uuid.New()
			ex(`INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, status, report_only, budget_wall_seconds, issue_title, issue_description)
			    VALUES ($1, $2, $3, 'cross_check', $4, 'claude', 'queued', true, 1800, 'plan', 'candidate')`,
				child, p.user, r.env.fx.Repo, lead)
			ex(`INSERT INTO cross_checks (lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
			        base_commit, candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled, automatic_revision_limit)
			    VALUES ($1, $2, 'plan', 1, 3, 'plan', '[]', 's', repeat('a', 40), $3, 'claude', now() + interval '1 hour', true, 2)`,
				lead, child, []byte("digest"))
			r.expectRun = child
		} else {
			r.expectRun = p.queued
		}
		if r.cell.Scen == "d" {
			repo := r.env.fx.Repo
			if p.user == r.env.cx.Fresh {
				repo = r.env.cx.FreshRepo
			}
			ex(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, updated_at)
			    SELECT gen_random_uuid(), $1, $2, 100000 + g, 'q', 'd', 'queued', 'issue',
			           now() - interval '2 hours' + make_interval(secs => g), now() - interval '2 hours'
			    FROM generate_series(1, $3::int) g`, p.user, repo, r.env.cfg.backlog)
		}
		if r.cell.Scen == "e" {
			lc, err := pgx.Connect(ctx, r.dsn)
			if err != nil {
				t.Fatalf("scenario e: locker connect: %v", err)
			}
			r.locker = lc
			tx, err := lc.Begin(ctx)
			if err != nil {
				t.Fatalf("scenario e: locker begin: %v", err)
			}
			r.lockerTx = tx
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, r.expectRun).Scan(&id); err != nil {
				t.Fatalf("scenario e: lock the claimable run: %v", err)
			}
		}
	default:
		t.Fatalf("unknown scenario %q", r.cell.Scen)
	}
	if r.env.cfg.cloneAnalyze {
		ex(`ANALYZE runs, workers, cross_checks`)
	}
	// The matched buffer warm-up: the same reads for every cell, before any measured connection.
	for _, tbl := range cpWarmTables {
		var n int64
		if err := obs.queryRow(ctx, `SELECT count(*) FROM `+tbl, nil, &n); err != nil {
			t.Fatalf("warm read %s: %v", tbl, err)
		}
	}
	var n int64
	if err := obs.queryRow(ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND status = 'queued'`, []any{p.user}, &n); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	r.params = r.bind.params
}

func (r *cpRunner) cleanup(ctx context.Context) {
	if r.lockerTx != nil {
		_ = r.lockerTx.Rollback(ctx)
	}
	if r.locker != nil {
		_ = r.locker.Close(ctx)
	}
}

type cpTable struct {
	Rel             string     `json:"rel"`
	Live            int64      `json:"n_live_tup"`
	Dead            int64      `json:"n_dead_tup"`
	Ins             int64      `json:"n_tup_ins"`
	Upd             int64      `json:"n_tup_upd"`
	Del             int64      `json:"n_tup_del"`
	SizeBytes       int64      `json:"relation_size_bytes"`
	LastAnalyze     *time.Time `json:"last_analyze"`
	LastAutoAnalyze *time.Time `json:"last_autoanalyze"`
}

type cpStats struct {
	Tables         []cpTable `json:"tables"`
	IdxSizeBytes   int64     `json:"idx_runs_claimable_size_bytes"`
	IdxScans       int64     `json:"idx_runs_claimable_idx_scan"`
	TotalDeadTuple int64     `json:"total_dead_tup"`
}

var cpStatTables = []string{"runs", "workers", "recovery_custody_holds", "run_user_inputs", "forge_origin_cooldowns", "cross_checks"}

func cpReadStats(ctx context.Context, t *testing.T, obs *pmObserver) cpStats {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	c := obs.conn
	// Push this backend's pending statistics, then read.
	if _, err := c.Exec(ctx, `SELECT pg_stat_force_next_flush()`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("stats flush: %v", err)
	}
	if _, err := c.Exec(ctx, `SELECT 1`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("stats flush: %v", err)
	}
	rows, err := c.Query(ctx, `SELECT c.relname, COALESCE(s.n_live_tup, 0), COALESCE(s.n_dead_tup, 0), COALESCE(s.n_tup_ins, 0),
	        COALESCE(s.n_tup_upd, 0), COALESCE(s.n_tup_del, 0), pg_relation_size(c.oid), s.last_analyze, s.last_autoanalyze
	    FROM pg_class c LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
	    WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND c.relname = ANY($1) ORDER BY c.relname`, cpStatTables)
	if err != nil {
		t.Fatalf("read table stats: %v", err)
	}
	defer rows.Close()
	var st cpStats
	for rows.Next() {
		var tb cpTable
		var la, laa pgtype.Timestamptz
		if err := rows.Scan(&tb.Rel, &tb.Live, &tb.Dead, &tb.Ins, &tb.Upd, &tb.Del, &tb.SizeBytes, &la, &laa); err != nil {
			t.Fatalf("scan table stats: %v", err)
		}
		if la.Valid {
			v := la.Time
			tb.LastAnalyze = &v
		}
		if laa.Valid {
			v := laa.Time
			tb.LastAutoAnalyze = &v
		}
		st.TotalDeadTuple += tb.Dead
		st.Tables = append(st.Tables, tb)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read table stats: %v", err)
	}
	if err := c.QueryRow(ctx, `SELECT pg_relation_size('idx_runs_claimable'::regclass),
	        COALESCE((SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = 'idx_runs_claimable'), 0)`).Scan(&st.IdxSizeBytes, &st.IdxScans); err != nil {
		t.Fatalf("read index stats: %v", err)
	}
	return st
}

func (s cpStats) runsDead() int64 {
	for _, tb := range s.Tables {
		if tb.Rel == "runs" {
			return tb.Dead
		}
	}
	return 0
}

// ---------------------------------------------------------------------------------------
// Statements, snapshots, quantiles

type cpStmt struct {
	Present     bool      `json:"present"`
	Name        string    `json:"name,omitempty"`
	Generic     int64     `json:"generic_plans"`
	Custom      int64     `json:"custom_plans"`
	PrepareTime time.Time `json:"prepare_time"`
	Mode        string    `json:"show_plan_cache_mode"`
	PID         uint32    `json:"pid"`
}

func cpSnap(ctx context.Context, pool *pgxpool.Pool, claimSQL string) (cpStmt, error) {
	var s cpStmt
	c, err := pool.Acquire(ctx)
	if err != nil {
		return s, err
	}
	defer c.Release()
	sctx := pmSkip(ctx)
	s.PID = c.Conn().PgConn().PID()
	if err := c.Conn().QueryRow(sctx, `SHOW plan_cache_mode`, pgx.QueryExecModeSimpleProtocol).Scan(&s.Mode); err != nil {
		return s, fmt.Errorf("SHOW plan_cache_mode: %w", err)
	}
	rows, err := c.Conn().Query(sctx, `SELECT name, statement, generic_plans, custom_plans, prepare_time FROM pg_prepared_statements`, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return s, fmt.Errorf("pg_prepared_statements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, text string
		var g, cu int64
		var pt time.Time
		if err := rows.Scan(&name, &text, &g, &cu, &pt); err != nil {
			return s, err
		}
		if text == claimSQL || strings.TrimSpace(text) == strings.TrimSpace(claimSQL) {
			if s.Present {
				return s, errors.New("pg_prepared_statements holds the ClaimRun text twice on one connection")
			}
			s = cpStmt{Present: true, Name: name, Generic: g, Custom: cu, PrepareTime: pt, Mode: s.Mode, PID: s.PID}
		}
	}
	return s, rows.Err()
}

func cpMs(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func cpQ(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	v := pmQuantile(xs, q)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// ---------------------------------------------------------------------------------------
// One call of the segment

type cpCallRes struct {
	dur       time.Duration
	rt        int64
	prepares  int64
	claimExec int64
	outcome   string // claimed | idle | precheck_idle
}

func (r *cpRunner) wantClaimed() bool { return r.cell.Scen == "c" || r.cell.Scen == "d" }

// verifyClaimed checks, inside the savepoint and before ROLLBACK TO, that the claim returned the
// expected run, claimed it, and opened exactly the expected custody hold.
func (r *cpRunner) verifyClaimed(ctx context.Context, q pgx.Tx, run store.Run) error {
	if run.Status != "claimed" {
		return fmt.Errorf("ClaimRun returned run %s with status %q, want claimed", run.ID, run.Status)
	}
	if r.cell.Scen == "c" && run.ID != r.expectRun {
		return fmt.Errorf("ClaimRun returned run %s, want %s", run.ID, r.expectRun)
	}
	if run.UserID != r.prof.user || !run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != r.prof.worker {
		return fmt.Errorf("ClaimRun returned run %s for user %s worker %v, want user %s worker %s", run.ID, run.UserID, run.WorkerID, r.prof.user, r.prof.worker)
	}
	var holds int
	if err := q.QueryRow(pmSkip(ctx), `SELECT count(*)::int FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, run.ID).Scan(&holds); err != nil {
		return fmt.Errorf("count custody holds: %w", err)
	}
	want := 0
	if r.prof.expectHold {
		want = 1
	}
	if holds != want {
		return fmt.Errorf("claimed run %s has %d open custody holds, want %d", run.ID, holds, want)
	}
	return nil
}

// mimic is one ClaimRun segment, in the order documented in the file header.
func (r *cpRunner) mimic(ctx context.Context, pool *pgxpool.Pool, tr *cpTracer) (cpCallRes, error) {
	var res cpCallRes
	var verify time.Duration
	rt0, pr0, ce0 := tr.starts.Load(), tr.prepares.Load(), tr.claimExecs.Load()
	t0 := time.Now()
	finish := func() {
		res.dur = time.Since(t0) - verify
		res.rt = tr.starts.Load() - rt0
		res.prepares = tr.prepares.Load() - pr0
		res.claimExec = tr.claimExecs.Load() - ce0
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback(pmSkip(context.WithoutCancel(ctx)))
		}
	}()
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(ctx, r.prof.worker); err != nil {
		return res, fmt.Errorf("GetWorkerForUpdate: %w", err)
	}
	if r.cell.Cand.c2 {
		var exists bool
		if err := tx.QueryRow(ctx, cpC2SQL, r.prof.user).Scan(&exists); err != nil {
			return res, fmt.Errorf("C2 precheck: %w", err)
		}
		if !exists {
			if err := tx.Commit(ctx); err != nil {
				return res, fmt.Errorf("commit: %w", err)
			}
			done = true
			res.outcome = "precheck_idle"
			finish()
			return res, nil
		}
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT cpm"); err != nil {
		return res, fmt.Errorf("savepoint: %w", err)
	}
	if err := q.OptInRunFences(ctx); err != nil {
		return res, fmt.Errorf("OptInRunFences: %w", err)
	}
	prior := ""
	if r.cell.Cand.c1 {
		var forced pgtype.Text
		if err := tx.QueryRow(ctx, cpC1SetSQL).Scan(&prior, &forced); err != nil {
			return res, fmt.Errorf("C1 set: %w", err)
		}
		if (prior == "auto") != (forced.Valid && forced.String == "force_generic_plan") {
			return res, fmt.Errorf("C1 set: prior mode %q but set_config returned %+v", prior, forced)
		}
	}
	run, cerr := q.ClaimRun(ctx, r.params)
	switch {
	case cerr == nil:
		res.outcome = "claimed"
	case errors.Is(cerr, pgx.ErrNoRows):
		res.outcome = "idle"
	default:
		return res, fmt.Errorf("ClaimRun: %w", cerr)
	}
	if r.cell.Cand.c1 {
		var got string
		if err := tx.QueryRow(ctx, cpC1RestoreSQL, prior).Scan(&got); err != nil {
			return res, fmt.Errorf("C1 restore: %w", err)
		}
		if got != prior {
			return res, fmt.Errorf("C1 restore set %q, want %q", got, prior)
		}
	}
	if res.outcome == "claimed" {
		v0 := time.Now()
		if err := r.verifyClaimed(ctx, tx, run); err != nil {
			return res, err
		}
		verify += time.Since(v0)
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT cpm"); err != nil {
			return res, fmt.Errorf("rollback to savepoint: %w", err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT cpm"); err != nil {
			return res, fmt.Errorf("release savepoint: %w", err)
		}
	} else if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT cpm"); err != nil {
		return res, fmt.Errorf("rollback to savepoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	done = true
	finish()
	return res, nil
}

// isolated is the bare ClaimRun statement in a savepoint, no candidate; only the statement is timed.
func (r *cpRunner) isolated(ctx context.Context, pool *pgxpool.Pool, tr *cpTracer) (cpCallRes, error) {
	var res cpCallRes
	rt0, pr0, ce0 := tr.starts.Load(), tr.prepares.Load(), tr.claimExecs.Load()
	tx, err := pool.BeginTx(pmSkip(ctx), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(pmSkip(context.WithoutCancel(ctx))) }()
	q := store.New(tx)
	err = pmSavepoint(ctx, tx, func() error {
		t0 := time.Now()
		run, cerr := q.ClaimRun(ctx, r.params)
		res.dur = time.Since(t0)
		switch {
		case cerr == nil:
			res.outcome = "claimed"
			return r.verifyClaimed(ctx, tx, run)
		case errors.Is(cerr, pgx.ErrNoRows):
			res.outcome = "idle"
			return nil
		default:
			return fmt.Errorf("ClaimRun: %w", cerr)
		}
	})
	if err != nil {
		return res, err
	}
	res.rt = tr.starts.Load() - rt0
	res.prepares = tr.prepares.Load() - pr0
	res.claimExec = tr.claimExecs.Load() - ce0
	return res, nil
}

// ---------------------------------------------------------------------------------------
// One phase (one fresh connection): cold call, warm-up, timed calls

// cpEarly is the connection's generic/custom plan count for the ClaimRun statement after the
// call-th execution on it (cold call = 1), for the first calls only.
type cpEarly struct {
	Call    int   `json:"call"`
	Generic int64 `json:"generic_plans"`
	Custom  int64 `json:"custom_plans"`
}

type cpPhaseRes struct {
	Kind                 string         `json:"kind"`
	PID                  uint32         `json:"pid"`
	ShowPlanCacheMode    string         `json:"show_plan_cache_mode"`
	ColdMs               float64        `json:"cold_ms"`
	ColdRoundTrips       int64          `json:"cold_round_trips"`
	ColdPrepares         int64          `json:"cold_prepares"`
	ColdOutcome          string         `json:"cold_outcome"`
	WarmupMedianMs       float64        `json:"warmup_median_ms"`
	WarmupMs             []float64      `json:"warmup_ms"`
	FirstCallsSumMs      float64        `json:"cold_plus_warmup_sum_ms"`
	EarlyPlanCounts      []cpEarly      `json:"early_plan_counts_per_call"`
	N                    int            `json:"n"`
	TimedMs              []float64      `json:"timed_ms"`
	P50Ms                float64        `json:"p50_ms"`
	P90Ms                float64        `json:"p90_ms"`
	MaxMs                float64        `json:"max_ms"`
	MeanMs               float64        `json:"mean_ms"`
	Q1P50Ms              float64        `json:"first_quartile_p50_ms"`
	Q4P50Ms              float64        `json:"last_quartile_p50_ms"`
	RoundTripsPerCall    []int64        `json:"round_trips_per_call"`
	RoundTripsMedian     int64          `json:"round_trips_median"`
	Outcomes             map[string]int `json:"timed_outcomes"`
	ClaimExecsTotal      int64          `json:"claim_executions_tracer_total"`
	ClaimExecsTimed      int64          `json:"claim_executions_tracer_timed"`
	GenericDeltaTimed    int64          `json:"generic_plans_delta_timed"`
	CustomDeltaTimed     int64          `json:"custom_plans_delta_timed"`
	GenericDeltaTotal    int64          `json:"generic_plans_delta_total"`
	CustomDeltaTotal     int64          `json:"custom_plans_delta_total"`
	StatementName        string         `json:"claimrun_statement_name"`
	PrepareTimeUnchanged bool           `json:"prepare_time_unchanged"`
	ReplayVerified       bool           `json:"binding_replay_verified"`
	WallSeconds          float64        `json:"wall_seconds"`
	Snapshots            []cpStmt       `json:"snapshots_after_cold_warmup_timed"`
}

func (r *cpRunner) openPool(ctx context.Context, tr *cpTracer) *pgxpool.Pool {
	t := r.env.t
	cfg, err := pgxpool.ParseConfig(r.dsn)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	cfg.ConnConfig.Tracer = tr
	if r.cell.Mode != "auto" {
		cfg.ConnConfig.RuntimeParams["plan_cache_mode"] = r.cell.Mode
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	return pool
}

func (r *cpRunner) checkOutcome(kind string, res cpCallRes) {
	t := r.env.t
	switch {
	case r.wantClaimed() && res.outcome != "claimed":
		t.Fatalf("%s %s/%s %s: scenario %s must claim, got %s", kind, r.cell.Stage, r.cell.Cand.name, r.cell.Label, r.cell.Scen, res.outcome)
	case !r.wantClaimed() && res.outcome == "claimed":
		t.Fatalf("%s %s %s: scenario %s must be idle (pgx.ErrNoRows), but a run was claimed", kind, r.cell.Stage, r.cell.Label, r.cell.Scen)
	}
	wantExec := int64(1)
	if res.outcome == "precheck_idle" {
		wantExec = 0
	}
	if res.claimExec != wantExec {
		t.Fatalf("%s %s: outcome %s but the tracer counted %d ClaimRun executions, want %d", kind, r.cell.Label, res.outcome, res.claimExec, wantExec)
	}
}

func (r *cpRunner) phase(ctx context.Context, kind string, warm, n int,
	call func(context.Context, *pgxpool.Pool, *cpTracer) (cpCallRes, error)) cpPhaseRes {
	t := r.env.t
	start := time.Now()
	tr := &cpTracer{claimSQL: r.env.claimSQL}
	pool := r.openPool(ctx, tr)
	defer pool.Close()
	out := cpPhaseRes{Kind: kind, N: n, Outcomes: map[string]int{}}
	snap := func(stage string) cpStmt {
		s, err := cpSnap(ctx, pool, r.env.claimSQL)
		if err != nil {
			t.Fatalf("%s %s: snapshot after %s: %v", kind, r.cell.Label, stage, err)
		}
		if s.Mode != r.cell.Mode {
			t.Fatalf("%s %s: SHOW plan_cache_mode = %q after %s, want %q", kind, r.cell.Label, s.Mode, stage, r.cell.Mode)
		}
		return s
	}
	s0 := snap("connect")
	if s0.Present {
		t.Fatalf("%s %s: ClaimRun already prepared on a fresh connection", kind, r.cell.Label)
	}
	out.PID, out.ShowPlanCacheMode = s0.PID, s0.Mode

	var replay []any
	tr.setCapture(&replay)
	checkReplay := func() {
		if len(replay) == 0 || out.ReplayVerified {
			return
		}
		if !reflect.DeepEqual(replay, cpArgsOf(r.params)) {
			t.Fatalf("%s %s: the sqlc method sent arguments that differ from the replayed binding", kind, r.cell.Label)
		}
		out.ReplayVerified = true
		tr.setCapture(nil)
	}
	run := func(phase string) cpCallRes {
		res, err := call(ctx, pool, tr)
		if err != nil {
			t.Fatalf("%s %s: %s call: %v", kind, r.cell.Label, phase, err)
		}
		r.checkOutcome(kind, res)
		checkReplay()
		return res
	}

	early := func(call int) {
		if call > 12 {
			return
		}
		s := snap(fmt.Sprintf("call %d", call))
		out.EarlyPlanCounts = append(out.EarlyPlanCounts, cpEarly{Call: call, Generic: s.Generic, Custom: s.Custom})
	}
	cold := run("cold")
	early(1)
	out.ColdMs, out.ColdRoundTrips, out.ColdPrepares, out.ColdOutcome = cpMs(cold.dur), cold.rt, cold.prepares, cold.outcome
	snaps := []cpStmt{snap("cold call")}
	var warms []float64
	for i := 0; i < warm; i++ {
		warms = append(warms, cpMs(run("warm-up").dur))
		early(i + 2)
	}
	out.WarmupMs = warms
	out.FirstCallsSumMs = out.ColdMs
	for _, w := range warms {
		out.FirstCallsSumMs += w
	}
	out.WarmupMedianMs = cpQ(warms, 0.5)
	snaps = append(snaps, snap("warm-up"))
	execsBefore := tr.claimExecs.Load()
	for i := 0; i < n; i++ {
		res := run("timed")
		out.TimedMs = append(out.TimedMs, cpMs(res.dur))
		out.RoundTripsPerCall = append(out.RoundTripsPerCall, res.rt)
		out.Outcomes[res.outcome]++
	}
	out.ClaimExecsTimed = tr.claimExecs.Load() - execsBefore
	out.ClaimExecsTotal = tr.claimExecs.Load()
	snaps = append(snaps, snap("timed calls"))
	out.Snapshots = snaps
	if n > 0 {
		out.P50Ms, out.P90Ms = cpQ(out.TimedMs, 0.5), cpQ(out.TimedMs, 0.9)
		out.MaxMs = cpQ(out.TimedMs, 1)
		out.MeanMs = 0
		for _, v := range out.TimedMs {
			out.MeanMs += v / float64(n)
		}
		qn := n / 4
		if qn == 0 {
			qn = 1
		}
		out.Q1P50Ms = cpQ(out.TimedMs[:qn], 0.5)
		out.Q4P50Ms = cpQ(out.TimedMs[n-qn:], 0.5)
		rts := slices.Clone(out.RoundTripsPerCall)
		slices.Sort(rts)
		out.RoundTripsMedian = rts[len(rts)/2]
	}
	last := snaps[len(snaps)-1]
	out.PrepareTimeUnchanged = true
	if last.Present {
		out.StatementName = last.Name
		for _, s := range snaps {
			if s.Present && (!s.PrepareTime.Equal(last.PrepareTime) || s.Name != last.Name) {
				out.PrepareTimeUnchanged = false
			}
		}
		out.GenericDeltaTimed = last.Generic - snaps[1].Generic
		out.CustomDeltaTimed = last.Custom - snaps[1].Custom
		out.GenericDeltaTotal, out.CustomDeltaTotal = last.Generic, last.Custom
		if !out.PrepareTimeUnchanged {
			t.Fatalf("%s %s: the ClaimRun statement was re-prepared mid-phase (prepare_time or name moved)", kind, r.cell.Label)
		}
	} else if out.ClaimExecsTotal > 0 {
		t.Fatalf("%s %s: ClaimRun ran %d times but pg_prepared_statements never listed it", kind, r.cell.Label, out.ClaimExecsTotal)
	}
	for _, s := range snaps {
		if s.PID != out.PID {
			t.Fatalf("%s %s: the connection changed backend (pid %d then %d)", kind, r.cell.Label, out.PID, s.PID)
		}
	}
	out.WallSeconds = time.Since(start).Seconds()
	return out
}

// ---------------------------------------------------------------------------------------
// Cells

type cpCellRes struct {
	Stage      string      `json:"stage"`
	Candidate  string      `json:"candidate"`
	Mode       string      `json:"plan_cache_mode"`
	Scenario   string      `json:"scenario"`
	Profile    string      `json:"profile"`
	Rep        int         `json:"rep"`
	N          int         `json:"n"`
	Database   string      `json:"database"`
	Mimic      cpPhaseRes  `json:"segment"`
	Isolated   *cpPhaseRes `json:"isolated_statement,omitempty"`
	StatsStart cpStats     `json:"stats_start"`
	StatsEnd   cpStats     `json:"stats_end"`
	SetupSecs  float64     `json:"setup_seconds"`
	TotalSecs  float64     `json:"total_seconds"`
	Calls      int         `json:"calls_per_phase"`
}

func (e *cpEnv) runCell(ctx context.Context, c cpCell) {
	t := e.t
	prof := e.profiles[c.Prof]
	bind := e.binding(ctx, prof)
	started := time.Now()
	e.lab.withClone(ctx, c.Label, func(name, dsn string, obs *pmObserver) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		r := &cpRunner{env: e, cell: c, prof: prof, bind: bind, dsn: dsn, obs: obs}
		defer r.cleanup(context.WithoutCancel(ctx))
		r.prepare(cctx)
		res := cpCellRes{Stage: c.Stage, Candidate: c.Cand.name, Mode: c.Mode, Scenario: c.Scen, Profile: c.Prof, Rep: c.Rep,
			N: c.N, Database: name, Calls: 1 + c.Warm + c.N}
		res.StatsStart = cpReadStats(cctx, t, obs)
		res.SetupSecs = time.Since(started).Seconds()
		res.Mimic = r.phase(cctx, "segment", c.Warm, c.N, r.mimic)
		if e.cfg.isolated && c.Stage != "F" {
			iso := r.phase(cctx, "isolated", c.Warm, c.N, r.isolated)
			res.Isolated = &iso
		}
		res.StatsEnd = cpReadStats(cctx, t, obs)
		r.verifyClean(cctx)
		res.TotalSecs = time.Since(started).Seconds()
		e.out.emit("cell", res)
		e.results = append(e.results, res)
		e.printRow(res)
	})
}

// verifyClean proves nothing a call wrote survived in the clone.
func (r *cpRunner) verifyClean(ctx context.Context) {
	t := r.env.t
	var holds, claimed int64
	if err := r.obs.queryRow(ctx, `SELECT (SELECT count(*) FROM recovery_custody_holds WHERE state = 'open'),
	        (SELECT count(*) FROM runs WHERE status = 'claimed')`, nil, &holds, &claimed); err != nil {
		t.Fatalf("%s: verify clone: %v", r.cell.Label, err)
	}
	if holds != 0 || claimed != 0 {
		t.Fatalf("%s: a call's write survived its rollback: %d open custody holds, %d claimed runs", r.cell.Label, holds, claimed)
	}
	if r.wantClaimed() || r.cell.Scen == "e" {
		var st string
		if err := r.obs.queryRow(ctx, `SELECT status FROM runs WHERE id = $1`, []any{r.expectRun}, &st); err != nil || st != "queued" {
			t.Fatalf("%s: the claimable run is %q (err %v) after the cell, want queued", r.cell.Label, st, err)
		}
	}
}

func (e *cpEnv) printRow(c cpCellRes) {
	row := func(p cpPhaseRes) string {
		return fmt.Sprintf("cold=%.2f|first11sum=%.1f|p50=%.2f|p90=%.2f|max=%.2f|q1p50=%.2f|q4p50=%.2f|gen_d=%d|cus_d=%d|gen_tot=%d|cus_tot=%d|execs=%d|rt_med=%d|rt_cold=%d",
			p.ColdMs, p.FirstCallsSumMs, p.P50Ms, p.P90Ms, p.MaxMs, p.Q1P50Ms, p.Q4P50Ms, p.GenericDeltaTimed, p.CustomDeltaTimed, p.GenericDeltaTotal, p.CustomDeltaTotal, p.ClaimExecsTimed, p.RoundTripsMedian, p.ColdRoundTrips)
	}
	iso := ""
	if c.Isolated != nil {
		iso = "|ISO:" + row(*c.Isolated)
	}
	fmt.Printf("CELLROW|stage=%s|cand=%s|mode=%s|scen=%s|prof=%s|rep=%d|SEG:%s%s|dead_tup=%d->%d|runs_dead=%d->%d|idx_bytes=%d->%d|secs=%.1f\n",
		c.Stage, c.Candidate, c.Mode, c.Scenario, c.Profile, c.Rep, row(c.Mimic), iso,
		c.StatsStart.TotalDeadTuple, c.StatsEnd.TotalDeadTuple, c.StatsStart.runsDead(), c.StatsEnd.runsDead(),
		c.StatsStart.IdxSizeBytes, c.StatsEnd.IdxSizeBytes, c.TotalSecs)
}

// ---------------------------------------------------------------------------------------
// T3: the server-side split (EXPLAIN ANALYZE of a PREPAREd ClaimRun, executions 1..12)

type cpT3Exec struct {
	Stage      string  `json:"stage"`
	Mode       string  `json:"plan_cache_mode"`
	Scenario   string  `json:"scenario"`
	Rep        int     `json:"rep"`
	Database   string  `json:"database"`
	PID        uint32  `json:"pid"`
	Exec       int     `json:"execution_n"`
	WallMs     float64 `json:"client_wall_ms"`
	PlanningMs float64 `json:"planning_ms"`
	ExecMs     float64 `json:"execution_ms"`
	TotalCost  float64 `json:"plan_total_cost"`
	JITPresent bool    `json:"jit_block_present"`
	Generic    int64   `json:"generic_plans_after"`
	Custom     int64   `json:"custom_plans_after"`
}

func (e *cpEnv) runT3Cell(ctx context.Context, c cpCell) {
	t := e.t
	prof := e.profiles[c.Prof]
	bind := e.binding(ctx, prof)
	e.lab.withClone(ctx, c.Label, func(name, dsn string, obs *pmObserver) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		r := &cpRunner{env: e, cell: c, prof: prof, bind: bind, dsn: dsn, obs: obs}
		defer r.cleanup(context.WithoutCancel(ctx))
		r.prepare(cctx)
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("t3: parse: %v", err)
		}
		if c.Mode != "auto" {
			cfg.RuntimeParams["plan_cache_mode"] = c.Mode
		}
		xc, err := pgx.ConnectConfig(cctx, cfg)
		if err != nil {
			t.Fatalf("t3: connect: %v", err)
		}
		defer func() { _ = xc.Close(context.WithoutCancel(ctx)) }()
		var showMode string
		if err := xc.QueryRow(cctx, `SHOW plan_cache_mode`, pgx.QueryExecModeSimpleProtocol).Scan(&showMode); err != nil || showMode != c.Mode {
			t.Fatalf("t3 %s: SHOW plan_cache_mode = %q (err %v), want %q", c.Label, showMode, err, c.Mode)
		}
		pid := xc.PgConn().PID()
		if _, err := xc.Exec(cctx, `PREPARE cpclaim AS `+e.claimSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("t3: PREPARE: %v", err)
		}
		args := cpSimpleArgs(cpArgsOf(r.params))
		ph := make([]string, len(args))
		for i := range ph {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		explain := `EXPLAIN (ANALYZE, SUMMARY ON, FORMAT JSON) EXECUTE cpclaim(` + strings.Join(ph, ",") + `)`
		qargs := append([]any{pgx.QueryExecModeSimpleProtocol}, args...)
		for n := 1; n <= cpT3Executions; n++ {
			tx, err := xc.BeginTx(cctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatalf("t3: begin: %v", err)
			}
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(cctx, prof.worker); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: GetWorkerForUpdate: %v", err)
			}
			if err := q.OptInRunFences(cctx); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: OptInRunFences: %v", err)
			}
			if _, err := tx.Exec(cctx, "SAVEPOINT cpm"); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: savepoint: %v", err)
			}
			t0 := time.Now()
			var raw []byte
			err = tx.QueryRow(cctx, explain, qargs...).Scan(&raw)
			wall := time.Since(t0)
			if err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: execution %d: %v", n, err)
			}
			// The EXPLAIN ANALYZE ran the statement: check its effect inside the savepoint.
			var holds, claimed int64
			if err := tx.QueryRow(pmSkip(cctx), `SELECT (SELECT count(*) FROM recovery_custody_holds WHERE state = 'open'),
			        (SELECT count(*) FROM runs WHERE status = 'claimed')`).Scan(&holds, &claimed); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: verify: %v", err)
			}
			wantClaims := int64(0)
			if r.wantClaimed() {
				wantClaims = 1
			}
			wantHolds := int64(0)
			if r.wantClaimed() && prof.expectHold {
				wantHolds = 1
			}
			if claimed != wantClaims || holds != wantHolds {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3 %s execution %d: %d claimed runs and %d open holds inside the savepoint, want %d and %d", c.Label, n, claimed, holds, wantClaims, wantHolds)
			}
			if _, err := tx.Exec(cctx, "ROLLBACK TO SAVEPOINT cpm"); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: rollback to savepoint: %v", err)
			}
			if _, err := tx.Exec(cctx, "RELEASE SAVEPOINT cpm"); err != nil {
				_ = tx.Rollback(cctx)
				t.Fatalf("t3: release savepoint: %v", err)
			}
			if err := tx.Commit(cctx); err != nil {
				t.Fatalf("t3: commit: %v", err)
			}
			var docs []pmExplainDoc
			if err := json.Unmarshal(raw, &docs); err != nil || len(docs) != 1 {
				t.Fatalf("t3: execution %d: unparseable EXPLAIN JSON (%d docs, err %v): %.200s", n, len(docs), err, raw)
			}
			d := docs[0]
			if d.ExecutionTime <= 0 {
				t.Fatalf("t3: execution %d reports Execution Time %v", n, d.ExecutionTime)
			}
			var generic, custom int64
			if err := xc.QueryRow(cctx, `SELECT generic_plans, custom_plans FROM pg_prepared_statements WHERE name = 'cpclaim'`,
				pgx.QueryExecModeSimpleProtocol).Scan(&generic, &custom); err != nil {
				t.Fatalf("t3: pg_prepared_statements: %v", err)
			}
			ex := cpT3Exec{Stage: c.Stage, Mode: c.Mode, Scenario: c.Scen, Rep: c.Rep, Database: name, PID: pid, Exec: n,
				WallMs: cpMs(wall), PlanningMs: d.PlanningTime, ExecMs: d.ExecutionTime, TotalCost: d.Plan.TotalCost,
				JITPresent: d.JIT != nil, Generic: generic, Custom: custom}
			e.out.emit("t3_exec", ex)
			fmt.Printf("T3ROW|mode=%s|scen=%s|rep=%d|exec=%d|plan_ms=%.3f|exec_ms=%.3f|wall_ms=%.3f|cost=%.1f|generic=%d|custom=%d|jit=%v\n",
				c.Mode, c.Scen, c.Rep, n, ex.PlanningMs, ex.ExecMs, ex.WallMs, ex.TotalCost, generic, custom, ex.JITPresent)
		}
		r.verifyClean(cctx)
	})
}

// cpSimpleArgs rewrites the arguments the simple protocol cannot encode without a server-assigned
// type (a []uuid.UUID has no OID there) into text array literals, which EXECUTE coerces to the
// prepared parameter's type. Every other argument is passed through unchanged.
func cpSimpleArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a
		if ids, ok := a.([]uuid.UUID); ok {
			parts := make([]string, len(ids))
			for j, id := range ids {
				parts[j] = id.String()
			}
			out[i] = "{" + strings.Join(parts, ",") + "}"
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------
// Stage tables

func (e *cpEnv) cell(stage string, cand cpCand, mode, scen, prof string, rep int) cpCell {
	n, warm := e.cfg.n, cpWarmups
	tag := cpModeShort[mode]
	switch stage {
	case "P0":
		n = cpPilotN
	case "F":
		n, warm = 1, 0
	}
	if prof != "p1" || stage == "T5" || stage == "F" {
		tag = prof
	}
	return cpCell{Stage: stage, Cand: cand, Mode: mode, Scen: scen, Prof: prof, Rep: rep, N: n, Warm: warm,
		Label: cpLabel(stage, cand, scen, tag, rep)}
}

func (e *cpEnv) stageCells(stage string) []cpCell {
	const auto, gen, cus = "auto", "force_generic_plan", "force_custom_plan"
	var cells []cpCell
	switch stage {
	case "P0":
		for _, s := range []string{"a", "c"} {
			cells = append(cells, e.cell("P0", cpBase, auto, s, "p1", 1))
		}
	case "F":
		for _, p := range []string{"p1", "p2", "p3", "p4"} {
			for _, s := range []string{"a", "c"} {
				cells = append(cells, e.cell("F", cpBase, auto, s, p, 1))
			}
		}
		// The T4 scenarios, once each (a fixture feasibility check, not a measurement).
		for _, s := range []string{"b", "d", "e"} {
			cells = append(cells, e.cell("F", cpBase, auto, s, "p1", 1))
		}
		cells = append(cells, e.cell("F", cpBoth, auto, "c", "p1", 1), e.cell("F", cpBoth, auto, "e", "p1", 1))
	case "T1":
		for rep := 1; rep <= 3; rep++ {
			var combos []cpCell
			for _, s := range []string{"a", "c"} {
				for _, cand := range []cpCand{cpBase, cpC1, cpC2, cpBoth} {
					combos = append(combos, e.cell("T1", cand, auto, s, "p1", rep))
				}
			}
			cells = append(cells, cpRotate(combos, (rep-1)*3)...)
		}
	case "T2":
		for rep := 1; rep <= 3; rep++ {
			var combos []cpCell
			for _, m := range []string{gen, cus} {
				for _, s := range []string{"a", "c"} {
					combos = append(combos, e.cell("T2", cpBase, m, s, "p1", rep))
				}
			}
			cells = append(cells, cpRotate(combos, rep-1)...)
		}
	case "T3":
		for rep := 1; rep <= 2; rep++ {
			var combos []cpCell
			for _, m := range []string{auto, gen, cus} {
				for _, s := range []string{"a", "c"} {
					c := e.cell("T3", cpBase, m, s, "p1", rep)
					c.T3 = true
					combos = append(combos, c)
				}
			}
			cells = append(cells, cpRotate(combos, (rep-1)*3)...)
		}
	case "T4":
		for rep := 1; rep <= 3; rep++ {
			var combos []cpCell
			for _, s := range []string{"b", "d", "e"} {
				for _, cand := range []cpCand{cpBase, e.cfg.winner} {
					combos = append(combos, e.cell("T4", cand, auto, s, "p1", rep))
				}
			}
			cells = append(cells, cpRotate(combos, rep-1)...)
		}
	case "T5":
		for i, p := range []string{"p1", "p2", "p3", "p4"} {
			cands := []cpCand{cpBase, e.cfg.winner}
			if i%2 == 1 {
				cands = []cpCand{e.cfg.winner, cpBase}
			}
			for _, s := range []string{"a", "c"} {
				for _, cand := range cands {
					cells = append(cells, e.cell("T5", cand, auto, s, p, 1))
				}
			}
		}
	case "R":
		for rep := 1; rep <= 3; rep++ {
			for _, s := range []string{"b", "d", "e"} {
				cells = append(cells, e.cell("R", e.cfg.runnerUp, auto, s, "p1", rep))
			}
		}
		for _, p := range []string{"p1", "p2", "p3", "p4"} {
			for _, s := range []string{"a", "c"} {
				cells = append(cells, e.cell("R", e.cfg.runnerUp, auto, s, p, 1))
			}
		}
	}
	return cells
}

// ---------------------------------------------------------------------------------------
// Projection (from the pilot's measured per-clone and per-call cost)

type cpProjection struct {
	PerCloneSetupSecs map[string]float64 `json:"per_clone_setup_seconds"`
	PerCallSecs       map[string]float64 `json:"per_call_seconds_segment_plus_isolated"`
	Stages            map[string]float64 `json:"projected_seconds_by_stage"`
	TotalSecs         float64            `json:"projected_seconds_total"`
	N                 int                `json:"assumed_n"`
	Note              string             `json:"note"`
}

func (e *cpEnv) project(n int) cpProjection {
	setup := map[string]float64{}
	perCall := map[string]float64{}
	cnt := map[string]int{}
	for _, r := range e.results {
		if r.Stage != "P0" {
			continue
		}
		k := r.Scenario
		calls := float64(r.Calls)
		call := r.Mimic.WallSeconds
		if r.Isolated != nil {
			call += r.Isolated.WallSeconds
		}
		setup[k] += r.SetupSecs
		perCall[k] += call / calls
		cnt[k]++
	}
	for k := range setup {
		setup[k] /= float64(cnt[k])
		perCall[k] /= float64(cnt[k])
	}
	p := cpProjection{PerCloneSetupSecs: setup, PerCallSecs: perCall, Stages: map[string]float64{}, N: n,
		Note: "linear model from the P0 cells: idle-type scenarios (a,b,e) use P0's (a) cost, claimable-type (c,d) use P0's (c) cost; T3 counts its 12 executions at the (a)/(c) per-call cost; stage E (the M3 end-to-end Service.Claim cells) is not part of this projection"}
	if len(cnt) < 2 {
		return p
	}
	calls := float64(1 + cpWarmups + n)
	cost := func(scen string, callCount float64) float64 {
		k := "a"
		if scen == "c" || scen == "d" {
			k = "c"
		}
		return setup[k] + callCount*perCall[k]
	}
	add := func(stage string, scens []string, reps int) {
		for _, s := range scens {
			p.Stages[stage] += float64(reps) * cost(s, calls)
		}
	}
	add("T1", []string{"a", "c"}, 12) // 4 candidates x 3 reps per scenario
	add("T2", []string{"a", "c"}, 6)
	for _, s := range []string{"a", "c"} {
		p.Stages["T3"] += 6 * cost(s, float64(cpT3Executions))
	}
	add("T4", []string{"b", "d", "e"}, 6)
	add("T5", []string{"a", "c"}, 8)
	add("R", []string{"b", "d", "e"}, 3)
	add("R", []string{"a", "c"}, 4)
	for _, v := range p.Stages {
		p.TotalSecs += v
	}
	return p
}

// ---------------------------------------------------------------------------------------
// The test

func TestClaimRunPlanMeasurement(t *testing.T) {
	if os.Getenv("UZI_CLAIMRUN_PLAN_MEASURE") != "1" {
		t.Skip("UZI_CLAIMRUN_PLAN_MEASURE=1 not set: opt-in ClaimRun plan measurement harness")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; the measurement needs a throwaway superuser Postgres")
	}
	cfg := cpLoadConfig(t)
	if _, err := pmSQLLoad(); err != nil {
		t.Fatalf("parse sqlc query texts: %v", err)
	}
	claimSQL := pmSQLTexts()["ClaimRun"]
	if claimSQL == "" {
		t.Fatal("no sqlc text for ClaimRun")
	}
	ctx := context.Background()
	pmRequireThrowawayDB(ctx, t, dsn)
	outPath := filepath.Clean(cfg.out)
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create output: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close output: %v", err)
		}
	}()
	out := &pmOut{f: f}

	lab, err := newPMLab(t, out, dsn, filepath.Dir(outPath), "default")
	if err != nil {
		t.Fatalf("template name: %v", err)
	}
	tplName := lab.templateName
	if err := lab.create(ctx, tplName, "CREATE DATABASE "+pgx.Identifier{tplName}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatalf("create template: %v", err)
	}
	t.Cleanup(func() {
		if err := lab.drop(tplName); err != nil {
			t.Errorf("drop template: %v", err)
		}
	})
	tdsn := pmDSNForDB(t, dsn, tplName)
	if err := store.Migrate(ctx, tdsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fx := pmSeed(ctx, t, tdsn)
	cx := cpOverlay(ctx, t, tdsn, fx)
	tobs := cpTemplateStats(ctx, t, tdsn)
	time.Sleep(500 * time.Millisecond) // let the template's last backend exit before the first clone

	meta := cpMeta(ctx, t, dsn)
	meta["started_at"] = time.Now().UTC()
	meta["stages"] = cfg.stages
	meta["n_timed_calls"] = cfg.n
	meta["pilot_n"] = cpPilotN
	meta["warmups"] = cpWarmups
	meta["winner"] = cfg.winner.name
	meta["runner_up"] = cfg.runnerUp.name
	meta["isolated_pass"] = cfg.isolated
	meta["clone_analyze"] = cfg.cloneAnalyze
	meta["template_database"] = lab.templateName
	meta["database_prefix"] = lab.prefix
	meta["fleet"] = map[string]any{"fleet_users": cx.Users, "online_workers": cx.OnlineWorkers, "backlog_runs_scenario_d": cfg.backlog,
		"mid_history_users": cpMidUsers, "mid_history_runs_each": cpMidHistory}
	meta["template_stats"] = tobs
	out.emit("meta", meta)

	env := &cpEnv{t: t, out: out, lab: lab, fx: fx, cx: cx, cfg: cfg, claimSQL: claimSQL,
		profiles: cpMakeProfiles(fx, cx), bind: map[string]*cpBinding{}}

	for _, stage := range cfg.stages {
		began := time.Now()
		if stage == "S" {
			fmt.Printf("STAGE|S|scales=%v\n", cfg.scales)
			for _, scale := range cfg.scales {
				env.runScale(ctx, scale)
			}
			out.emit("stage_done", map[string]any{"stage": "S", "scales": cfg.scales, "wall_seconds": time.Since(began).Seconds()})
			fmt.Printf("STAGEDONE|S|wall_seconds=%.1f\n", time.Since(began).Seconds())
			continue
		}
		if stage == "E" {
			fmt.Printf("STAGE|E|cells=%d\n", len(cpECells()))
			env.runServiceStage(ctx)
			out.emit("stage_done", map[string]any{"stage": "E", "cells": len(cpECells()), "wall_seconds": time.Since(began).Seconds()})
			fmt.Printf("STAGEDONE|E|wall_seconds=%.1f\n", time.Since(began).Seconds())
			continue
		}
		cells := env.stageCells(stage)
		fmt.Printf("STAGE|%s|cells=%d\n", stage, len(cells))
		for _, c := range cells {
			if c.T3 {
				env.runT3Cell(ctx, c)
			} else {
				env.runCell(ctx, c)
			}
		}
		out.emit("stage_done", map[string]any{"stage": stage, "cells": len(cells), "wall_seconds": time.Since(began).Seconds()})
		fmt.Printf("STAGEDONE|%s|cells=%d|wall_seconds=%.1f\n", stage, len(cells), time.Since(began).Seconds())
		if stage == "P0" {
			for _, n := range []int{100, 50} {
				p := env.project(n)
				out.emit("projection", p)
				fmt.Printf("PROJECTION|n=%d|total_seconds=%.0f|total_minutes=%.1f|stages=%v|setup=%v|per_call=%v\n",
					n, p.TotalSecs, p.TotalSecs/60, p.Stages, p.PerCloneSetupSecs, p.PerCallSecs)
			}
		}
	}
	out.emit("done", map[string]any{"finished_at": time.Now().UTC()})
}

// cpTemplateStats records last_analyze / last_autoanalyze and the autovacuum reloptions of the
// seeded template (read on its own connection, closed before any clone is created).
func cpTemplateStats(ctx context.Context, t *testing.T, dsn string) map[string]any {
	t.Helper()
	obs := &pmObserver{}
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("template stats connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	obs.conn = c
	st := cpReadStats(ctx, t, obs)
	var tables, off int64
	if err := c.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE reloptions @> ARRAY['autovacuum_enabled=false'])
	    FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind = 'r'`).Scan(&tables, &off); err != nil {
		t.Fatalf("template reloptions: %v", err)
	}
	if tables == 0 || off != tables {
		t.Fatalf("autovacuum_enabled=false on %d of %d template tables", off, tables)
	}
	return map[string]any{"stats": st, "tables": tables, "tables_autovacuum_disabled": off}
}

func cpMeta(ctx context.Context, t *testing.T, dsn string) map[string]any {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("meta connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	m := map[string]any{}
	for _, k := range []string{"server_version", "jit", "jit_above_cost", "jit_inline_above_cost", "jit_optimize_above_cost",
		"plan_cache_mode", "shared_buffers", "max_connections", "autovacuum", "synchronous_commit", "fsync", "work_mem",
		"max_parallel_workers_per_gather", "default_statistics_target"} {
		var v string
		if err := c.QueryRow(ctx, `SHOW `+k, pgx.QueryExecModeSimpleProtocol).Scan(&v); err != nil {
			t.Fatalf("SHOW %s: %v", k, err)
		}
		m[k] = v
	}
	host, _ := os.Hostname()
	m["host"] = host
	m["go_version"] = runtime.Version()
	m["gomaxprocs"] = runtime.GOMAXPROCS(0)
	m["num_cpu"] = runtime.NumCPU()
	m["gomemlimit_env"] = os.Getenv("GOMEMLIMIT")
	m["gomemlimit_effective_bytes"] = debug.SetMemoryLimit(-1)
	if m["plan_cache_mode"] != "auto" || m["autovacuum"] != "on" {
		t.Fatalf("server defaults moved: plan_cache_mode=%v autovacuum=%v", m["plan_cache_mode"], m["autovacuum"])
	}
	return m
}
