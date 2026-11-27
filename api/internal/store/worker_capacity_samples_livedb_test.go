package store_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// worker_capacity_samples_livedb_test.go executes the real SampleWorkerCapacityBatch statement and
// the pinned-session lock (issue #4693) against PostgreSQL. The statement samples EVERY worker in
// the database, so each top-level test stands its own isolated database (standIsolatedDB). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const wcsStaleAfter = 45*time.Second + 500*time.Millisecond

var wcsBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// wcsAt is an instant inside five-minute bucket k (k counts buckets from wcsBase).
func wcsAt(k int) time.Time { return wcsBase.Add(time.Duration(k)*5*time.Minute + 90*time.Second) }

// wcsBucket is the start of bucket k.
func wcsBucket(k int) time.Time { return wcsBase.Add(time.Duration(k) * 5 * time.Minute) }

type wcsFx struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	q     *store.Queries
	n     int
	repos map[uuid.UUID]uuid.UUID
	// egress, when set, binds the next inserted issue run to that egress profile.
	egress uuid.UUID
}

func newWcsFx(t *testing.T) *wcsFx {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "wcs_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &wcsFx{t: t, ctx: ctx, pool: pool, q: store.New(pool), repos: map[uuid.UUID]uuid.UUID{}}
}

func (f *wcsFx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture SQL: %v\n%s", err, sql)
	}
}

func (f *wcsFx) user() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, fmt.Sprintf("wcs-%s@e2e", id))
	return id
}

func (f *wcsFx) repo(owner uuid.UUID) uuid.UUID {
	f.t.Helper()
	if r, ok := f.repos[owner]; ok {
		return r
	}
	conn, repo := uuid.New(), uuid.New()
	f.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, owner, []byte{1})
	f.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,1,'g/wcs','https://forge.e2e/g/wcs','main',true)`, repo, conn)
	f.repos[owner] = repo
	return repo
}

type wcsWorkerOpts struct {
	status    string
	heartbeat *time.Time
	maxRuns   *int32
	maxCross  *int32
}

// worker inserts an external worker owned by owner. Zero opts: online with a fresh heartbeat.
func (f *wcsFx) worker(owner uuid.UUID, o wcsWorkerOpts) uuid.UUID {
	f.t.Helper()
	if o.status == "" {
		o.status = "online"
	}
	id := uuid.New()
	f.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, max_cross_check_slots)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, owner, id.String(), id[:], o.status, o.heartbeat, o.maxRuns, o.maxCross)
	return id
}

func i32(v int32) *int32 { return &v }

// run inserts a run of the given kind and status bound to worker (uuid.Nil for none). A terminal
// issue run serves as the target of the kinds that need one.
func (f *wcsFx) run(owner uuid.UUID, kind, status string, worker uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	var w any
	if worker != uuid.Nil {
		w = worker
	}
	f.n++
	repo := f.repo(owner)
	cols := `id, user_id, kind, status, worker_id, issue_title, issue_description, claim_generation`
	vals := `$1, $2, $3, $4, $5, 'wcs', 'wcs', 1`
	args := []any{id, owner, kind, status, w}
	add := func(col string, v any) {
		args = append(args, v)
		cols += ", " + col
		vals += fmt.Sprintf(", $%d", len(args))
	}
	switch kind {
	case "issue", "self_improve":
		add("repo_id", repo)
		add("issue_iid", f.n)
		if f.egress != uuid.Nil && kind == "issue" {
			add("egress_profile_id", f.egress)
		}
	case "ci_fix":
		add("repo_id", repo)
		add("pipeline_id", f.n)
		add("pipeline_ref", "refs/heads/main")
	case "prompt":
		add("repo_id", repo)
	case "task":
		add("repo_id", repo)
		add("branch", fmt.Sprintf("uzi/task/%d", f.n))
	case "mr_rework":
		add("repo_id", repo)
		add("pipeline_ref", "refs/heads/main")
		add("mr_iid", f.n)
		add("target_run_id", f.target(owner))
	case "judge":
		add("target_run_id", f.target(owner))
	case "job":
		add("job_type", "research")
	case "cross_check":
		add("repo_id", repo)
		add("target_run_id", f.target(owner))
		add("harness", "codex")
		add("report_only", true)
		add("budget_wall_seconds", 600)
	case "chat":
	default:
		f.t.Fatalf("fixture: unhandled kind %q", kind)
	}
	f.exec(`INSERT INTO runs (`+cols+`) VALUES (`+vals+`)`, args...)
	return id
}

// profileRun inserts an issue run bound to a fresh egress profile (the run shape the isolated lane
// claims), in the given status, on worker (uuid.Nil for none).
func (f *wcsFx) profileRun(owner uuid.UUID, status string, worker uuid.UUID) uuid.UUID {
	f.t.Helper()
	profile := uuid.New()
	f.exec(`INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
		profile, "wcs-"+profile.String()[:20])
	f.egress = profile
	defer func() { f.egress = uuid.Nil }()
	return f.run(owner, "issue", status, worker)
}

