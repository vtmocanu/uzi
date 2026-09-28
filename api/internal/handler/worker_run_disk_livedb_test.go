package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1809 M6 (D8) — the per-run disk persistence acceptance test, through the real worker
// heartbeat route and a real Postgres: a heartbeat carrying run_disk writes worker_run_disk and
// the data-volume inode pair; a later heartbeat REPLACES the set (a run no longer listed is
// deleted, a listed one updated); a heartbeat without run_disk leaves the rows untouched; an empty
// list clears them; a run id of another owner is dropped; the worker DTO overlay and the run DTO
// size overlay read the rows back (the run DTO only from the run's current worker); sampled_at is
// the reported measurement time, clamped, and the freshness window reads it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one.
func TestWorkerRunDiskReplaceLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		wsvc:      workersvc.New(q, box, workersvc.Params{}),
		version:   "dev",
		now:       time.Now,
		startedAt: time.Now(),
	}
	srv := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	owner, other := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, other} {
		mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("rundisk-%s@e2e", u.String()[:8]))
	}
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("generate join token: %v", err)
	}
	workerID := uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'rundisk-worker', $3, 'online')`,
		workerID, owner, hash)

	// An issue run needs a repo (the runs_kind_shape CHECK): one forge connection + repo per owner.
	repoFor := map[uuid.UUID]uuid.UUID{}
	for _, u := range []uuid.UUID{owner, other} {
		connID, repoID := uuid.New(), uuid.New()
		mustExecT(ctx, t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		          VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, u, []byte("x"))
		mustExecT(ctx, t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		          VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String())
		repoFor[u] = repoID
	}
	iid := int64(981000)
	seedRun := func(user uuid.UUID) uuid.UUID {
		id := uuid.New()
		iid++
		mustExecT(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5)`, id, user, repoFor[user], iid, workerID)
		return id
	}
	runA, runB, runC := seedRun(owner), seedRun(owner), seedRun(owner)
	foreign := seedRun(other)

	heartbeat := func(extra string) {
		t.Helper()
		body := `{"version":"1","stats":{"mem_bytes":100,"source":"cgroup"` + extra + `}}`
		req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
	}
	type row struct {
		home, cache int64
		truncated   bool
	}
	readRows := func() map[uuid.UUID]row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT run_id, home_bytes, cache_bytes, truncated FROM worker_run_disk WHERE worker_id = $1`, workerID)
		if err != nil {
			t.Fatalf("read worker_run_disk: %v", err)
		}
		defer rows.Close()
		out := map[uuid.UUID]row{}
		for rows.Next() {
			var id uuid.UUID
			var r row
			if err := rows.Scan(&id, &r.home, &r.cache, &r.truncated); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = r
		}
		return out
	}

	// (1) First report: A, B and a foreign-owner run; plus the data-volume inode pair.
	heartbeat(fmt.Sprintf(`,"disk_data_inodes":7001,"disk_data_total_inodes":65536,"run_disk":[`+
		`{"run_id":%q,"home_bytes":5000,"cache_bytes":3000,"truncated":true},`+
		`{"run_id":%q,"home_bytes":900,"cache_bytes":100},`+
		`{"run_id":%q,"home_bytes":99999,"cache_bytes":1}]`, runA, runB, foreign))
	got := readRows()
	if len(got) != 2 || got[runA] != (row{5000, 3000, true}) || got[runB] != (row{900, 100, false}) {
		t.Fatalf("after the first report rows = %+v, want A 5000/3000/truncated and B 900/100 (foreign dropped)", got)
	}
	var inodes, totalInodes *int64
	if err := pool.QueryRow(ctx, `SELECT stats_disk_data_inodes, stats_disk_data_total_inodes FROM workers WHERE id = $1`, workerID).Scan(&inodes, &totalInodes); err != nil {
		t.Fatalf("read inodes: %v", err)
	}
	if inodes == nil || *inodes != 7001 || totalInodes == nil || *totalInodes != 65536 {
		t.Fatalf("stored inodes = %v/%v, want 7001/65536", inodes, totalInodes)
	}

	// The worker DTO overlay: largest first.
	byWorker := h.runDiskByWorker(ctx, []uuid.UUID{workerID})
	list := byWorker[workerID]
	if len(list) != 2 || list[0].RunID != runA.String() || list[0].HomeBytes != 5000 || !list[0].Truncated || list[1].RunID != runB.String() {
		t.Fatalf("run_disk overlay = %+v, want [A 5000 truncated, B 900]", list)
	}

	// (2) Replacement: B dropped, A updated, C added.
	heartbeat(fmt.Sprintf(`,"run_disk":[{"run_id":%q,"home_bytes":6000,"cache_bytes":0},{"run_id":%q,"home_bytes":10,"cache_bytes":5}]`, runA, runC))
	got = readRows()
	if len(got) != 2 || got[runA] != (row{6000, 0, false}) || got[runC] != (row{10, 5, false}) {
		t.Fatalf("after the replacing report rows = %+v, want A 6000/0 and C 10/5 (B deleted)", got)
	}

	// (3) A heartbeat without run_disk leaves the rows untouched.
	heartbeat("")
	if got = readRows(); len(got) != 2 {
		t.Fatalf("a heartbeat without run_disk changed the rows: %+v", got)
	}

	// The run DTO overlay reads the current worker's fresh row.
	var runRow store.Run
	runRow.ID = runA
	runRow.WorkerID.Bytes, runRow.WorkerID.Valid = workerID, true
	var dto apitypes.RunDTO
	h.overlayRunDiskSize(ctx, runRow, &dto)
	if dto.HomeBytes == nil || *dto.HomeBytes != 6000 || dto.CacheBytes == nil || *dto.CacheBytes != 0 || dto.DiskTruncated {
		t.Fatalf("run DTO sizes = %v/%v truncated=%v, want 6000/0", dto.HomeBytes, dto.CacheBytes, dto.DiskTruncated)
	}
	// Only the run's CURRENT worker's row is shown: a run bound to another worker (which has not
	// reported it) or to no worker gets no size, never this worker's row as a fallback.
	for name, wid := range map[string]*uuid.UUID{"another worker": func() *uuid.UUID { u := uuid.New(); return &u }(), "no worker": nil} {
		r := store.Run{ID: runA}
		if wid != nil {
			r.WorkerID.Bytes, r.WorkerID.Valid = *wid, true
		}
		var other apitypes.RunDTO
		h.overlayRunDiskSize(ctx, r, &other)
		if other.HomeBytes != nil || other.CacheBytes != nil {
			t.Fatalf("%s: run DTO sizes = %v/%v, want absent (no substitution from another worker's row)", name, other.HomeBytes, other.CacheBytes)
		}
	}
	// A stale row (older than the 15-minute window) is not shown.
	mustExecT(ctx, t, pool, `UPDATE worker_run_disk SET sampled_at = now() - interval '20 minutes' WHERE run_id = $1`, runA)
	var stale apitypes.RunDTO
	h.overlayRunDiskSize(ctx, runRow, &stale)
	if stale.HomeBytes != nil || stale.CacheBytes != nil {
		t.Fatalf("a stale row surfaced on the run DTO: %v/%v", stale.HomeBytes, stale.CacheBytes)
	}

	// (4) An empty list is a real report and clears the worker's rows.
	heartbeat(`,"run_disk":[]`)
	if got = readRows(); len(got) != 0 {
		t.Fatalf("an empty run_disk left rows: %+v", got)
	}

	// (5) Deleting a run cascades its rows.
	heartbeat(fmt.Sprintf(`,"run_disk":[{"run_id":%q,"home_bytes":1,"cache_bytes":1}]`, runC))
	mustExecT(ctx, t, pool, `DELETE FROM runs WHERE id = $1`, runC)
	if got = readRows(); len(got) != 0 {
		t.Fatalf("deleting the run left its size row: %+v", got)
	}

	// (6) sampled_at is the worker's MEASUREMENT time, clamped to [now-24h, now+5m] on the database
	// clock, and missing means now(). The freshness window reads it: a report measured 20 minutes
	// ago is stored but not shown, even though the heartbeat carrying it just arrived.
	runD, runE, runF, runG, runH := seedRun(owner), seedRun(owner), seedRun(owner), seedRun(owner), seedRun(owner)
	now := time.Now().UTC()
	threeMin := now.Add(-3 * time.Minute).Truncate(time.Second)
	heartbeat(fmt.Sprintf(`,"run_disk":[`+
		`{"run_id":%q,"home_bytes":10,"cache_bytes":0,"sampled_at":%q},`+
		`{"run_id":%q,"home_bytes":20,"cache_bytes":0,"sampled_at":%q},`+
		`{"run_id":%q,"home_bytes":30,"cache_bytes":0,"sampled_at":%q},`+
		`{"run_id":%q,"home_bytes":40,"cache_bytes":0,"sampled_at":%q},`+
		`{"run_id":%q,"home_bytes":50,"cache_bytes":0}]`,
		runD, threeMin.Format(time.RFC3339),
		runE, now.Add(-20*time.Minute).Format(time.RFC3339),
		runF, now.Add(-72*time.Hour).Format(time.RFC3339),
		runG, now.Add(2*time.Hour).Format(time.RFC3339),
		runH))
	// Offsets from the database clock, in seconds, so the assertions are immune to host/DB skew.
	offsets := map[uuid.UUID]float64{}
	rows, err := pool.Query(ctx, `SELECT run_id, EXTRACT(EPOCH FROM sampled_at - now())::float8 FROM worker_run_disk WHERE worker_id = $1`, workerID)
	if err != nil {
		t.Fatalf("read sampled_at: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var off float64
		if err := rows.Scan(&id, &off); err != nil {
			t.Fatalf("scan sampled_at: %v", err)
		}
		offsets[id] = off
	}
	rows.Close()
	var dbSkew float64 // host now - db now, seconds
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM $1::timestamptz - now())::float8`, now).Scan(&dbSkew); err != nil {
		t.Fatalf("read db clock: %v", err)
	}
	within := func(name string, got, want, tol float64) {
		t.Helper()
		if got < want-tol || got > want+tol {
			t.Fatalf("%s: sampled_at - now() = %.1fs, want %.1fs (+/- %.0fs); all offsets %v", name, got, want, tol, offsets)
		}
	}
	within("reported 3 minutes ago (kept as measured)", offsets[runD], dbSkew-180, 5)
	within("reported 20 minutes ago (kept as measured)", offsets[runE], dbSkew-1200, 5)
	within("reported 3 days ago (clamped to 24h)", offsets[runF], -86400, 5)
	within("reported 2 hours ahead (clamped to 5m)", offsets[runG], 300, 5)
	within("not reported (now)", offsets[runH], 0, 5)
	// The overlay drops the 20-minute-old and the clamped 24h-old measurements.
	shown := map[string]bool{}
	for _, e := range h.runDiskByWorker(ctx, []uuid.UUID{workerID})[workerID] {
		shown[e.RunID] = true
	}
	if !shown[runD.String()] || !shown[runG.String()] || !shown[runH.String()] || shown[runE.String()] || shown[runF.String()] {
		t.Fatalf("fresh overlay = %v, want D, G, H shown and E (20m old), F (clamped 24h) hidden", shown)
	}
	stale = apitypes.RunDTO{}
	h.overlayRunDiskSize(ctx, store.Run{ID: runE, WorkerID: runRow.WorkerID}, &stale)
	if stale.HomeBytes != nil {
		t.Fatalf("a run measured 20 minutes ago surfaced on the run DTO: %v", *stale.HomeBytes)
	}
}
