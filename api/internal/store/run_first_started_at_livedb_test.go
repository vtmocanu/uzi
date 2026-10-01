package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #2004: runs.first_started_at is the never-reset display anchor; runs.started_at stays the
// budget/timeout anchor that every limit/recovery/pool promotion NULLs for a fresh wall. These
// live-DB tests pin both halves of that split against a real Postgres.

type fsaFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	q        *store.Queries
	userID   uuid.UUID
	repoID   uuid.UUID
	live     store.Worker // fresh heartbeat + wall_park_v1
	stale    store.Worker // never heartbeated
	nextIssu int64
}

func newFSAFixture(t *testing.T) *fsaFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &fsaFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New(), nextIssu: 100}
	connID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.userID, fmt.Sprintf("fsa-%s@e2e", f.userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, f.userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, f.repoID, connID)
	mk := func(name string) store.Worker {
		w, err := f.q.CreateWorker(ctx, store.CreateWorkerParams{
			UserID: f.userID, Name: name, TokenHash: append([]byte(name+"-"), f.userID[:]...), AnthropicBindMode: "auto",
		})
		if err != nil {
			t.Fatalf("CreateWorker %s: %v", name, err)
		}
		return w
	}
	f.live = mk("fsa-live")
	f.stale = mk("fsa-stale")
	mustExec(ctx, t, pool, `UPDATE workers SET last_heartbeat_at = now(), protocol_capabilities = ARRAY['wall_park_v1'] WHERE id = $1`, f.live.ID)
	return f
}

// startedRun inserts a claimed run on the worker, reports it running through SetRunRunning (the
// only writer of first_started_at), then backdates BOTH anchors by `age`.
func (f *fsaFixture) startedRun(w store.Worker, age time.Duration, wallSeconds int) uuid.UUID {
	f.t.Helper()
	f.nextIssu++
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, budget_wall_seconds)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'claimed', $5, $6)`, id, f.userID, f.repoID, f.nextIssu, w.ID, wallSeconds)
	f.setRunning(id, w)
	if r := f.read(id); !r.firstStarted.Valid || !r.startedAt.Valid {
		f.t.Fatalf("SetRunRunning must stamp both anchors on first start, got first=%v started=%v", r.firstStarted, r.startedAt)
	}
	back := time.Now().Add(-age)
	mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET started_at = $2, first_started_at = $2 WHERE id = $1`, id, back)
	return id
}

func (f *fsaFixture) setRunning(id uuid.UUID, w store.Worker) {
	f.t.Helper()
	rows, err := f.q.SetRunRunning(f.ctx, store.SetRunRunningParams{ID: id, WorkerID: pgU(w.ID), IterationCount: 1})
	if err != nil || rows != 1 {
		f.t.Fatalf("SetRunRunning(%s): rows=%d err=%v", id, rows, err)
	}
}

type fsaRow struct {
	status                   string
	startedAt, firstStarted  pgtype.Timestamptz
	wall                     pgtype.Int4
	paused, ext, finalizeSec int32
	kind                     string
	interactive              bool
}

func (f *fsaFixture) read(id uuid.UUID) fsaRow {
	f.t.Helper()
	var r fsaRow
	err := f.pool.QueryRow(f.ctx, `SELECT status, started_at, first_started_at, budget_wall_seconds,
	       budget_paused_seconds, budget_extension_seconds, budget_finalize_seconds, kind, interactive
	  FROM runs WHERE id = $1`, id).Scan(&r.status, &r.startedAt, &r.firstStarted, &r.wall, &r.paused, &r.ext, &r.finalizeSec, &r.kind, &r.interactive)
	if err != nil {
		f.t.Fatalf("read run %s: %v", id, err)
	}
	return r
}

func fsaTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// TestRunFirstStartedAtSurvivesResumeCyclesLiveDB: limit, recovery and pool promotions (sweep and
// single-row forms) NULL started_at for a fresh wall; the reclaim + SetRunRunning that follows
// re-stamps it fresh. first_started_at must come through every cycle untouched.
func TestRunFirstStartedAtSurvivesResumeCyclesLiveDB(t *testing.T) {
	f := newFSAFixture(t)
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	sweepNow := fsaTS(past.Add(24 * time.Hour))

	id := f.startedRun(f.stale, 5*time.Hour, 3600)
	orig := f.read(id)
	if !orig.firstStarted.Valid || !orig.startedAt.Valid {
		t.Fatalf("fixture: both anchors must be set, got %+v", orig)
	}

	cycles := []struct {
		name    string
		park    string
		promote func() int64
	}{
		{"limit sweep", `status='limit_wait', retry_not_before=$2`, func() int64 {
			rows, err := f.q.PromoteLimitWaitRuns(f.ctx, sweepNow)
			if err != nil {
				t.Fatalf("PromoteLimitWaitRuns: %v", err)
			}
			return countID(rows, id)
		}},
		{"limit now", `status='limit_wait', retry_not_before=$2`, func() int64 {
			n, err := f.q.PromoteLimitWaitRunNow(f.ctx, store.PromoteLimitWaitRunNowParams{ID: id, UserID: f.userID})
			if err != nil {
				t.Fatalf("PromoteLimitWaitRunNow: %v", err)
			}
			return n
		}},
		{"recovery sweep", `status='recovery_wait', recovery_retry_not_before=$2`, func() int64 {
			rows, err := f.q.PromoteRecoveryWaitRuns(f.ctx, sweepNow)
			if err != nil {
				t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
			}
			var n int64
			for _, r := range rows {
				if r.ID == id {
					n++
				}
			}
			return n
		}},
		{"recovery now", `status='recovery_wait', recovery_retry_not_before=$2`, func() int64 {
			n, err := f.q.PromoteRecoveryWaitRunNow(f.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: f.userID})
			if err != nil {
				t.Fatalf("PromoteRecoveryWaitRunNow: %v", err)
			}
			return n
		}},
		{"pool", `status='pool_wait'`, func() int64 {
			n, err := f.q.PromotePoolWaitRun(f.ctx, store.PromotePoolWaitRunParams{ID: id, UserID: f.userID})
			if err != nil {
				t.Fatalf("PromotePoolWaitRun: %v", err)
			}
			return n
		}},
	}

	for _, c := range cycles {
		t.Run(c.name, func(t *testing.T) {
			// Park (raw: the production parkers write many unrelated columns this test does not
			// exercise), promote, reclaim, report running.
			if strings.Contains(c.park, "$2") {
				mustExec(f.ctx, t, f.pool, `UPDATE runs SET `+c.park+` WHERE id=$1`, id, past)
			} else {
				mustExec(f.ctx, t, f.pool, `UPDATE runs SET `+c.park+` WHERE id=$1`, id)
			}
			if n := c.promote(); n != 1 {
				t.Fatalf("promote affected %d of the run, want 1", n)
			}
			mid := f.read(id)
			if mid.status != "queued" || mid.startedAt.Valid {
				t.Fatalf("after promote: status=%q started_at=%v, want queued with NULL started_at (fresh wall)", mid.status, mid.startedAt)
			}
			if !mid.firstStarted.Valid || !mid.firstStarted.Time.Equal(orig.firstStarted.Time) {
				t.Fatalf("promotion touched first_started_at: %v, want %v", mid.firstStarted, orig.firstStarted.Time)
			}
			mustExec(f.ctx, t, f.pool, `UPDATE runs SET status='claimed' WHERE id=$1`, id)
			f.setRunning(id, f.stale)
			got := f.read(id)
			if !got.firstStarted.Valid || !got.firstStarted.Time.Equal(orig.firstStarted.Time) {
				t.Fatalf("first_started_at = %v, want unchanged %v", got.firstStarted, orig.firstStarted.Time)
			}
			if !got.startedAt.Valid || time.Since(got.startedAt.Time) > time.Minute {
				t.Fatalf("started_at = %v, want a fresh stamp (budget anchor restarts)", got.startedAt)
			}
		})
	}
}