// isolatedWorker creates the run-bound isolated-lane worker for runID and returns its id. The run
// is moved to running on it by the caller.
func (f *wcsFx) isolatedWorker(owner, runID uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	w, err := f.q.CreateEphemeralHostedWorker(f.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID: owner, Name: name, TokenHash: []byte(name + "-token-hash-0123456789abcdef!"),
		TemplateDeclared: pgtype.Text{String: "default", Valid: true}, HostedSize: pgtype.Text{String: "small", Valid: true},
		DockerEnabled: pgtype.Bool{Bool: false, Valid: true}, EphemeralRunID: runID, AnthropicBindMode: "default", IsolatedLane: true,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE workers SET status='online', last_heartbeat_at=$2 WHERE id=$1`, w.ID, wcsAt(0))
	return w.ID
}

// laneRun inserts a claimed cross-check run on the durable cross-check lane.
func (f *wcsFx) laneRun(owner, worker uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := f.run(owner, "cross_check", "claimed", worker)
	f.exec(`UPDATE runs SET cross_check_lane = true WHERE id = $1`, id)
	var lane bool
	if err := f.pool.QueryRow(f.ctx, `SELECT cross_check_lane FROM runs WHERE id=$1`, id).Scan(&lane); err != nil || !lane {
		f.t.Fatalf("fixture: lane run did not keep cross_check_lane (lane=%v err=%v)", lane, err)
	}
	return id
}

func (f *wcsFx) target(owner uuid.UUID) uuid.UUID {
	f.t.Helper()
	return f.run(owner, "issue", "completed", uuid.Nil)
}

func wcsTs(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func wcsParams(obs time.Time, size int32) store.SampleWorkerCapacityBatchParams {
	p := store.SampleWorkerCapacityBatchParams{
		StaleAfter: pgtype.Interval{Microseconds: wcsStaleAfter.Microseconds(), Valid: true},
		BatchSize:  size,
	}
	if !obs.IsZero() {
		p.ObservedAt = wcsTs(obs)
	}
	return p
}

// session opens the pinned session and fails the test when the lock is not acquired.
func (f *wcsFx) session(stmt, lock time.Duration) *store.WorkerCapacitySampleSession {
	f.t.Helper()
	s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, f.pool, stmt, lock)
	if err != nil || !got {
		f.t.Fatalf("open session: got=%v err=%v", got, err)
	}
	f.t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	return s
}

// tick runs one batch at bucket k on a fresh session and returns its result.
func (f *wcsFx) tick(k int, size int32) store.SampleWorkerCapacityBatchRow {
	f.t.Helper()
	s := f.session(10*time.Second, 5*time.Second)
	row, err := s.SampleBatch(f.ctx, wcsParams(wcsAt(k), size))
	if err != nil {
		f.t.Fatalf("batch at bucket %d: %v", k, err)
	}
	if unlocked, err := s.Close(f.ctx); err != nil || !unlocked {
		f.t.Fatalf("close: unlocked=%v err=%v", unlocked, err)
	}
	return row
}

type wcsSample struct {
	Worker, User            uuid.UUID
	Bucket, Observed        time.Time
	Freshness               string
	Heartbeat               *time.Time
	Kind                    string
	Ephemeral, IsolatedLane bool
	MaxRuns, MaxCross       *int32
	Attribution             string
	Ordinary, Lane, Chat    *int32
}

const wcsSampleCols = `worker_id, user_id, bucket_start, observed_at, freshness, last_heartbeat_at, worker_kind, ephemeral,
 isolated_lane, max_concurrent_runs, max_cross_check_slots, attribution, ordinary_active, cross_check_lane_active, chat_active`

func (f *wcsFx) samples(worker uuid.UUID) []wcsSample {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT `+wcsSampleCols+` FROM worker_capacity_samples WHERE worker_id=$1 ORDER BY bucket_start`, worker)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []wcsSample
	for rows.Next() {
		var s wcsSample
		if err := rows.Scan(&s.Worker, &s.User, &s.Bucket, &s.Observed, &s.Freshness, &s.Heartbeat, &s.Kind, &s.Ephemeral,
			&s.IsolatedLane, &s.MaxRuns, &s.MaxCross, &s.Attribution, &s.Ordinary, &s.Lane, &s.Chat); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// sampleAt returns the single sample the worker has in bucket k.
func (f *wcsFx) sampleAt(worker uuid.UUID, k int) wcsSample {
	f.t.Helper()
	for _, s := range f.samples(worker) {
		if s.Bucket.Equal(wcsBucket(k)) {
			return s
		}
	}
	f.t.Fatalf("worker %s has no sample in bucket %d", worker, k)
	return wcsSample{}
}

func (f *wcsFx) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func wantCounts(t *testing.T, what string, s wcsSample, attribution string, ordinary, lane, chat int32) {
	t.Helper()
	if s.Attribution != attribution {
		t.Errorf("%s: attribution = %q, want %q", what, s.Attribution, attribution)
		return
	}
	if attribution == "unavailable" {
		if s.Ordinary != nil || s.Lane != nil || s.Chat != nil {
			t.Errorf("%s: unavailable sample carries counts (%v %v %v)", what, s.Ordinary, s.Lane, s.Chat)
		}
		return
	}
	if s.Ordinary == nil || s.Lane == nil || s.Chat == nil {
		t.Errorf("%s: owner sample has a NULL count (%v %v %v)", what, s.Ordinary, s.Lane, s.Chat)
		return
	}
	if *s.Ordinary != ordinary || *s.Lane != lane || *s.Chat != chat {
		t.Errorf("%s: ordinary/lane/chat = %d/%d/%d, want %d/%d/%d", what, *s.Ordinary, *s.Lane, *s.Chat, ordinary, lane, chat)
	}
}

// Example 1: an ephemeral worker is sampled while it holds a running run, then deleted; the sample
// stays, attributed to its owner.
func TestWorkerCapacitySampleEphemeralWorkerDeletedKeepsSampleLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	run := f.run(owner, "issue", "queued", uuid.Nil)
	w, err := f.q.CreateEphemeralHostedWorker(f.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID: owner, Name: "eph", TokenHash: []byte("eph-token-hash-0123456789abcdef"),
		TemplateDeclared: pgtype.Text{String: "default", Valid: true}, HostedSize: pgtype.Text{String: "small", Valid: true},
		DockerEnabled: pgtype.Bool{Bool: false, Valid: true}, EphemeralRunID: run, AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE workers SET status='online', last_heartbeat_at=$2 WHERE id=$1`, w.ID, wcsAt(0))
	f.exec(`UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, run, w.ID)
	if row := f.tick(0, 100); row.Inserted != 1 {
		t.Fatalf("first tick inserted %d, want 1", row.Inserted)
	}
	f.exec(`UPDATE runs SET status='completed' WHERE id=$1`, run)
	n, err := f.q.DeleteEphemeralWorkerForRun(f.ctx, store.DeleteEphemeralWorkerForRunParams{RunID: run})
	if err != nil || n != 1 {
		t.Fatalf("DeleteEphemeralWorkerForRun: n=%d err=%v", n, err)
	}
	if got := f.count(`SELECT count(*) FROM workers WHERE id=$1`, w.ID); got != 0 {
		t.Fatalf("worker still present after delete")
	}
	got := f.samples(w.ID)
	if len(got) != 1 {
		t.Fatalf("samples after worker deletion = %d, want 1", len(got))
	}
	s := got[0]
	if s.User != owner || !s.Ephemeral || s.Kind != "hosted" || s.Freshness != "online" {
		t.Errorf("sample = %+v", s)
	}
	wantCounts(t, "ephemeral worker", s, "owner", 1, 0, 0)
}

// Example 2: an unknown cap stays NULL; a known cap, including zero, is copied verbatim.
func TestWorkerCapacitySampleNullCapsStayNullLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	unknown := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	known := f.worker(owner, wcsWorkerOpts{heartbeat: &hb, maxRuns: i32(0), maxCross: i32(2)})
	f.tick(0, 100)
	u := f.sampleAt(unknown, 0)
	if u.MaxRuns != nil || u.MaxCross != nil {
		t.Errorf("unknown caps recorded as %v / %v, want NULL", u.MaxRuns, u.MaxCross)
	}
	k := f.sampleAt(known, 0)
	if k.MaxRuns == nil || *k.MaxRuns != 0 || k.MaxCross == nil || *k.MaxCross != 2 {
		t.Errorf("known caps recorded as %v / %v, want 0 / 2", k.MaxRuns, k.MaxCross)
	}
}

