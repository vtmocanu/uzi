package store

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestListPlanRevisionStateForRunsLiveDB executes the generated query against
// noisy histories (issue #2041) and asserts that its matching partial index is selected.
func TestListPlanRevisionStateForRunsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("plan-index-%s@example.com", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext) VALUES ($1, $2, 'github', 'https://example.com', 'bot', 1, $3)`, connID, userID, []byte{1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled) VALUES ($1, $2, 1, 'g/plan-index', 'https://example.com/g/plan-index', 'main', true)`, repoID, connID)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	for n, id := range ids {
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running')`, id, userID, repoID, int64(n+1))
		exec(`INSERT INTO run_messages (run_id, seq, kind, payload) SELECT $1, s, 'text', '{}'::jsonb FROM generate_series(1, 3000) s`, id)
	}
	// A ends revising, B ends with a revised plan, C has no plan frames.
	// D has a plan but is outside the requested page.
	want := []ListPlanRevisionStateForRunsRow{
		{RunID: ids[0], Seq: 10, Kind: "plan"},
		{RunID: ids[0], Seq: 20, Kind: "plan_revising"},
		{RunID: ids[1], Seq: 30, Kind: "plan_revising"},
		{RunID: ids[1], Seq: 40, Kind: "plan"},
	}
	for _, row := range want {
		exec(`UPDATE run_messages SET kind = $3 WHERE run_id = $1 AND seq = $2`, row.RunID, row.Seq, row.Kind)
	}
	exec(`UPDATE run_messages SET kind = 'plan' WHERE run_id = $1 AND seq = 50`, ids[3])
	sort.Slice(want, func(i, j int) bool {
		if want[i].RunID == want[j].RunID {
			return want[i].Seq < want[j].Seq
		}
		return want[i].RunID.String() < want[j].RunID.String()
	})
	got, err := New(pool).ListPlanRevisionStateForRuns(ctx, ids[:3])
	if err != nil {
		t.Fatalf("ListPlanRevisionStateForRuns: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan frames: got %+v, want %+v", got, want)
	}
	exec(`ANALYZE run_messages`)
	// Like run_activity_livedb_test.go, disable sequential scans to prove the
	// index path exists. Use a transaction to bind the setting to this query.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}
	// EXPLAIN the actual generated SQL, avoiding a duplicate that could drift.
	rows, err := tx.Query(ctx, "EXPLAIN "+listPlanRevisionStateForRuns, ids[:3])
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	t.Logf("EXPLAIN ListPlanRevisionStateForRuns (enable_seqscan=off):\n%s", plan.String())
	if !strings.Contains(plan.String(), "idx_run_messages_plan_seq") {
		t.Fatalf("EXPLAIN did not use idx_run_messages_plan_seq:\n%s", plan.String())
	}
}
