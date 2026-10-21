package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// check_activity_signals_test.go pins PRD #3166: the cross-check activity events are `status`
// rows with new `event` names. They are telemetry only, so the run-health scans (loop detection
// and in-flight / long-call analysis) must read a window that contains them exactly as they read
// the same window without them.

var checkActivityPayloads = []map[string]any{
	{"event": "check_activity_start", "v": 1, "cap": 200, "generation": 2},
	{"event": "check_activity", "op": "read", "outcome": "ok", "path": "api/x.go", "lines": []int{1, 200}},
	{"event": "check_activity", "op": "search", "outcome": "ok", "path": ".", "matches": 3},
	{"event": "check_activity_summary", "generation": 2, "family": "claude", "stage": "verdict",
		"reads": map[string]int{"ok": 1}, "searches": map[string]int{"ok": 1},
		"dropped": 0, "untracked": 0, "unsettled": 0, "complete": true},
}

// withToolActivity interleaves activity rows (newest-first, like the query) around rows. Seqs
// are not renumbered: the scans key on kind and payload, never on contiguity.
func withToolActivity(t *testing.T, rows []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
	t.Helper()
	var out []store.ListRunToolWindowRow
	for i, p := range checkActivityPayloads {
		out = append(out, store.ListRunToolWindowRow{Seq: int32(10000 - i), Kind: "status", Payload: mustJSON(t, p)})
	}
	// Enough newest activity rows that a scan counting status rows toward the window would
	// leave fewer than loopThreshold real tool_use rows inside it.
	for i := 0; i < loopWindow-loopThreshold+1; i++ {
		out = append(out, store.ListRunToolWindowRow{Seq: int32(9000 - i), Kind: "status", Payload: mustJSON(t, checkActivityPayloads[1])})
	}
	for i, r := range rows {
		out = append(out, r)
		if i%3 == 0 {
			out = append(out, store.ListRunToolWindowRow{Seq: int32(5000 - i), Kind: "status", Payload: mustJSON(t, checkActivityPayloads[1])})
		}
	}
	return out
}

func TestAnalyzeToolWindowIgnoresCheckActivity(t *testing.T) {
	cases := []struct {
		name string
		rows []store.ListRunToolWindowRow
		want bool
	}{
		{"looping window", loopPairs(t, loopWindow), true},
		{"empty window", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := analyzeToolWindow(tc.rows); got != tc.want {
				t.Fatalf("baseline analyzeToolWindow = %v, want %v", got, tc.want)
			}
			if got := analyzeToolWindow(withToolActivity(t, tc.rows)); got != tc.want {
				t.Fatalf("with check_activity rows analyzeToolWindow = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("activity alone is never a loop", func(t *testing.T) {
		var only []store.ListRunToolWindowRow
		for i := 0; i < 3*loopWindow; i++ {
			only = append(only, store.ListRunToolWindowRow{Seq: int32(1000 - i), Kind: "status", Payload: mustJSON(t, checkActivityPayloads[1])})
		}
		if analyzeToolWindow(only) {
			t.Fatal("identical check_activity rows must not read as a tool loop")
		}
	})
}

func TestAnalyzeLeadWindowIgnoresCheckActivity(t *testing.T) {
	activity := func(from int32) []store.ListRunLeadToolWindowRow {
		var out []store.ListRunLeadToolWindowRow
		for i, p := range checkActivityPayloads {
			out = append(out, store.ListRunLeadToolWindowRow{Seq: from + int32(i), Kind: "status", Payload: mustJSON(t, p)})
		}
		return out
	}
	t.Run("activity alone is not in flight", func(t *testing.T) {
		st := analyzeLeadWindow(rowsDesc(activity(1)...))
		if st.inFlight || st.delegationOpen || !st.oldestOpen.IsZero() {
			t.Fatalf("analyzeLeadWindow(activity only) = %+v, want zero", st)
		}
	})
	t.Run("activity does not close or end an open call", func(t *testing.T) {
		// Oldest-first: init, an open parent dispatch, activity events after it. The
		// activity rows are not a lifecycle boundary, so the open call stays in flight.
		rows := []store.ListRunLeadToolWindowRow{leadEventRow(t, 1, "status", "init"), leadUseRow(t, 2, "parent")}
		rows = append(rows, activity(3)...)
		st := analyzeLeadWindow(rowsDesc(rows...))
		if !st.inFlight || !st.delegationOpen {
			t.Fatalf("analyzeLeadWindow = %+v, want the open parent dispatch still in flight", st)
		}
		if !leadInFlight(rowsDesc(rows...)) {
			t.Fatal("leadInFlight = false, want true")
		}
	})
	t.Run("activity does not reopen a completed call", func(t *testing.T) {
		rows := []store.ListRunLeadToolWindowRow{
			leadEventRow(t, 1, "status", "init"), leadUseRow(t, 2, "parent"), leadResultRow(t, 3, "parent"),
		}
		rows = append(rows, activity(4)...)
		if st := analyzeLeadWindow(rowsDesc(rows...)); st.inFlight {
			t.Fatalf("analyzeLeadWindow = %+v, want nothing in flight", st)
		}
	})
}