// Example 3 and freshness precision: the staleness window is 45.5s, a heartbeat exactly at the
// cutoff is online, one microsecond earlier is stale, and stale or offline workers are never online.
func TestWorkerCapacitySampleFreshnessLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	obs := wcsAt(0)
	at := func(d time.Duration) *time.Time { v := obs.Add(-d); return &v }
	cases := map[string]struct {
		opts wcsWorkerOpts
		want string
	}{
		"exactly at cutoff":       {wcsWorkerOpts{heartbeat: at(wcsStaleAfter)}, "online"},
		"one microsecond earlier": {wcsWorkerOpts{heartbeat: at(wcsStaleAfter + time.Microsecond)}, "stale"},
		"inside 45.5s not 45s":    {wcsWorkerOpts{heartbeat: at(45*time.Second + 200*time.Millisecond)}, "online"},
		"online without a beat":   {wcsWorkerOpts{}, "stale"},
		"offline with fresh beat": {wcsWorkerOpts{status: "offline", heartbeat: at(time.Second)}, "offline"},
		"offline no beat":         {wcsWorkerOpts{status: "offline"}, "offline"},
	}
	ids := map[string]uuid.UUID{}
	for name, c := range cases {
		o := c.opts
		ids[name] = f.worker(owner, o)
	}
	f.tick(0, 100)
	online := 0
	for name, c := range cases {
		s := f.sampleAt(ids[name], 0)
		if s.Freshness != c.want {
			t.Errorf("%s: freshness = %q, want %q", name, s.Freshness, c.want)
		}
		if s.Freshness == "online" {
			online++
		}
	}
	if online != 2 {
		t.Errorf("online-classified workers = %d, want 2 (stale and offline must not count as online capacity)", online)
	}
}

// Example 4: deleting the owner removes the owner's samples and nobody else's.
func TestWorkerCapacitySampleOwnerDeleteCascadesLiveDB(t *testing.T) {
	f := newWcsFx(t)
	a, b := f.user(), f.user()
	hb := wcsAt(0)
	wa := f.worker(a, wcsWorkerOpts{heartbeat: &hb})
	wb := f.worker(b, wcsWorkerOpts{heartbeat: &hb})
	f.tick(0, 100)
	f.tick(1, 100)
	// Both owners must have recorded both buckets, or a cascade that removes nothing would pass.
	for _, w := range []uuid.UUID{wa, wb} {
		f.sampleAt(w, 0)
		f.sampleAt(w, 1)
	}
	f.exec(`DELETE FROM users WHERE id=$1`, a)
	if got := f.samples(wa); len(got) != 0 {
		t.Errorf("deleted owner still has samples: %+v", got)
	}
	// The other owner keeps exactly its two buckets.
	f.sampleAt(wb, 0)
	f.sampleAt(wb, 1)
	if got := f.samples(wb); len(got) != 2 {
		t.Errorf("other owner samples = %+v, want buckets 0 and 1 only", got)
	}
}