// TestRunResumedLegGetsFreshWallLiveDB: a run that burned most of its wall, parked, promoted and
// resumed must NOT be selected by the wall sweeps, and its RunDeadline is the NEW started_at plus
// the wall, never first_started_at plus the wall. A control run with the same backdate that was
// never cycled proves the sweeps would otherwise select it.
func TestRunResumedLegGetsFreshWallLiveDB(t *testing.T) {
	f := newFSAFixture(t)
	const wall = 3600
	age := 3500 * time.Second
	tick := time.Now().Add(10 * time.Minute) // past the OLD deadline (3500s + 600s > 3600s)

	for _, tc := range []struct {
		name   string
		worker store.Worker
		sweep  func(id uuid.UUID) bool
	}{
		{"RequestWallParks", f.live, func(id uuid.UUID) bool {
			rows, err := f.q.RequestWallParks(f.ctx, store.RequestWallParksParams{
				Now: fsaTS(tick), GlobalTimeoutSeconds: 86400, WorkerStaleCutoff: fsaTS(time.Now().Add(-time.Hour)),
			})
			if err != nil {
				t.Fatalf("RequestWallParks: %v", err)
			}
			for _, r := range rows {
				if r.ID == id {
					return true
				}
			}
			return false
		}},
		{"ParkRunsAtWall", f.stale, func(id uuid.UUID) bool {
			rows, err := f.q.ParkRunsAtWall(f.ctx, store.ParkRunsAtWallParams{
				Now: fsaTS(tick), GlobalTimeoutSeconds: 86400, WorkerStaleCutoff: fsaTS(time.Now().Add(-time.Hour)), GraceSeconds: 600,
			})
			if err != nil {
				t.Fatalf("ParkRunsAtWall: %v", err)
			}
			for _, r := range rows {
				if r.ID == id {
					return true
				}
			}
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumed := f.startedRun(tc.worker, age, wall)
			control := f.startedRun(tc.worker, age, wall)
			first := f.read(resumed).firstStarted

			mustExec(f.ctx, t, f.pool, `UPDATE runs SET status='pool_wait' WHERE id=$1`, resumed)
			if n, err := f.q.PromotePoolWaitRun(f.ctx, store.PromotePoolWaitRunParams{ID: resumed, UserID: f.userID}); err != nil || n != 1 {
				t.Fatalf("PromotePoolWaitRun: n=%d err=%v", n, err)
			}
			mustExec(f.ctx, t, f.pool, `UPDATE runs SET status='claimed' WHERE id=$1`, resumed)
			f.setRunning(resumed, tc.worker)

			row := f.read(resumed)
			if row.status != "running" {
				t.Fatalf("status = %q, want running", row.status)
			}
			// Deadline from the row: new started_at + wall, never first_started_at + wall.
			dl := workersvc.RunDeadline(row.startedAt, row.wall, row.paused, row.kind, row.interactive, row.status,
				24*time.Hour, row.ext, row.finalizeSec)
			if dl == nil {
				t.Fatalf("RunDeadline = nil for a running timed run")
			}
			if want := row.startedAt.Time.Add(wall * time.Second); !dl.Equal(want) {
				t.Fatalf("RunDeadline = %v, want started_at+wall %v", dl, want)
			}
			if stale := first.Time.Add(wall * time.Second); !dl.After(stale) {
				t.Fatalf("RunDeadline %v is anchored on first_started_at (%v), want a fresh wall", dl, stale)
			}

			// One sweep tick: it must pick up the control and leave the resumed run alone.
			// The sweep runs once; its own result is only read for the control, and the resumed
			// run is checked through its row state below.
			controlHit := tc.sweep(control)
			after := f.read(resumed)
			if after.status != "running" {
				t.Fatalf("resumed run was parked by the wall sweep: status=%q", after.status)
			}
			var pauseMode pgtype.Text
			if err := f.pool.QueryRow(f.ctx, `SELECT pause_mode FROM runs WHERE id=$1`, resumed).Scan(&pauseMode); err != nil {
				t.Fatalf("read pause_mode: %v", err)
			}
			if pauseMode.Valid {
				t.Fatalf("resumed run carries a wall request (pause_mode=%q) after one sweep tick", pauseMode.String)
			}
			if !controlHit {
				t.Fatalf("control run (same backdate, never cycled) was not selected by %s: the test would be vacuous", tc.name)
			}
		})
	}
}

