package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var errCrossRunPlanCaptured = errors.New("cross-run generated statement captured")

type crossRunPlanCapture struct {
	store.DBTX
	sql   string
	args  []any
	calls int
}

func (c *crossRunPlanCapture) record(sql string, args []any) {
	c.sql = sql
	c.args = append([]any(nil), args...)
	c.calls++
}
func (c *crossRunPlanCapture) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.record(sql, args)
	return nil, errCrossRunPlanCaptured
}
func (c *crossRunPlanCapture) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	c.record(sql, args)
	return crossRunPlanRow{}
}

type crossRunPlanRow struct{}

func (crossRunPlanRow) Scan(...any) error { return errCrossRunPlanCaptured }

type crossRunPlanNode struct {
	Type        string             `json:"Node Type"`
	Relation    string             `json:"Relation Name"`
	Index       string             `json:"Index Name"`
	IndexCond   string             `json:"Index Cond"`
	TempReads   float64            `json:"Temp Read Blocks"`
	TempWrites  float64            `json:"Temp Written Blocks"`
	Rows        float64            `json:"Actual Rows"`
	Loops       float64            `json:"Actual Loops"`
	Removed     float64            `json:"Rows Removed by Filter"`
	JoinRemoved float64            `json:"Rows Removed by Join Filter"`
	Hits        float64            `json:"Shared Hit Blocks"`
	Reads       float64            `json:"Shared Read Blocks"`
	Plans       []crossRunPlanNode `json:"Plans"`
}

