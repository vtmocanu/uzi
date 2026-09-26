package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// health_lead_window_livedb_test.go is the issue #1394 live-DB gate for
// ListRunLeadToolWindow: it executes the REAL query against a throwaway Postgres and
// proves the lead-lane filter (agent_instance IS NULL), the kind + payload event
// filter for lifecycle boundaries (status init/result, error result), the exclusion of
// nested subagent rows and unrelated status rows, and the seq DESC order. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via ./e2e/run-store-it.sh).

func TestListRunLeadToolWindowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedLaneRun(t, userID, workerID, repoID)
	at := time.Now().UTC().Truncate(time.Microsecond)

	const parent = "parent-agent"
	env.insertLaneMessage(t, runID, 1, "status", "lead", "", `{"event":"init","model":"m"}`, at)
	env.insertLaneMessage(t, runID, 2, "tool_use", "lead", "",
		`{"id":"`+parent+`","name":"Agent","input":{"description":"work"}}`, at)
	// An unrelated lead status row: not a boundary, must be excluded.
	env.insertLaneMessage(t, runID, 3, "status", "lead", "", `{"event":"other"}`, at)
	seq := int32(4)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("nested-%d", i)
		env.insertLaneMessage(t, runID, seq, "tool_use", "coder", parent,
			`{"id":"`+id+`","name":"Read","input":{}}`, at)
		env.insertLaneMessage(t, runID, seq+1, "tool_result", "coder", parent,
			`{"tool_use_id":"`+id+`"}`, at)
		seq += 2
	}
	// A nested subagent's own result/init-shaped status frames must be excluded too.
	env.insertLaneMessage(t, runID, seq, "status", "coder", parent, `{"event":"result"}`, at)
	env.insertLaneMessage(t, runID, seq+1, "status", "lead", "", `{"event":"result","subtype":"success"}`, at)
	env.insertLaneMessage(t, runID, seq+2, "error", "lead", "", `{"event":"result","subtype":"error_during_execution"}`, at)
	// A lead error that is not a result, and a lead text row: both excluded.
	env.insertLaneMessage(t, runID, seq+3, "error", "lead", "", `{"event":"other"}`, at)
	env.insertLaneMessage(t, runID, seq+4, "text", "lead", "", `{"text":"hi"}`, at)
	env.insertLaneMessage(t, runID, seq+5, "tool_result", "lead", "", `{"tool_use_id":"`+parent+`"}`, at)

	rows, err := env.q.ListRunLeadToolWindow(env.ctx, store.ListRunLeadToolWindowParams{RunID: runID, Lim: toolWindowFetch})
	if err != nil {
		t.Fatalf("ListRunLeadToolWindow: %v", err)
	}
	type got struct {
		seq  int32
		kind string
	}
	want := []got{
		{seq + 5, "tool_result"},
		{seq + 2, "error"},
		{seq + 1, "status"},
		{2, "tool_use"},
		{1, "status"},
	}
	if len(rows) != len(want) {
		for _, r := range rows {
			t.Logf("row seq=%d kind=%s payload=%s", r.Seq, r.Kind, r.Payload)
		}
		t.Fatalf("got %d rows, want %d (lead tool rows + lifecycle boundaries only)", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Seq != w.seq || rows[i].Kind != w.kind {
			t.Fatalf("row %d = seq %d kind %s, want seq %d kind %s", i, rows[i].Seq, rows[i].Kind, w.seq, w.kind)
		}
	}

	// The fetched rows drive leadInFlight: the parent dispatch under its init reads in
	// flight; a result boundary newer than it ends the leg (not in flight); and the
	// full window, with the parent's result, is not in flight either.
	if !leadInFlight(rows[3:]) {
		t.Fatal("leadInFlight(parent use + init) = false, want true")
	}
	if leadInFlight(rows[1:]) {
		t.Fatal("leadInFlight(result boundary above parent use) = true, want false")
	}
	if leadInFlight(rows) {
		t.Fatal("leadInFlight(after parent result) = true, want false")
	}

	// The limit applies to the lead lane alone: 2 rows is the two newest lead rows,
	// not nested frames.
	lim, err := env.q.ListRunLeadToolWindow(env.ctx, store.ListRunLeadToolWindowParams{RunID: runID, Lim: 2})
	if err != nil {
		t.Fatalf("ListRunLeadToolWindow lim 2: %v", err)
	}
	if len(lim) != 2 || lim[0].Seq != seq+5 || lim[1].Seq != seq+2 {
		t.Fatalf("lim 2 rows = %+v, want seqs %d, %d", lim, seq+5, seq+2)
	}
}