// TestRunFirstStartedAtBackfillLiveDB follows status_since_backfill_livedb_test.go: a throwaway
// database migrated to 279 is seeded with legacy rows, 00280 is applied, and the backfill is
// asserted. Started rows, parked ones included, seed from started_at; rows with no started_at
// (never started, or promoted and not yet running again) stay NULL; created_at is never read.
func TestRunFirstStartedAtBackfillLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	name := "first_started_bf_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	adminPool.Close()

	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupAdmin, err := store.OpenPool(ctx, dsn)
		if err != nil {
			t.Logf("cleanup: open admin pool to drop %s: %v", name, err)
			return
		}
		defer cleanupAdmin.Close()
		if _, err := cleanupAdmin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup: drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	newDSN := u.String()

	if err := store.MigrateTo(ctx, newDSN, 279); err != nil {
		t.Fatalf("MigrateTo(279): %v", err)
	}
	pool, err = store.OpenPool(ctx, newDSN)
	if err != nil {
		t.Fatalf("open work pool: %v", err)
	}
	var have int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='runs' AND column_name='first_started_at'`).Scan(&have); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if have != 0 {
		t.Fatalf("runs.first_started_at exists at v279; the test would be vacuous")
	}

	// Distinct created_at per row, none equal to any started_at, so a created_at-seeded
	// backfill cannot pass.
	type seed struct {
		tag     string
		status  string
		created time.Time
		started *time.Time
		fin     *time.Time
	}
	ts := func(h int) *time.Time { v := time.Date(2021, 3, 1, h, 0, 0, 0, time.UTC); return &v }
	seeds := []seed{
		{"finished", "completed", time.Date(2021, 1, 1, 1, 0, 0, 0, time.UTC), ts(2), ts(4)},
		{"running", "running", time.Date(2021, 1, 2, 1, 0, 0, 0, time.UTC), ts(6), nil},
		{"parked", "limit_wait", time.Date(2021, 1, 3, 1, 0, 0, 0, time.UTC), nil, nil},
		// A park keeps started_at (only promotion clears it), so a run parked at the upgrade
		// with its anchor still set seeds from it like any other started row.
		{"parked-started", "limit_wait", time.Date(2021, 1, 5, 1, 0, 0, 0, time.UTC), ts(8), nil},
		{"never", "queued", time.Date(2021, 1, 4, 1, 0, 0, 0, time.UTC), nil, nil},
	}
	ids := map[string]uuid.UUID{}
	for _, s := range seeds {
		id := repro106SeedRun(ctx, t, pool, "fsa-"+s.tag)
		ids[s.tag] = id
		mustExec(ctx, t, pool, `UPDATE runs SET status=$2, created_at=$3, started_at=$4, finished_at=$5 WHERE id=$1`,
			id, s.status, s.created, s.started, s.fin)
	}

	if err := store.MigrateTo(ctx, newDSN, 280); err != nil {
		t.Fatalf("MigrateTo(280): %v", err)
	}

	read := func(tag string) (first, started, created pgtype.Timestamptz) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT first_started_at, started_at, created_at FROM runs WHERE id=$1`, ids[tag]).
			Scan(&first, &started, &created); err != nil {
			t.Fatalf("read %s: %v", tag, err)
		}
		return
	}
	for _, s := range seeds {
		first, started, created := read(s.tag)
		if s.started != nil {
			if !first.Valid || !first.Time.Equal(*s.started) || !started.Time.Equal(*s.started) {
				t.Errorf("%s: first_started_at = %v, want seeded from started_at %v", s.tag, first, *s.started)
			}
		} else if first.Valid {
			t.Errorf("%s: first_started_at = %v, want NULL (no started_at to seed from)", s.tag, first.Time)
		}
		if first.Valid && first.Time.Equal(created.Time) {
			t.Errorf("%s: first_started_at equals created_at; the backfill must never read created_at", s.tag)
		}
	}

	// The seeded running row keeps its value across a heartbeat and a later reset + reclaim.
	q := store.New(pool)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM runs WHERE id=$1`, ids["running"]).Scan(&userID); err != nil {
		t.Fatalf("read user: %v", err)
	}
	w, err := q.CreateWorker(ctx, store.CreateWorkerParams{UserID: userID, Name: "fsa-bf", TokenHash: []byte("fsa-bf-" + userID.String()), AnthropicBindMode: "auto"})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	runID := ids["running"]
	mustExec(ctx, t, pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, runID, w.ID)
	report := func() {
		t.Helper()
		if n, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: runID, WorkerID: pgU(w.ID), IterationCount: 1}); err != nil || n != 1 {
			t.Fatalf("SetRunRunning: n=%d err=%v", n, err)
		}
	}
	want := *ts(6)
	report() // heartbeat
	if first, started, _ := read("running"); !first.Time.Equal(want) || !started.Time.Equal(want) {
		t.Fatalf("heartbeat moved an anchor: first=%v started=%v, want both %v", first.Time, started.Time, want)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET status='limit_wait', retry_not_before=$2 WHERE id=$1`, runID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	rows, err := q.PromoteLimitWaitRuns(ctx, fsaTS(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)))
	if err != nil || countID(rows, runID) != 1 {
		t.Fatalf("PromoteLimitWaitRuns: err=%v rows=%v", err, rows)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET status='claimed' WHERE id=$1`, runID)
	report()
	first, started, _ := read("running")
	if !first.Time.Equal(want) {
		t.Fatalf("first_started_at = %v after reset + reclaim, want %v", first.Time, want)
	}
	if !started.Valid || started.Time.Equal(want) || time.Since(started.Time) > time.Minute {
		t.Fatalf("started_at = %v after reset + reclaim, want a fresh stamp", started)
	}
}

func countID(rows []store.PromoteLimitWaitRunsRow, id uuid.UUID) int64 {
	var n int64
	for _, r := range rows {
		if r.ID == id {
			n++
		}
	}
	return n
}