// Occupancy: the slot-holding set, the lane split, supporting statuses, the isolated-lane flag and
// over-cap values (never clamped).
func TestWorkerCapacitySampleOccupancyLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)

	// every slot-holding status counts, each on a different ordinary kind
	all := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	for i, st := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		f.run(owner, []string{"issue", "prompt", "task", "ci_fix", "self_improve"}[i], st, all)
	}
	// chat is counted separately from ordinary
	chat := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	f.run(owner, "chat", "running", chat)
	f.run(owner, "issue", "running", chat)
	// the cross-check lane is counted apart; a cross-check on an ordinary slot is ordinary
	lane := f.worker(owner, wcsWorkerOpts{heartbeat: &hb, maxCross: i32(1)})
	f.laneRun(owner, lane)
	f.run(owner, "cross_check", "running", lane)
	// statuses outside the slot set: pool_wait, recovery_wait and a queued run with only worker affinity
	idle := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	f.run(owner, "issue", "pool_wait", idle)
	f.run(owner, "prompt", "recovery_wait", idle)
	f.run(owner, "task", "queued", idle)
	// over the advertised cap: stored as observed
	over := f.worker(owner, wcsWorkerOpts{heartbeat: &hb, maxRuns: i32(1)})
	for i := 0; i < 3; i++ {
		f.run(owner, "issue", "running", over)
	}
	// an isolated-lane worker holding its profile-bound run (the shape ClaimRun's lane clause admits)
	// is sampled like any other with its flag recorded
	isoRun := f.profileRun(owner, "queued", uuid.Nil)
	isoID := f.isolatedWorker(owner, isoRun, "iso")
	f.exec(`UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, isoRun, isoID)

	f.tick(0, 100)
	wantCounts(t, "all statuses", f.sampleAt(all, 0), "owner", 5, 0, 0)
	wantCounts(t, "chat separate", f.sampleAt(chat, 0), "owner", 1, 0, 1)
	wantCounts(t, "lane + ordinary cross-check", f.sampleAt(lane, 0), "owner", 1, 1, 0)
	wantCounts(t, "no slot held", f.sampleAt(idle, 0), "owner", 0, 0, 0)
	o := f.sampleAt(over, 0)
	wantCounts(t, "over cap", o, "owner", 3, 0, 0)
	if o.MaxRuns == nil || *o.MaxRuns != 1 {
		t.Errorf("over-cap worker cap = %v, want 1", o.MaxRuns)
	}
	is := f.sampleAt(isoID, 0)
	wantCounts(t, "isolated lane", is, "owner", 1, 0, 0)
	if !is.IsolatedLane || !is.Ephemeral {
		t.Errorf("isolated worker sample = %+v, want isolated_lane and ephemeral recorded", is)
	}
}

// Attribution: a cross-owner slot-holding run, a lane shape outside the proven gate, the CHECK that
// refuses a partial shape, a real chat continuation and a real fresh-resume prepare, claim and restore.
func TestWorkerCapacitySampleAttributionLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner, other := f.user(), f.user()
	hb := wcsAt(0)

	// a slot-holding run of another owner: unavailable, counts NULL, the rest still recorded
	cross := f.worker(owner, wcsWorkerOpts{heartbeat: &hb, maxRuns: i32(4)})
	f.run(owner, "issue", "running", cross)
	f.run(other, "issue", "running", cross)
	// cross-owner runs OUTSIDE the slot set neither count nor flip attribution
	bystander := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	f.run(owner, "issue", "running", bystander)
	f.run(other, "issue", "pool_wait", bystander)
	f.run(other, "prompt", "queued", bystander)
	f.run(other, "task", "recovery_wait", bystander)
	// a slot-holding run whose kind is outside the proven gate (constraints dropped in this owned DB only)
	f.exec(`ALTER TABLE runs DROP CONSTRAINT runs_kind_check`)
	f.exec(`ALTER TABLE runs DROP CONSTRAINT runs_kind_shape`)
	unproven := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	f.run(owner, "issue", "running", unproven)
	f.exec(`INSERT INTO runs (id, user_id, kind, status, worker_id, issue_title, issue_description)
 VALUES ($1,$2,'future_kind','running',$3,'x','x')`, uuid.New(), owner, unproven)

	// the lane clause is two-way, as in ClaimRun: an unprofiled run on an isolated-lane worker and a
	// profile-bound run on a non-isolated worker are both outside the proven shape
	mmRunA := f.run(owner, "issue", "queued", uuid.Nil)
	mmIso := f.isolatedWorker(owner, mmRunA, "mma")
	f.exec(`UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, mmRunA, mmIso)
	mmPlain := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	f.profileRun(owner, "running", mmPlain)

	f.tick(0, 100)
	a := f.sampleAt(mmIso, 0)
	wantCounts(t, "unprofiled run on an isolated worker", a, "unavailable", 0, 0, 0)
	if !a.IsolatedLane || !a.Ephemeral {
		t.Errorf("unavailable isolated sample lost its flags: %+v", a)
	}
	wantCounts(t, "profile-bound run on a non-isolated worker", f.sampleAt(mmPlain, 0), "unavailable", 0, 0, 0)
	c := f.sampleAt(cross, 0)
	wantCounts(t, "cross-owner slot run", c, "unavailable", 0, 0, 0)
	if c.MaxRuns == nil || *c.MaxRuns != 4 || c.Freshness != "online" || c.Heartbeat == nil {
		t.Errorf("unavailable sample lost caps/freshness/heartbeat: %+v", c)
	}
	wantCounts(t, "cross-owner outside slot set", f.sampleAt(bystander, 0), "owner", 1, 0, 0)
	wantCounts(t, "unproven lane shape", f.sampleAt(unproven, 0), "unavailable", 0, 0, 0)

	// direct inserts with a partial shape are rejected
	for name, q := range map[string]string{
		"owner without counts":      `INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, worker_kind, ephemeral, isolated_lane, attribution) VALUES (gen_random_uuid(), now(), $1, now(), 'online', 'external', false, false, 'owner')`,
		"owner with one NULL count": `INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, worker_kind, ephemeral, isolated_lane, attribution, ordinary_active, cross_check_lane_active) VALUES (gen_random_uuid(), now(), $1, now(), 'online', 'external', false, false, 'owner', 0, 0)`,
		"unavailable with counts":   `INSERT INTO worker_capacity_samples (worker_id, bucket_start, user_id, observed_at, freshness, worker_kind, ephemeral, isolated_lane, attribution, ordinary_active, cross_check_lane_active, chat_active) VALUES (gen_random_uuid(), now(), $1, now(), 'online', 'external', false, false, 'unavailable', 0, 0, 0)`,
	} {
		_, err := f.pool.Exec(f.ctx, q, owner)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Errorf("%s: err = %v, want check violation 23514", name, err)
		}
	}

	// real chat continuation: queued with the worker pre-set holds no slot; once claimed it counts as chat
	prior := f.run(owner, "chat", "completed", uuid.Nil)
	chatW := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	cont, err := f.q.CreateChatContinueRun(f.ctx, store.CreateChatContinueRunParams{
		UserID: owner, IssueTitle: "continue", ResumeOfRunID: pgtype.UUID{Bytes: prior, Valid: true},
		WorkerID: pgtype.UUID{Bytes: chatW, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cont.WorkerID.Valid || cont.WorkerID.Bytes != chatW || cont.Status != "queued" {
		t.Fatalf("continue run = status %q worker %v; the fixture needs a queued run with affinity", cont.Status, cont.WorkerID)
	}
	f.tick(1, 100)
	wantCounts(t, "chat continue queued", f.sampleAt(chatW, 1), "owner", 0, 0, 0)
	f.exec(`UPDATE runs SET status='claimed', claim_generation=1 WHERE id=$1`, cont.ID)
	f.tick(2, 100)
	wantCounts(t, "chat continue claimed", f.sampleAt(chatW, 2), "owner", 0, 0, 1)

	// real fresh-resume: prepare queues the run on W2 (no slot), the claim end state holds one on W2, restore returns it to W1 (no slot)
	w1 := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	w2 := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	fr := f.run(owner, "issue", "recovery_wait", w1)
	f.exec(`UPDATE runs SET started_at = now() - interval '1 hour', status_since = now() - interval '10 minutes',
 recovery_wait_cause = 'retained_recovery_adoption_failed', released_worker_id = $2, claim_released_at = now() - interval '11 minutes' WHERE id = $1`, fr, w1)
	pend := uuid.New()
	f.exec(`INSERT INTO run_fresh_resumes (id, run_id, user_id, request_id, evidence_generation, evidence_kind,
 checkpoint_ref, sha, candidate_token, state) VALUES ($1,$2,$3,$4,1,'checkpoint_ref','refs/uzi-checkpoints/x','abc','tok','pending')`,
		pend, fr, owner, uuid.New())
	inTx := func(fn func(*store.Queries, pgx.Tx)) {
		t.Helper()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		qtx := f.q.WithTx(tx)
		if err := qtx.OptInFreshResume(f.ctx, pend.String()); err != nil {
			t.Fatal(err)
		}
		fn(qtx, tx)
		if err := qtx.ClearFreshResumeOptIn(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(3, 100)
	wantCounts(t, "fresh resume before prepare", f.sampleAt(w1, 3), "owner", 0, 0, 0)
	inTx(func(q *store.Queries, _ pgx.Tx) {
		if _, err := q.PrepareFreshResumeClaim(f.ctx, store.PrepareFreshResumeClaimParams{RunID: fr, ID: pend, WorkerID: pgtype.UUID{Bytes: w2, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	})
	f.tick(4, 100)
	wantCounts(t, "fresh resume prepared (queued on W2)", f.sampleAt(w2, 4), "owner", 0, 0, 0)
	wantCounts(t, "fresh resume prepared (W1)", f.sampleAt(w1, 4), "owner", 0, 0, 0)
	inTx(func(q *store.Queries, tx pgx.Tx) {
		if _, err := tx.Exec(f.ctx, `UPDATE runs SET status='claimed', claim_generation=4, claimed_at=now(), claim_released_at=NULL,
 released_worker_id=NULL, released_worker_nonce=NULL, status_since=now(), codex_claim_epoch=codex_claim_epoch+1, codex_cap_hash=NULL WHERE id=$1`, fr); err != nil {
			t.Fatal(err)
		}
		if _, err := q.MarkFreshResumeClaimed(f.ctx, store.MarkFreshResumeClaimedParams{
			ID: pend, WorkerID: pgtype.UUID{Bytes: w2, Valid: true}, ClaimGeneration: pgtype.Int8{Int64: 4, Valid: true}, TransferSeconds: 7200}); err != nil {
			t.Fatal(err)
		}
	})
	f.tick(5, 100)
	wantCounts(t, "fresh resume claim end state (W2)", f.sampleAt(w2, 5), "owner", 1, 0, 0)
	wantCounts(t, "fresh resume claim end state (W1)", f.sampleAt(w1, 5), "owner", 0, 0, 0)
	inTx(func(q *store.Queries, _ pgx.Tx) {
		if n, err := q.RestoreFreshResumeTuple(f.ctx, pend); err != nil || n != 1 {
			t.Fatalf("restore: n=%d err=%v", n, err)
		}
		if _, err := q.CloseFreshResume(f.ctx, store.CloseFreshResumeParams{ID: pend, State: "refused", Reason: "assembly"}); err != nil {
			t.Fatal(err)
		}
	})
	f.tick(6, 100)
	wantCounts(t, "fresh resume restored (W2)", f.sampleAt(w2, 6), "owner", 0, 0, 0)
	wantCounts(t, "fresh resume restored (W1)", f.sampleAt(w1, 6), "owner", 0, 0, 0)
}

// The production clock: a NULL observed_at means statement_timestamp(), not the enclosing
// transaction's now(), and the bucket follows that instant.
func TestWorkerCapacitySampleProductionClockLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := time.Now()
	w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	var t0, lb, ub time.Time
	if err := tx.QueryRow(f.ctx, `SELECT now()`).Scan(&t0); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `SELECT pg_sleep(0.05)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(f.ctx, `SELECT statement_timestamp()`).Scan(&lb); err != nil {
		t.Fatal(err)
	}
	if !lb.After(t0) {
		t.Fatalf("test setup: statement_timestamp %v is not after now() %v", lb, t0)
	}
	if _, err := f.q.WithTx(tx).SampleWorkerCapacityBatch(f.ctx, wcsParams(time.Time{}, 10)); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(f.ctx, `SELECT clock_timestamp()`).Scan(&ub); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.samples(w)
	if len(got) != 1 {
		t.Fatalf("samples = %d, want 1", len(got))
	}
	obs := got[0].Observed
	if !lb.Before(obs) || obs.After(ub) {
		t.Errorf("observed_at %v not in (%v, %v]; it was taken from the transaction start", obs, lb, ub)
	}
	if want := obs.UTC().Truncate(5 * time.Minute); !got[0].Bucket.Equal(want) {
		t.Errorf("bucket = %v, want %v", got[0].Bucket, want)
	}
}

// A worker with a sample in a LATER bucket gets no earlier-bucket insert after a clock step backwards.
func TestWorkerCapacitySampleClockRegressionLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	if row := f.tick(1, 10); row.Inserted != 1 {
		t.Fatalf("tick at B+1 inserted %d, want 1", row.Inserted)
	}
	row := f.tick(0, 10)
	if row.Scanned != 0 || row.Inserted != 0 {
		t.Errorf("tick at the earlier bucket scanned/inserted %d/%d, want 0/0", row.Scanned, row.Inserted)
	}
	if got := f.samples(w); len(got) != 1 || !got[0].Bucket.Equal(wcsBucket(1)) {
		t.Errorf("samples = %+v, want exactly the B+1 sample", got)
	}
}

// A bucket with no tick stays empty: a worker sampled at B and again at B+2 has exactly those two
// samples, nothing is backfilled at B+1.
func TestWorkerCapacitySampleMissedBucketStaysEmptyLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	if row := f.tick(0, 10); row.Inserted != 1 {
		t.Fatalf("tick at B inserted %d, want 1", row.Inserted)
	}
	if row := f.tick(2, 10); row.Inserted != 1 {
		t.Fatalf("tick at B+2 inserted %d, want 1", row.Inserted)
	}
	got := f.samples(w)
	if len(got) != 2 || !got[0].Bucket.Equal(wcsBucket(0)) || !got[1].Bucket.Equal(wcsBucket(2)) {
		t.Errorf("samples = %d, want exactly the B and B+2 samples with none at B+1: %+v", len(got), got)
	}
}

// Close on a session whose connection is open but inside a transaction (open or failed) must not
// run the unlock: it reports no proven unlock, closes the connection, and the lock ends with the
// backend.
func TestWorkerCapacitySampleCloseNonIdleLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		stmts  []string
		status byte
	}{
		{"open transaction", []string{"BEGIN"}, 'T'},
		{"failed transaction", []string{"BEGIN", "SELECT 1/0"}, 'E'},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newWcsFx(t)
			p := f.probePool(t)
			s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, p.pool, 10*time.Second, 5*time.Second)
			if err != nil || !got {
				t.Fatalf("open session: got=%v err=%v", got, err)
			}
			t.Cleanup(func() { _, _ = s.Close(context.Background()) })
			for _, st := range c.stmts {
				_ = s.ExecForTest(f.ctx, st) // the failing statement is expected to error
			}
			if s.TxStatusForTest() != c.status || s.Idle() {
				t.Fatalf("setup: tx status %q idle=%v, want %q and not idle", s.TxStatusForTest(), s.Idle(), c.status)
			}
			unlocked, err := s.Close(f.ctx)
			if unlocked || err != nil {
				t.Errorf("Close on a non-idle session: unlocked=%v err=%v, want false/nil (no unlock attempted)", unlocked, err)
			}
			f.waitNoSessions(t, p.tag)
			f.waitLockFree(t)
		})
	}
}

// A repeated tick in the same bucket inserts nothing.
func TestWorkerCapacitySampleRepeatedTickLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	if row := f.tick(0, 10); row.Inserted != 1 {
		t.Fatalf("first tick inserted %d, want 1", row.Inserted)
	}
	if row := f.tick(0, 10); row.Scanned != 0 || row.Inserted != 0 {
		t.Errorf("second tick in the same bucket scanned/inserted %d/%d, want 0/0", row.Scanned, row.Inserted)
	}
	if got := f.samples(w); len(got) != 1 {
		t.Errorf("samples = %d, want 1", len(got))
	}
	if row := f.tick(1, 10); row.Inserted != 1 {
		t.Errorf("next bucket inserted %d, want 1", row.Inserted)
	}
}

// A fleet larger than one batch is sampled exactly once per worker across the batches of a tick and
// successive ticks of the same bucket.
func TestWorkerCapacitySampleFleetLargerThanBatchLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	var ws []uuid.UUID
	for i := 0; i < 7; i++ {
		ws = append(ws, f.worker(owner, wcsWorkerOpts{heartbeat: &hb}))
	}
	s := f.session(10*time.Second, 5*time.Second)
	var inserted, scanned int64
	for i, wantScanned := range []int64{3, 3, 1} {
		row, err := s.SampleBatch(f.ctx, wcsParams(wcsAt(0), 3))
		if err != nil {
			t.Fatal(err)
		}
		if row.Scanned != wantScanned || row.Inserted != wantScanned {
			t.Fatalf("batch %d scanned/inserted %d/%d, want %d/%d", i, row.Scanned, row.Inserted, wantScanned, wantScanned)
		}
		inserted += row.Inserted
		scanned += row.Scanned
	}
	if _, err := s.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	// a later tick in the same bucket reaches nobody new
	if row := f.tick(0, 3); row.Scanned != 0 || row.Inserted != 0 {
		t.Errorf("later tick scanned/inserted %d/%d, want 0/0", row.Scanned, row.Inserted)
	}
	// ticks of 3 reach a fleet of 7 over successive ticks, each worker once
	f.exec(`DELETE FROM worker_capacity_samples`)
	for tick, want := range []int64{3, 3, 1, 0} {
		if row := f.tick(0, 3); row.Inserted != want {
			t.Errorf("tick %d inserted %d, want %d", tick, row.Inserted, want)
		}
	}
	for _, w := range ws {
		if got := f.samples(w); len(got) != 1 {
			t.Errorf("worker %s has %d samples, want exactly 1", w, len(got))
		}
	}
	if inserted != 7 || scanned != 7 {
		t.Errorf("one session covered %d scanned / %d inserted, want 7/7", scanned, inserted)
	}
}

// The page's own LIMIT applies before any occupancy work: scanned equals the batch size.
func TestWorkerCapacitySamplePageCardinalityLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	for i := 0; i < 5; i++ {
		f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	}
	if row := f.tick(0, 2); row.Scanned != 2 || row.Inserted != 2 {
		t.Errorf("scanned/inserted %d/%d, want 2/2", row.Scanned, row.Inserted)
	}
}

// A failed insert leaves nothing behind and the session usable; a retry in the same bucket yields
// exactly one sample per worker.
func TestWorkerCapacitySampleFailedInsertRetryLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	var ws []uuid.UUID
	for i := 0; i < 3; i++ {
		ws = append(ws, f.worker(owner, wcsWorkerOpts{heartbeat: &hb}))
	}
	blocker, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(f.ctx) }()
	if _, err := blocker.Exec(f.ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); err != nil {
		t.Fatal(err)
	}
	s := f.session(10*time.Second, 200*time.Millisecond)
	_, err = s.SampleBatch(f.ctx, wcsParams(wcsAt(0), 10))
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("blocked batch err = %v, want lock_not_available 55P03", err)
	}
	if !s.Idle() {
		t.Error("session is not idle after a failed batch")
	}
	if got := f.count(`SELECT count(*) FROM worker_capacity_samples`); got != 0 {
		t.Fatalf("failed batch left %d rows", got)
	}
	if err := blocker.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	row, err := s.SampleBatch(f.ctx, wcsParams(wcsAt(0), 10))
	if err != nil || row.Inserted != 3 {
		t.Fatalf("retry: row=%+v err=%v", row, err)
	}
	for _, w := range ws {
		if got := f.samples(w); len(got) != 1 {
			t.Errorf("worker %s has %d samples after retry, want 1", w, len(got))
		}
	}
}

// A batch that committed but whose session then vanished abruptly (the socket is closed with no
// unlock round trip and the batch result is never used) is not repeated by the next session.
func TestWorkerCapacitySampleLostReplyLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	var ws []uuid.UUID
	for i := 0; i < 3; i++ {
		ws = append(ws, f.worker(owner, wcsWorkerOpts{heartbeat: &hb}))
	}
	p := f.probePool(t)
	s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, p.pool, 10*time.Second, 5*time.Second)
	if err != nil || !got {
		t.Fatalf("open session: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	if _, err := s.SampleBatch(f.ctx, wcsParams(wcsAt(0), 10)); err != nil { // the result is discarded unused
		t.Fatal(err)
	}
	p.killClient(t) // no Close, no unlock
	// a separate session sees the committed rows
	for _, w := range ws {
		if got := f.count(`SELECT count(*) FROM worker_capacity_samples WHERE worker_id=$1`, w); got != 1 {
			t.Fatalf("committed batch left %d rows for %s", got, w)
		}
	}
	f.waitLockFree(t)
	if row := f.tick(0, 10); row.Inserted != 0 {
		t.Errorf("the next session inserted %d rows for already-sampled workers", row.Inserted)
	}
	for _, w := range ws {
		if got := f.count(`SELECT count(*) FROM worker_capacity_samples WHERE worker_id=$1`, w); got != 1 {
			t.Errorf("worker %s has %d samples after the next session, want 1", w, got)
		}
	}
}

// wcsProbe is a pool of its own whose sessions carry a unique application_name and whose client
// sockets the test can reach: it lets a test see a leaked session and end one abruptly. Only one
// dial is allowed, so a client-side cancel request (a second dial) cannot reach the server.
type wcsProbe struct {
	pool       *pgxpool.Pool
	tag        string
	mu         sync.Mutex
	conns      []net.Conn
	lastSQL    string
	afterSetup func() // runs when the session-settings statement has just completed
}

func (p *wcsProbe) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	p.mu.Lock()
	p.lastSQL = d.SQL
	p.mu.Unlock()
	return ctx
}

func (p *wcsProbe) TraceQueryEnd(_ context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	p.mu.Lock()
	hook, isSetup := p.afterSetup, strings.Contains(p.lastSQL, "set_config")
	p.mu.Unlock()
	if hook != nil && isSetup {
		hook()
	}
}

func (f *wcsFx) probePool(t *testing.T) *wcsProbe {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	p := &wcsProbe{tag: "wcs-probe-" + uuid.NewString()}
	cfg.MaxConns = 2
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = p.tag
	cfg.ConnConfig.Tracer = p
	d := &net.Dialer{}
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.conns) > 0 {
			return nil, errors.New("probe pool: only one dial is allowed")
		}
		c, err := d.DialContext(ctx, network, addr)
		if err == nil {
			p.conns = append(p.conns, c)
		}
		return c, err
	}
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	p.pool = pool
	return p
}

// killClient closes the probe session's client socket with no Terminate and no cancel request.
func (p *wcsProbe) killClient(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) != 1 {
		t.Fatalf("probe pool dialed %d connections, want 1", len(p.conns))
	}
	_ = p.conns[0].Close()
}

// waitNoSessions polls, within 1.5s, until no backend carries the probe's application_name.
func (f *wcsFx) waitNoSessions(t *testing.T, tag string) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		n := f.count(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, tag)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d session(s) tagged %s still open 1.5s after they were abandoned", n, tag)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The try-lock round trip fails with the connection still open (the context expires between the
// setup statement and the try): the session must be closed, not leaked, even though the caller's
// context is already expired. The idle-session backstop (5s) is longer than the bound here.
func TestWorkerCapacitySampleTryLockErrorClosesConnLiveDB(t *testing.T) {
	f := newWcsFx(t)
	p := f.probePool(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	p.mu.Lock()
	p.afterSetup = cancel
	p.mu.Unlock()
	s, got, err := store.OpenWorkerCapacitySampleSession(ctx, p.pool, time.Second, time.Second)
	if s != nil || got || err == nil || !strings.Contains(err.Error(), "try lock") {
		t.Fatalf("open: session=%v got=%v err=%v, want nil/false and a try-lock error", s, got, err)
	}
	p.pool.Close()
	f.waitNoSessions(t, p.tag)
	f.waitLockFree(t)
}

// A client that vanishes while its statement is blocked on a row lock is noticed by the server's
// connection check, which frees the advisory lock long before the 10s statement and lock timeouts.
func TestWorkerCapacitySampleClientVanishedMidStatementLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
	blocker, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(f.ctx) }()
	if _, err := blocker.Exec(f.ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); err != nil {
		t.Fatal(err)
	}
	p := f.probePool(t)
	s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, p.pool, 10*time.Second, 10*time.Second)
	if err != nil || !got {
		t.Fatalf("open session: got=%v err=%v", got, err)
	}
	t.Cleanup(func() { _, _ = s.Close(context.Background()) })
	done := make(chan error, 1)
	go func() {
		_, err := s.SampleBatch(f.ctx, wcsParams(wcsAt(0), 10))
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'`, p.tag) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the batch never blocked on the row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.killClient(t)
	select {
	case err := <-done:
		if err == nil {
			t.Error("a batch on a killed connection returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the batch did not return after its connection was closed")
	}
	f.waitLockFree(t)
}

// The session lock: exclusion, proven unlock, an unknown try result closes the connection, and a
// statement abandoned at its client deadline frees the lock.
func TestWorkerCapacitySampleSessionLockLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	f.worker(owner, wcsWorkerOpts{heartbeat: &hb})

	first := f.session(5*time.Second, 5*time.Second)
	second, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, f.pool, time.Second, time.Second)
	if err != nil || got || second != nil {
		t.Fatalf("second open while held: session=%v got=%v err=%v, want nil/false/nil", second, got, err)
	}
	if unlocked, err := first.Close(f.ctx); err != nil || !unlocked {
		t.Fatalf("close: unlocked=%v err=%v, want proven unlock", unlocked, err)
	}
	third, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, f.pool, time.Second, time.Second)
	if err != nil || !got {
		t.Fatalf("open after a normal close: got=%v err=%v, want immediate acquisition", got, err)
	}
	if _, err := third.Close(f.ctx); err != nil {
		t.Fatal(err)
	}

	// a timeout under 1ms would render as "0" and disable the server timer: refused before acquiring
	for _, c := range [][2]time.Duration{{0, time.Second}, {time.Second, 0}, {999 * time.Microsecond, time.Second}, {time.Second, time.Microsecond}} {
		if s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, f.pool, c[0], c[1]); err == nil || got || s != nil {
			t.Fatalf("open with timeouts %v: session=%v got=%v err=%v, want a refusal", c, s, got, err)
		}
	}
	f.waitLockFree(t)

	// the context expires somewhere inside open: every outcome must leave the lock free
	for _, d := range []time.Duration{time.Nanosecond, 20 * time.Microsecond, 100 * time.Microsecond, 400 * time.Microsecond, 2 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		s, got, _ := store.OpenWorkerCapacitySampleSession(ctx, f.pool, time.Second, time.Second)
		cancel()
		if s != nil {
			_, _ = s.Close(context.Background())
		}
		if s == nil && got {
			t.Fatalf("deadline %v: acquired without a session", d)
		}
		f.waitLockFree(t)
	}

	// a statement blocked past the client deadline returns a context error; the lock is then reacquirable
	blocker, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(f.ctx) }()
	if _, err := blocker.Exec(f.ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, owner); err != nil {
		t.Fatal(err)
	}
	s := f.session(10*time.Second, 10*time.Second)
	ctx, cancel := context.WithTimeout(f.ctx, 300*time.Millisecond)
	start := time.Now()
	_, err = s.SampleBatch(ctx, wcsParams(wcsAt(0), 10))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked batch err = %v, want a context deadline error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("blocked batch took %v, want it bounded by the client deadline", time.Since(start))
	}
	if s.Idle() {
		t.Error("session reports idle after an abandoned statement")
	}
	if unlocked, _ := s.Close(f.ctx); unlocked {
		t.Error("abandoned session claims a proven unlock")
	}
	f.waitLockFree(t)
}

