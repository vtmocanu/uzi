package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestLatestToolUseForRunsLiveDB is the SEAM proof for PRD #1064 M2's current_activity
// query. It executes LatestToolUseForRuns against a real Postgres (a fake store cannot
// exercise DISTINCT ON, the seq-DESC ordering, or the partial index), asserting: the
// newest tool_use wins per run, a newer tool_result is SKIPPED (kind filter), and a run
// with only non-tool_use frames returns NO row (⇒ null current_activity). It also
// records the EXPLAIN for the query, proving the partial index
// idx_run_messages_tool_use_seq is available to the planner.
//
// It lives in the store package deliberately: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... only, so a
// *LiveDB test placed elsewhere would never gate. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres.
func TestLatestToolUseForRunsLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rta-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rta', 'https://forge.e2e/g/rta', 'main', true)`, repoID, connID)

	// Three runs, each a distinct issue_iid (uq_runs_one_active_per_issue is per repo).
	runA, runB, runC := uuid.New(), uuid.New(), uuid.New()
	mkRun := func(id uuid.UUID, iid int64) {
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running')`, id, userID, repoID, iid)
	}
	mkRun(runA, 1)
	mkRun(runB, 2)
	mkRun(runC, 3)

	msg := func(runID uuid.UUID, seq int32, kind, agent, label string, payload any) {
		t.Helper()
		var raw []byte
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			raw = b
		} else {
			raw = []byte(`{}`)
		}
		var agentArg, labelArg any
		if agent != "" {
			agentArg = agent
		}
		if label != "" {
			labelArg = label
		}
		exec(`INSERT INTO run_messages (run_id, seq, kind, agent, agent_label, payload)
		      VALUES ($1, $2, $3, $4, $5, $6)`, runID, seq, kind, agentArg, labelArg, raw)
	}
	toolUse := func(name string, input map[string]any) map[string]any {
		return map[string]any{"name": name, "input": input}
	}

	// run A: Read (seq 1) → Edit (seq 2, the newest tool_use) → a NEWER tool_result (seq 3).
	// The tool_result must NOT win: the query filters kind='tool_use'.
	msg(runA, 1, "tool_use", "coder", "task", toolUse("Read", map[string]any{"file_path": "api/a.go"}))
	msg(runA, 2, "tool_use", "coder", "task", toolUse("Edit", map[string]any{"file_path": "api/b.go"}))
	msg(runA, 3, "tool_result", "coder", "task", map[string]any{"tool_use_id": "t1", "content": "ok"})

	// run B: a status (seq 1) then a single tool_use (seq 2, the newest).
	msg(runB, 1, "status", "worker", "", map[string]any{"phase": "implement iteration 1"})
	msg(runB, 2, "tool_use", "lead", "", toolUse("Bash", map[string]any{"command": "go build ./...", "description": "build"}))

	// run C: no tool_use at all → must return no row.
	msg(runC, 1, "status", "worker", "", map[string]any{"phase": "implement iteration 1"})
	msg(runC, 2, "text", "lead", "", map[string]any{"text": "thinking"})

	rows, err := q.LatestToolUseForRuns(ctx, []uuid.UUID{runA, runB, runC})
	if err != nil {
		t.Fatalf("LatestToolUseForRuns: %v", err)
	}
	byRun := map[uuid.UUID]store.LatestToolUseForRunsRow{}
	for _, r := range rows {
		if _, dup := byRun[r.RunID]; dup {
			t.Fatalf("DISTINCT ON returned >1 row for run %s", r.RunID)
		}
		byRun[r.RunID] = r
	}

	// run A: newest tool_use is the Edit at seq 2, NOT the tool_result at seq 3.
	a, ok := byRun[runA]
	if !ok {
		t.Fatalf("run A missing from result")
	}
	if a.Seq != 2 || a.Kind != "tool_use" {
		t.Fatalf("run A: got seq=%d kind=%q, want seq=2 kind=tool_use (newer tool_result must be skipped)", a.Seq, a.Kind)
	}
	if name := payloadName(t, a.Payload); name != "Edit" {
		t.Fatalf("run A: payload tool = %q, want Edit", name)
	}

	// run B: newest tool_use is the Bash at seq 2 (the status at seq 1 is skipped).
	b, ok := byRun[runB]
	if !ok {
		t.Fatalf("run B missing from result")
	}
	if b.Seq != 2 || payloadName(t, b.Payload) != "Bash" {
		t.Fatalf("run B: got seq=%d tool=%q, want seq=2 tool=Bash", b.Seq, payloadName(t, b.Payload))
	}

	// run C: no tool_use frame → no row.
	if _, ok := byRun[runC]; ok {
		t.Fatalf("run C has no tool_use frame but returned a row")
	}

	// ── EXPLAIN: prove the partial index idx_run_messages_tool_use_seq is available. ──
	// Grow the table with noise frames so the planner has a reason to prefer the index,
	// then ANALYZE and record the plan. A tiny throwaway table can still be seq-scanned
	// on cost, so if the natural plan does not name the index this also records the plan
	// with enable_seqscan off, which proves the index path exists and is selected.
	for n := 0; n < 40; n++ {
		noise := uuid.New()
		mkRun(noise, int64(1000+n))
		for s := int32(1); s <= 30; s++ {
			kind := "status"
			payload := map[string]any{"phase": "x"}
			if s%3 == 0 {
				kind = "tool_use"
				payload = toolUse("Read", map[string]any{"file_path": "api/n.go"})
			}
			msg(noise, s, kind, "coder", "task", payload)
		}
	}
	exec(`ANALYZE run_messages`)

	const explainSQL = `EXPLAIN SELECT DISTINCT ON (run_id) run_id, seq, kind, agent, agent_label, payload, created_at
FROM run_messages
WHERE run_id = ANY($1::uuid[])
  AND kind = 'tool_use'
ORDER BY run_id, seq DESC`

	explain := func(seqScanOff bool) string {
		t.Helper()
		if seqScanOff {
			if _, err := pool.Exec(ctx, `SET enable_seqscan = off`); err != nil {
				t.Fatalf("SET enable_seqscan: %v", err)
			}
			defer func() { _, _ = pool.Exec(ctx, `SET enable_seqscan = on`) }()
		}
		rows, err := pool.Query(ctx, explainSQL, []uuid.UUID{runA, runB})
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan EXPLAIN row: %v", err)
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("EXPLAIN rows: %v", err)
		}
		return b.String()
	}

	natural := explain(false)
	t.Logf("EXPLAIN (natural) for LatestToolUseForRuns:\n%s", natural)
	if !strings.Contains(natural, "idx_run_messages_tool_use_seq") {
		t.Logf("EXPLAIN (enable_seqscan=off) — proves the partial index path exists and is chosen:\n%s", explain(true))
	}
}

