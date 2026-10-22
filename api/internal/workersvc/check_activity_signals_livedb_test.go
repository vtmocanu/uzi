package workersvc

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// check_activity_signals_livedb_test.go is the PRD #3166 live-DB gate: the real health and
// interrupted-call window queries return the same rows with check_activity* status rows present
// in the run as without them. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (run via ./e2e/run-store-it.sh).

type windowSnapshot struct {
	tool []store.ListRunToolWindowRow
	lead []store.ListRunLeadToolWindowRow
}

func TestCheckActivityRowsLeaveHealthAndInterruptedWindowsUnchangedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil)
	e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce' WHERE id=$1", wid)

	// The lead run is an ordinary issue run at claim generation 4 (so InterruptedCallsWindow
	// authorizes it); the cross_check child hangs off a separate parent run.
	lead := e.seedActiveRunOwnedBy(t, wid)
	e.exec(t, "UPDATE runs SET claim_generation=4 WHERE id=$1", lead)
	parent := e.seedActiveRunOwnedBy(t, wid)
	child := uuid.New()
	e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status,worker_id,claim_generation)
VALUES($1,$2,$3,'cross_check',$4,'codex',true,60,'title','description','running',$5,2)`,
		child, e.userID, e.repoID, parent, wid)

	add := func(run uuid.UUID, seq int, kind string, payload any, agent string, gen int) {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(t, "INSERT INTO run_messages(run_id,seq,kind,payload,agent,agent_instance,claim_generation) VALUES($1,$2,$3,$4,$5,NULL,$6)",
			run, seq, kind, raw, agent, gen)
	}
	use := func(id string) any {
		return map[string]any{"id": id, "name": "Bash", "input": map[string]any{"command": "echo " + id}}
	}
	result := func(id string) any { return map[string]any{"tool_use_id": id, "content": "ok"} }

	// Baseline rows, identical in both runs: a completed call from generation 1, then an open
	// and a completed call in generation 2 (the interrupted-call source for generation 4 reads
	// the newest earlier generation that has tool rows).
	for _, r := range []struct {
		run   uuid.UUID
		agent string
	}{{lead, "lead"}, {child, "cross-checker"}} {
		add(r.run, 1, "status", map[string]any{"event": "init"}, r.agent, 1)
		add(r.run, 2, "tool_use", use("a"), r.agent, 1)
		add(r.run, 3, "tool_result", result("a"), r.agent, 1)
		add(r.run, 4, "tool_use", use("b"), r.agent, 2)
		add(r.run, 5, "tool_use", use("c"), r.agent, 2)
		add(r.run, 6, "tool_result", result("c"), r.agent, 2)
	}

	read := func(run uuid.UUID) windowSnapshot {
		t.Helper()
		tool, err := e.q.ListRunToolWindow(e.ctx, store.ListRunToolWindowParams{RunID: run, Lim: toolWindowFetch})
		if err != nil {
			t.Fatalf("ListRunToolWindow: %v", err)
		}
		leadRows, err := e.q.ListRunLeadToolWindow(e.ctx, store.ListRunLeadToolWindowParams{RunID: run, Lim: toolWindowFetch})
		if err != nil {
			t.Fatalf("ListRunLeadToolWindow: %v", err)
		}
		return windowSnapshot{tool: tool, lead: leadRows}
	}
	interrupted := func(run uuid.UUID, generation int64) []store.InterruptedCallsWindowRow {
		t.Helper()
		rows, err := e.q.InterruptedCallsWindow(e.ctx, store.InterruptedCallsWindowParams{
			RunID: run, WorkerID: wid, OwnerID: e.userID, RegisterNonce: "nonce", ClaimGeneration: generation,
		})
		if err != nil {
			t.Fatalf("InterruptedCallsWindow: %v", err)
		}
		return rows
	}

	beforeLead, beforeChild := read(lead), read(child)
	beforeInterruptedLead := interrupted(lead, 4)
	beforeInterruptedChild := interrupted(child, 2)
	if len(beforeInterruptedLead) == 0 || beforeInterruptedLead[0].CallID == "" {
		t.Fatalf("baseline InterruptedCallsWindow(lead) = %+v, want the open generation-2 call", beforeInterruptedLead)
	}
	if len(beforeInterruptedChild) != 0 {
		t.Fatalf("baseline InterruptedCallsWindow(cross_check) = %+v, want no rows (ineligible kind)", beforeInterruptedChild)
	}

	// Activity rows appended to both runs, tagged with the claim generation they describe.
	for _, r := range []struct {
		run uuid.UUID
		gen int
	}{{lead, 4}, {child, 2}} {
		add(r.run, 7, "status", map[string]any{"event": "check_activity_start", "v": 1, "cap": 200, "generation": r.gen}, "cross-checker", r.gen)
		add(r.run, 8, "status", map[string]any{"event": "check_activity", "op": "read", "outcome": "ok", "path": "api/x.go", "lines": []int{1, 200}}, "cross-checker", r.gen)
		add(r.run, 9, "status", map[string]any{"event": "check_activity", "op": "search", "outcome": "ok", "path": ".", "matches": 3}, "cross-checker", r.gen)
		add(r.run, 10, "status", map[string]any{"event": "check_activity_summary", "generation": r.gen, "family": "codex", "stage": "verdict",
			"reads": map[string]int{"ok": 1}, "searches": map[string]int{"ok": 1}, "dropped": 0, "untracked": 0, "unsettled": 0, "complete": true}, "cross-checker", r.gen)
	}
	const added = 4

	if got := read(lead); !reflect.DeepEqual(got, beforeLead) {
		t.Fatalf("lead health windows changed by check_activity rows:\nbefore %+v\nafter  %+v", beforeLead, got)
	}
	if got := read(child); !reflect.DeepEqual(got, beforeChild) {
		t.Fatalf("cross_check health windows changed by check_activity rows:\nbefore %+v\nafter  %+v", beforeChild, got)
	}
	if got := interrupted(child, 2); len(got) != 0 {
		t.Fatalf("InterruptedCallsWindow(cross_check) after = %+v, want no rows", got)
	}

	// The interrupted-call tail inspects the newest rows of ANY kind, so the activity rows
	// raise inspected_count by exactly their number; the selected tool rows are untouched.
	afterInterrupted := interrupted(lead, 4)
	if len(afterInterrupted) != len(beforeInterruptedLead) {
		t.Fatalf("InterruptedCallsWindow(lead) row count %d -> %d", len(beforeInterruptedLead), len(afterInterrupted))
	}
	for i := range afterInterrupted {
		if afterInterrupted[i].InspectedCount != beforeInterruptedLead[i].InspectedCount+added {
			t.Fatalf("row %d InspectedCount = %d, want %d", i, afterInterrupted[i].InspectedCount, beforeInterruptedLead[i].InspectedCount+added)
		}
		got, want := afterInterrupted[i], beforeInterruptedLead[i]
		got.InspectedCount = want.InspectedCount
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("InterruptedCallsWindow(lead) row %d changed:\nbefore %+v\nafter  %+v", i, want, got)
		}
	}

	// And the Go scans over the real rows agree.
	if analyzeToolWindow(read(lead).tool) || analyzeToolWindow(read(child).tool) {
		t.Fatal("analyzeToolWindow flagged a loop")
	}
	if st := analyzeLeadWindow(read(lead).lead); !st.inFlight {
		t.Fatalf("analyzeLeadWindow(lead) = %+v, want the open call still in flight", st)
	}
}
