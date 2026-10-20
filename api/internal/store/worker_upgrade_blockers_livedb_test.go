package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// blockerQueryCapture forwards the generated query to PostgreSQL and remembers
// that exact SQL for EXPLAIN, avoiding a second hand-maintained query.
type blockerQueryCapture struct {
	pgx.Tx
	sql string
}

func (c *blockerQueryCapture) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = sql
	return c.Tx.Query(ctx, sql, args...)
}

func TestWorkerUpgradeBlockersProjectionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	owner, foreign, conn, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, foreign} {
		exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", u, u.String()+"@blockers.test")
	}
	exec("INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://forge.test','bot',1,$3)", conn, owner, []byte{1})
	exec("INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES ($1,$2,1,'g/r','https://forge.test/g/r','main')", repo, conn)
	worker := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec("INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,'blocker-fixture',$3,'online')", id, owner, id[:])
		return id
	}
	seed := func(id, u uuid.UUID, w any, status, kind string) {
		t.Helper()
		var r, iid, branch, pipeline, ref, target, job any
		harness := "claude"
		report := false
		var budget any
		switch kind {
		case "issue", "self_improve":
			r = repo
			iid = int64(1)
		case "ci_fix":
			r = repo
			pipeline = int64(1)
			ref = "agent/" + id.String()
		case "prompt":
			r = repo
		case "task":
			r = repo
			branch = "agent/" + id.String()
		case "mr_rework":
			r = repo
			ref = "agent/" + id.String()
			target = id
		case "judge":
			target = id
		case "job":
			job = "research"
		case "cross_check":
			r = repo
			target = id
			harness = "codex"
			report = true
			budget = int32(600)
		}
		exec(`INSERT INTO runs (id,user_id,worker_id,status,kind,issue_title,issue_description,repo_id,issue_iid,branch,pipeline_id,pipeline_ref,target_run_id,mr_iid,job_type,harness,report_only,budget_wall_seconds,claim_released_at)
			VALUES ($1,$2,$3,$4,$5,'must not expose title','must not expose prompt',$6,$7,$8,$9,$10,$11,1,$12,$13,$14,$15,now())`,
			id, u, w, status, kind, r, iid, branch, pipeline, ref, target, job, harness, report, budget)
	}
	capture := &blockerQueryCapture{Tx: tx}
	q := store.New(capture)
	statuses := []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "paused", "pool_wait", "limit_wait", "recovery_wait"}
	kinds := []string{"issue", "ci_fix", "chat", "judge", "self_improve", "prompt", "task", "mr_rework", "job", "cross_check"}
	var ids []uuid.UUID
	expected := map[uuid.UUID]uuid.UUID{}
	statusesByWorker := map[uuid.UUID]string{}
	kindsByWorker := map[uuid.UUID]string{}
	for i, status := range statuses {
		w, id := worker(), uuid.New()
		ids = append(ids, w)
		expected[w] = id
		statusesByWorker[w] = status
		kindsByWorker[w] = kinds[i]
		seed(id, owner, w, status, kinds[i])
	}
	exec("UPDATE runs SET cross_check_lane=true WHERE user_id=$1 AND kind='cross_check'", owner)
	rows, err := q.ListWorkerUpgradeBlockers(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("all statuses and lanes: %+v", rows)
	}
	for _, r := range rows {
		if !r.RunID.Valid || uuid.UUID(r.RunID.Bytes) != expected[r.WorkerID] || r.SafeCount != 1 || r.OwnerMismatch || r.WorkerOwner != owner || r.Status.String != statusesByWorker[r.WorkerID] || r.Kind.String != kindsByWorker[r.WorkerID] {
			t.Fatalf("%+v", r)
		}
	}
	// Top five must use UUID order and the entire safe population's count.
	top, empty, mixed, foreignOnly, other := worker(), worker(), worker(), worker(), worker()
	var runIDs []string
	for i := 0; i < 9; i++ {
		id := uuid.New()
		runIDs = append(runIDs, id.String())
		seed(id, owner, top, "queued", "chat")
	}
	sort.Strings(runIDs)
	for _, status := range []string{"completed", "failed", "cancelled"} {
		seed(uuid.New(), owner, top, status, "chat")
	}
	seed(uuid.New(), owner, nil, "running", "chat")
	seed(uuid.New(), owner, other, "running", "chat")
	foreignRun := uuid.New()
	seed(foreignRun, foreign, foreignOnly, "awaiting_input", "chat")
	seed(uuid.New(), foreign, mixed, "running", "chat")
	safeRun := uuid.New()
	seed(safeRun, owner, mixed, "paused", "chat")
	batch := []uuid.UUID{top, empty, mixed, foreignOnly}
	rows, err = q.ListWorkerUpgradeBlockers(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("top5 and sentinels: %+v", rows)
	}
	n := 0
	last := ""
	for _, r := range rows {
		key := r.WorkerID.String() + "/" + uuid.UUID(r.RunID.Bytes).String()
		if key < last {
			t.Fatalf("non-deterministic order: %s < %s", key, last)
		}
		last = key
		switch r.WorkerID {
		case top:
			if r.SafeCount != 9 || uuid.UUID(r.RunID.Bytes).String() != runIDs[n] || r.OwnerMismatch {
				t.Fatalf("%+v", r)
			}
			n++
		case empty:
			if r.RunID.Valid || r.SafeCount != 0 || r.OwnerMismatch {
				t.Fatalf("%+v", r)
			}
		case mixed:
			if !r.OwnerMismatch || r.SafeCount != 1 || uuid.UUID(r.RunID.Bytes) != safeRun {
				t.Fatalf("%+v", r)
			}
		case foreignOnly:
			if !r.OwnerMismatch || r.RunID.Valid || r.SafeCount != 0 || r.Status.Valid || r.HealthReason.Valid {
				t.Fatalf("foreign disclosure: %+v", r)
			}
		default:
			t.Fatalf("unauthorized worker %s", r.WorkerID)
		}
	}
	if n != 5 {
		t.Fatal("top5 missing")
	}
	if rows, err := q.ListWorkerUpgradeBlockers(ctx, nil); err != nil || len(rows) != 0 {
		t.Fatalf("empty projection: %+v %v", rows, err)
	}

	// Disposable plan fixture: a 64-worker history population, 8,000 terminal history
	// rows (90% assigned to one worker), and the nonterminal population above.
	// All fixture rows and ANALYZE changes are rolled back. No index is proposed
	// without this measurement on the existing runs(worker_id) index.
	historyWorkers := []uuid.UUID{top}
	for i := 0; i < 63; i++ {
		historyWorkers = append(historyWorkers, worker())
	}
	exec(`INSERT INTO runs (id,user_id,worker_id,status,kind,issue_title,issue_description)
		SELECT gen_random_uuid(),$1,CASE WHEN n<=7200 THEN $2::uuid ELSE ($3::uuid[])[2+(n%63)] END,'completed','chat','history','history'
		FROM generate_series(1,8000) n`, owner, top, historyWorkers)
	exec("ANALYZE workers")
	exec("ANALYZE runs")
	if _, err = q.ListWorkerUpgradeBlockers(ctx, batch); err != nil {
		t.Fatal(err)
	}
	plan, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+capture.sql, batch)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var lines []string
	for plan.Next() {
		var line string
		if err := plan.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("workers: %d fixture; terminal history: 8000, 90%% skew; fresh ANALYZE; authorized batch: 4\n%s", 10+5+63, strings.Join(lines, "\n"))
	if len(lines) == 0 {
		t.Fatal("EXPLAIN returned no plan")
	}
}