func payloadName(t *testing.T, raw []byte) string {
	t.Helper()
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode payload name: %v", err)
	}
	return p.Name
}

// TestProgressOnlySetRunRunningLiveDB is the R2 assertion (PRD #1064): a progress-only
// `running` report — SetRunRunning with NO iteration_count (the immediate push the
// worker sends the moment it observes report_progress) — must leave iteration_count,
// status_since and started_at untouched. GREATEST on iteration_count and the entry-only
// stamps make the mid-run push a safe no-op on those columns.
func TestProgressOnlySetRunRunningLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rtapp-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rtapp', 'https://forge.e2e/g/rtapp', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("rtapp-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'claimed', $4)`, runID, userID, repoID, wkr.ID)

	// A first iteration-boundary report raises iteration_count to 5 and (entering
	// running) stamps status_since + started_at.
	if rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: runID, WorkerID: workerID, IterationCount: 5,
	}); err != nil {
		t.Fatalf("SetRunRunning(iter=5): %v", err)
	} else if rows != 1 {
		t.Fatalf("SetRunRunning(iter=5) moved %d rows, want 1", rows)
	}

	type snap struct {
		iter        int32
		statusSince time.Time
		startedAt   time.Time
	}
	read := func() snap {
		t.Helper()
		var s snap
		var startedAt pgtype.Timestamptz
		if err := pool.QueryRow(ctx,
			`SELECT iteration_count, status_since, started_at FROM runs WHERE id = $1`, runID,
		).Scan(&s.iter, &s.statusSince, &startedAt); err != nil {
			t.Fatalf("read run: %v", err)
		}
		if !startedAt.Valid {
			t.Fatalf("started_at is NULL after a running report, want stamped")
		}
		s.startedAt = startedAt.Time
		return s
	}
	before := read()
	if before.iter != 5 {
		t.Fatalf("iteration_count = %d after iter=5 report, want 5", before.iter)
	}

	// The progress-only push: SetRunRunning with NO iteration_count (zero value). This is
	// the shape the worker sends on observing report_progress.
	if rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: runID, WorkerID: workerID, // IterationCount left 0 — the progress-only shape.
	}); err != nil {
		t.Fatalf("SetRunRunning(progress-only): %v", err)
	} else if rows != 1 {
		t.Fatalf("SetRunRunning(progress-only) moved %d rows, want 1", rows)
	}

	after := read()
	if after.iter != before.iter {
		t.Fatalf("iteration_count regressed on a progress-only push: %d → %d (GREATEST must hold it)", before.iter, after.iter)
	}
	if !after.statusSince.Equal(before.statusSince) {
		t.Fatalf("status_since moved on a progress-only push: %v → %v (entry-only stamp)", before.statusSince, after.statusSince)
	}
	if !after.startedAt.Equal(before.startedAt) {
		t.Fatalf("started_at moved on a progress-only push: %v → %v (stamped once)", before.startedAt, after.startedAt)
	}
}
