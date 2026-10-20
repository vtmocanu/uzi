package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// This executes the generated production statement, including its null sentinel,
// and checks only the tail's ordered index limit, not the rest of the plan shape.
func TestInterruptedCallsPlanLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; lead runs ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := OpenPool(ctx, dsn)
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
			t.Fatal(err)
		}
	}
	user, worker, repo, conn, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@interrupted.test")
	exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://example.test','bot',1,$3)", conn, user, []byte("x"))
	exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r','https://example.test/g/r','main',true)", repo, conn)
	exec("INSERT INTO workers(id,user_id,name,token_hash,snapshot_register_nonce) VALUES($1,$2,'worker',$3,'nonce')", worker, user, worker[:])
	exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,$3,'issue',1,'title','desc','running',$4,2)", run, user, repo, worker)
	q := New(tx)
	args := InterruptedCallsWindowParams{RunID: run, WorkerID: worker, OwnerID: user, RegisterNonce: "nonce", ClaimGeneration: 2}
	rows, err := q.InterruptedCallsWindow(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Seq.Valid || rows[0].SourceGeneration != 0 || rows[0].InspectedCount != 0 {
		t.Fatalf("empty sentinel: %+v", rows)
	}
	// SQL must omit whole oversized sources, never project a command/key prefix.
	for _, tc := range []struct {
		command     any
		id          any
		wantCommand string
		omitted     bool
	}{
		{strings.Repeat("x", 2048), strings.Repeat("i", 1024), strings.Repeat("x", 2048), false},
		{strings.Repeat("界", 2048), "id", strings.Repeat("界", 2048), false},
		{strings.Repeat("x", 2049), "id", "", true},
		{123, 123, "", false},
	} {
		payload, _ := json.Marshal(map[string]any{"id": tc.id, "name": "Bash", "input": map[string]any{"command": tc.command}})
		exec("INSERT INTO run_messages(run_id,seq,kind,agent,agent_label,agent_instance,payload,claim_generation) VALUES($1,1,'tool_use',$2,$2,$3,$4,1)", run, strings.Repeat("a", 129), strings.Repeat("b", 128), payload)
		got, err := q.InterruptedCallsWindow(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Command != tc.wantCommand || got[0].CommandOmitted != tc.omitted || got[0].Agent != "" || got[0].AgentLabel != "" || got[0].AgentInstance != "" || got[0].NullLane {
			t.Fatalf("bounded projection: %+v", got)
		}
		if id, ok := tc.id.(string); ok {
			if got[0].CallID != id {
				t.Fatal("full key changed")
			}
		} else if got[0].CallID != "" {
			t.Fatal("numeric key accepted")
		}
		exec("DELETE FROM run_messages WHERE run_id=$1", run)
	}
	exec(`INSERT INTO run_messages(run_id,seq,kind,agent,payload,claim_generation)
 SELECT $1,s,'text','lead',jsonb_build_object('text', repeat(md5(s::text), 128)),2 FROM generate_series(1,32768) s`, run)
	// Bulk-inserted fixtures need statistics before measuring default planner choices.
	exec("ANALYZE run_messages")
	rows, err = q.InterruptedCallsWindow(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Seq.Valid || rows[0].InspectedCount != 256 {
		t.Fatalf("window: %+v", rows)
	}
	exec("PREPARE interrupted_plan(uuid,uuid,uuid,text,bigint) AS " + interruptedCallsWindow)
	for _, scenario := range []string{"displaced", "sparse"} {
		if scenario == "sparse" {
			exec(`UPDATE run_messages SET kind='tool_use', claim_generation=1,
 payload='{"id":"sparse-call","name":"Bash","input":{"command":"echo sparse"}}'::jsonb
 WHERE run_id=$1 AND seq=32760`, run)
			exec("ANALYZE run_messages")
			got, err := q.InterruptedCallsWindow(ctx, args)
			if err != nil || len(got) != 1 || !got[0].Seq.Valid || got[0].Seq.Int32 != 32760 || got[0].SourceGeneration != 1 || got[0].InspectedCount != 256 {
				t.Fatalf("sparse selection: %+v, %v", got, err)
			}
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(scenario+"/"+mode, func(t *testing.T) {
				exec("SET LOCAL plan_cache_mode = " + mode)
				var raw []byte
				sql := fmt.Sprintf("EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE interrupted_plan('%s','%s','%s','nonce',2)", run, worker, user)
				if err := tx.QueryRow(ctx, sql).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plans []struct {
					Plan          map[string]any
					ExecutionTime float64 `json:"Execution Time"`
					PlanningTime  float64 `json:"Planning Time"`
				}
				if err := json.Unmarshal(raw, &plans); err != nil {
					t.Fatal(err)
				}
				found := false
				var walk func(map[string]any)
				walk = func(node map[string]any) {
					if node["Subplan Name"] == "CTE tail" {
						found = true
						if node["Node Type"] != "Limit" || node["Actual Rows"] != float64(256) {
							t.Fatalf("tail is not bounded: %s", raw)
						}
						indexed := false
						var inspect func(map[string]any)
						inspect = func(n map[string]any) {
							if n["Node Type"] == "Sort" {
								t.Fatalf("tail sorts history: %s", raw)
							}
							if strings.Contains(fmt.Sprint(n["Node Type"]), "Index") && n["Scan Direction"] == "Backward" && strings.Contains(fmt.Sprint(n["Index Cond"]), "run_id") {
								indexed = true
							}
							if filter, ok := n["Filter"].(string); ok && (strings.Contains(filter, "kind") || strings.Contains(filter, "claim_generation")) {
								t.Fatalf("filtered before limit: %s", raw)
							}
							if children, ok := n["Plans"].([]any); ok {
								for _, child := range children {
									inspect(child.(map[string]any))
								}
							}
						}
						inspect(node)
						if !indexed {
							t.Fatalf("tail has no backwards run index scan: %s", raw)
						}
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, child := range children {
							walk(child.(map[string]any))
						}
					}
				}
				walk(plans[0].Plan)
				if !found {
					t.Fatalf("no materialized tail: %s", raw)
				}
				t.Logf("PostgreSQL prepared %s/%s: 32768 history rows, 4096-byte text sources, fresh ANALYZE, tail=256; planning=%.3fms execution=%.3fms; disposable local fixture, no concurrent load or production latency guarantee", scenario, mode, plans[0].PlanningTime, plans[0].ExecutionTime)
			})
		}
	}
	exec("DEALLOCATE interrupted_plan")
}