func crossRunPlanLiteral(t *testing.T, arg any) string {
	t.Helper()
	switch v := arg.(type) {
	case uuid.UUID:
		return "'" + v.String() + "'::uuid"
	case pgtype.UUID:
		if !v.Valid {
			return "NULL::uuid"
		}
		return "'" + uuid.UUID(v.Bytes).String() + "'::uuid"
	case pgtype.Int8:
		if !v.Valid {
			return "NULL::bigint"
		}
		return fmt.Sprintf("%d::bigint", v.Int64)
	case pgtype.Timestamptz:
		if !v.Valid {
			return "NULL::timestamptz"
		}
		if v.InfinityModifier != pgtype.Finite {
			t.Fatal("nonfinite plan argument")
		}
		return "'" + v.Time.UTC().Format(time.RFC3339Nano) + "'::timestamptz"
	default:
		t.Fatalf("unsupported captured argument %T", arg)
		return ""
	}
}
func crossRunExplain(t *testing.T, e *mergedPollEnv, label, mode string, c *crossRunPlanCapture) {
	t.Helper()
	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	name := "cross_run_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// PREPARE is session scoped even when its transaction rolls back.
	if mode != "ordinary" {
		defer func() {
			if _, err := conn.Exec(e.ctx, "DEALLOCATE "+name); err != nil {
				t.Errorf("deallocate: %v", err)
			}
		}()
	}
	tx, err := conn.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if mode == "ordinary" {
		t.Logf("%s exact generated SQL:\n%s\nargs=%v", label, c.sql, c.args)
	}
	statement := "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) " + c.sql
	args := c.args
	if mode != "ordinary" {
		if _, err := tx.Exec(e.ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.ctx, "PREPARE "+name+" AS "+c.sql); err != nil {
			t.Fatalf("%s PREPARE: %v", label, err)
		}
		values := make([]string, len(args))
		for i, arg := range args {
			values[i] = crossRunPlanLiteral(t, arg)
		}
		statement = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE " + name + "(" + strings.Join(values, ",") + ")"
		args = nil
	}
	var raw []byte
	if err := tx.QueryRow(e.ctx, statement, args...).Scan(&raw); err != nil {
		t.Fatalf("%s/%s EXPLAIN: %v", label, mode, err)
	}
	var result []struct {
		Plan      crossRunPlanNode `json:"Plan"`
		Execution float64          `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result) != 1 {
		t.Fatalf("plan decode count=%d err=%v", len(result), err)
	}
	root := result[0].Plan
	if root.Rows > 1 || root.Loops != 1 {
		t.Fatalf("%s/%s fetched rows=%g loops=%g, want <=1 row once", label, mode, root.Rows, root.Loops)
	}
	lockNodes := 0
	var walk func(crossRunPlanNode)
	walk = func(n crossRunPlanNode) {
		if n.Type == "LockRows" {
			lockNodes++
		}
		if n.Relation != "" || n.Type == "Sort" || n.Type == "Incremental Sort" {
			t.Logf("%s/%s node=%s relation=%s index=%s rows=%g loops=%g examined_output=%g removed_filter=%g removed_join=%g shared_hit=%g shared_read=%g",
				label, mode, n.Type, n.Relation, n.Index, n.Rows, n.Loops, n.Rows*n.Loops, n.Removed*n.Loops, n.JoinRemoved*n.Loops, n.Hits, n.Reads)
			if n.IndexCond != "" {
				t.Logf("%s/%s index_condition=%s", label, mode, n.IndexCond)
			}
		}
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(root)
	if lockNodes != 0 {
		t.Fatalf("%s unexpectedly locks rows during enumeration", label)
	}
	t.Logf("%s/%s fetched=%g lock_nodes=%d root_shared_hit=%g root_shared_read=%g root_temp_read=%g root_temp_write=%g execution_ms=%g; scan work logged independently, no scan-cost bound asserted", label, mode, root.Rows, lockNodes, root.Hits, root.Reads, root.TempReads, root.TempWrites, result[0].Execution)
}

func TestCrossRunDiscardQueryPlanLiveDB(t *testing.T) {
	e, w := newCrossRunDiscardEnv(t)
	// Finite, tied hot-issue history with an admitted malformed-inventory
	// prefix. No production index, trigger or planner switch is changed.
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,created_at,finished_at)
		SELECT md5($1::text||'/hot/'||n)::uuid,$2,$3,'issue',101,'hot','d','cancelled',1,
		'2020-01-01'::timestamptz + (n/4)*interval '1 second','2020-01-02' FROM generate_series(1,256) n`, e.run, e.worker.UserID, e.repo)
	e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
		SELECT md5($1::text||'/hot-hold/'||n)::uuid,$2,$3,md5($1::text||'/hot/'||n)::uuid,
		CASE WHEN n>128 THEN 2 ELSE 1 END,$4,'legacy',$4,md5($1::text||'/hot/'||n)::uuid,false,'open'
		FROM generate_series(1,256) n`, e.run, e.worker.UserID, e.repo, e.worker.ID)
	e.exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,enabled)
		SELECT md5($1::text||'/repo/'||n)::uuid,$2,10000+n,md5($1::text||'/repo/'||n),'https://forge.test/bg',true FROM generate_series(1,16) n`, e.run, e.conn)
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,created_at,finished_at)
		SELECT md5($1::text||'/bg/'||n)::uuid,$2,md5($1::text||'/repo/'||(1+(n-1)%16))::uuid,
		'issue',800+n,'background','d','cancelled',1,'2019-01-01'::timestamptz+n*interval '1 second','2019-01-02'
		FROM generate_series(1,4096) n`, e.run, e.worker.UserID)
	e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state)
		SELECT md5($1::text||'/bg-hold/'||n)::uuid,$2,md5($1::text||'/repo/'||(1+(n-1)%16))::uuid,
		md5($1::text||'/bg/'||n)::uuid,1,$3,'original',$3,md5($1::text||'/bg/'||n)::uuid,true,'open'
		FROM generate_series(1,1024) n`, e.run, e.worker.UserID, e.worker.ID)
	for _, table := range []string{"runs", "repos", "recovery_custody_holds", "recovery_captures", "run_held_publications", "worker_active_runs"} {
		e.exec("ANALYZE " + table)
	}
	var version string
	if err := e.pool.QueryRow(e.ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("database=%s fixture: 256 hot rows (four-way timestamp ties,128 malformed holds),4096 background rows/16 repos,1024 unrelated holds; recovery lock inventory is not measured by these read plans", version)
	// Get independent expected ordering from the fixture's immutable keys,
	// then compare each real LIMIT-1 page, reporting only the first mismatch.
	rows, err := e.pool.Query(e.ctx, `SELECT id,created_at FROM runs WHERE repo_id=$1 AND status='cancelled' ORDER BY created_at DESC,id DESC`, e.repo)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	var times []pgtype.Timestamptz
	for rows.Next() {
		var id uuid.UUID
		var at pgtype.Timestamptz
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		times = append(times, at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 257 {
		t.Fatalf("hot target history=%d want 257", len(ids))
	}
	p := store.ListCrossRunTargetsParams{RepoID: pgtype.UUID{Bytes: e.repo, Valid: true}, OwnerID: e.worker.UserID,
		IssueIid: pgtype.Int8{Int64: 101, Valid: true}, WitnessRunID: w, WitnessCreatedAt: pgtype.Timestamptz{Time: time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC), Valid: true}}
	for i, id := range ids {
		got, err := e.q.ListCrossRunTargets(e.ctx, p)
		if err != nil || len(got) != 1 {
			t.Fatalf("target page %d rows=%d err=%v", i, len(got), err)
		}
		if got[0].RunID != id {
			t.Fatalf("first target mismatch page %d: got %s want %s", i, got[0].RunID, id)
		}
		p.AfterRunID = id
		p.AfterCreatedAt = times[i]
	}
	if got, err := e.q.ListCrossRunTargets(e.ctx, p); err != nil || len(got) != 0 {
		t.Fatalf("exhausted targets=%d err=%v", len(got), err)
	}
	for _, key := range []struct {
		name  string
		index int
	}{{"NULL", -1}, {"tied", 1}, {"deep", 250}, {"exhausted", 256}} {
		p.AfterCreatedAt = pgtype.Timestamptz{}
		p.AfterRunID = uuid.Nil
		if key.index >= 0 {
			p.AfterCreatedAt = times[key.index]
			p.AfterRunID = ids[key.index]
		}
		c := &crossRunPlanCapture{}
		_, err := store.New(c).ListCrossRunTargets(e.ctx, p)
		if !errors.Is(err, errCrossRunPlanCaptured) || c.calls != 1 {
			t.Fatalf("target capture calls=%d err=%v", c.calls, err)
		}
		for _, mode := range []string{"ordinary", "force_custom_plan", "force_generic_plan"} {
			crossRunExplain(t, e, "ListCrossRunTargets/"+key.name, mode, c)
		}
	}
	for _, after := range []uuid.UUID{uuid.Nil, w} {
		c := &crossRunPlanCapture{}
		_, err := store.New(c).ListMergedPredecessorCandidates(e.ctx, store.ListMergedPredecessorCandidatesParams{RepoID: e.repo, AfterRunID: after})
		if !errors.Is(err, errCrossRunPlanCaptured) || c.calls != 1 {
			t.Fatalf("candidate capture calls=%d err=%v", c.calls, err)
		}
		for _, mode := range []string{"ordinary", "force_custom_plan", "force_generic_plan"} {
			crossRunExplain(t, e, "ListMergedPredecessorCandidates/after="+after.String(), mode, c)
		}
	}
	for _, name := range []string{"HasCrossRunLiveConsumer", "HasCrossRunSharedFinal"} {
		c := &crossRunPlanCapture{}
		q := store.New(c)
		var err error
		if name == "HasCrossRunLiveConsumer" {
			_, err = q.HasCrossRunLiveConsumer(e.ctx, store.HasCrossRunLiveConsumerParams{OwnerID: e.worker.UserID, RepoID: p.RepoID, IssueIid: p.IssueIid})
		} else {
			_, err = q.HasCrossRunSharedFinal(e.ctx, e.run)
		}
		if !errors.Is(err, errCrossRunPlanCaptured) || c.calls != 1 {
			t.Fatalf("%s capture calls=%d err=%v", name, c.calls, err)
		}
		for _, mode := range []string{"ordinary", "force_custom_plan", "force_generic_plan"} {
			crossRunExplain(t, e, name, mode, c)
		}
	}
}