// waitLockFree polls, within 1.5s (well under the 5s idle-session and 10s statement backstops, so
// only the client-side close or the connection check can have freed the lock), until a fresh
// session acquires and releases the lock.
func (f *wcsFx) waitLockFree(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		s, got, err := store.OpenWorkerCapacitySampleSession(f.ctx, f.pool, time.Second, time.Second)
		if err == nil && got {
			if _, err := s.Close(f.ctx); err != nil {
				t.Fatal(err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not reacquirable within 1.5s (got=%v err=%v)", got, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The occupancy probe on runs is served by runs_worker_nonterminal_idx. The statement text is the
// generated const from worker_capacity_samples.sql.go, so an edit to the query is what is explained.
func TestWorkerCapacitySampleRunsProbeUsesIndexLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	repo := f.repo(owner)
	for i := 0; i < 20; i++ {
		w := f.worker(owner, wcsWorkerOpts{heartbeat: &hb})
		f.run(owner, "issue", "running", w)
		// terminal history on the worker is what makes the partial index the cheaper probe
		f.exec(`INSERT INTO runs (user_id, repo_id, kind, status, worker_id, issue_title, issue_description)
 SELECT $1, $2, 'prompt', 'completed', $3, 'h', 'h' FROM generate_series(1, 400)`, owner, repo, w)
	}
	src, err := os.ReadFile("worker_capacity_samples.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)const sampleWorkerCapacityBatch = `(.*?)`").FindSubmatch(src)
	if m == nil {
		t.Fatal("generated query constant not found")
	}
	stmt := strings.TrimSpace(string(m[1]))
	f.exec(`ANALYZE`)
	rows, err := f.pool.Query(f.ctx, "EXPLAIN (COSTS OFF) "+stmt, pgtype.Timestamptz{}, pgtype.Interval{Microseconds: 1, Valid: true}, int32(10))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "runs_worker_nonterminal_idx") {
		t.Errorf("the runs probe does not use runs_worker_nonterminal_idx:\n%s", plan.String())
	}
}

// Two statements racing on the same bucket (the lock normally prevents it) end with one sample per
// worker and no error: the later one waits on the first's uncommitted rows, then skips them.
func TestWorkerCapacitySampleConcurrentSamplersLiveDB(t *testing.T) {
	f := newWcsFx(t)
	owner := f.user()
	hb := wcsAt(0)
	var ws []uuid.UUID
	for i := 0; i < 3; i++ {
		ws = append(ws, f.worker(owner, wcsWorkerOpts{heartbeat: &hb}))
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if row, err := f.q.WithTx(tx).SampleWorkerCapacityBatch(f.ctx, wcsParams(wcsAt(0), 10)); err != nil || row.Inserted != 3 {
		t.Fatalf("first sampler: row=%+v err=%v", row, err)
	}
	type result struct {
		row store.SampleWorkerCapacityBatchRow
		err error
	}
	done := make(chan result, 1)
	go func() {
		row, err := f.q.SampleWorkerCapacityBatch(f.ctx, wcsParams(wcsAt(0), 10))
		done <- result{row, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second sampler never blocked on the first's uncommitted rows")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.row.Inserted != 0 {
			t.Errorf("second sampler: row=%+v err=%v, want 0 inserted and no error", r.row, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second sampler did not finish after the first committed")
	}
	for _, w := range ws {
		if got := f.samples(w); len(got) != 1 {
			t.Errorf("worker %s has %d samples, want 1", w, len(got))
		}
	}
}
