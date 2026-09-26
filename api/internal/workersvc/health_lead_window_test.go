package workersvc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// health_lead_window_test.go pins issue #1394: the stalled signal's in-flight
// suppression reads the LEAD lane (ListRunLeadToolWindow) through leadInFlight, so a
// nested subagent's completed calls cannot hide the lead's open parent `Agent`
// dispatch, and a lifecycle boundary (init / result) bounds how far back an
// unmatched call counts.

func leadUseRow(t *testing.T, seq int32, id string) store.ListRunLeadToolWindowRow {
	return store.ListRunLeadToolWindowRow{Seq: seq, Kind: "tool_use", Payload: mustJSON(t, map[string]any{"id": id, "name": "Agent", "input": map[string]any{}})}
}

func leadResultRow(t *testing.T, seq int32, useID string) store.ListRunLeadToolWindowRow {
	return store.ListRunLeadToolWindowRow{Seq: seq, Kind: "tool_result", Payload: mustJSON(t, map[string]any{"tool_use_id": useID})}
}

func leadEventRow(t *testing.T, seq int32, kind, event string) store.ListRunLeadToolWindowRow {
	return store.ListRunLeadToolWindowRow{Seq: seq, Kind: kind, Payload: mustJSON(t, map[string]any{"event": event})}
}

// rowsDesc returns rows (written oldest-first for readability) in the query's
// seq-DESC order.
func rowsDesc(rows ...store.ListRunLeadToolWindowRow) []store.ListRunLeadToolWindowRow {
	out := make([]store.ListRunLeadToolWindowRow, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i])
	}
	return out
}

func TestLeadInFlight(t *testing.T) {
	cases := []struct {
		name string
		rows []store.ListRunLeadToolWindowRow // oldest-first
		want bool
	}{
		{
			// (a) the lead's parent Agent dispatch is open; the lead's own later rows
			// are an unrelated completed call. Nested rows never reach the lead lane.
			name: "open parent Agent under later completed lead call",
			rows: []store.ListRunLeadToolWindowRow{
				leadEventRow(t, 1, "status", "init"),
				leadUseRow(t, 2, "parent"),
				leadUseRow(t, 60, "read"),
				leadResultRow(t, 61, "read"),
			},
			want: true,
		},
		{
			name: "parent Agent result landed",
			rows: []store.ListRunLeadToolWindowRow{
				leadEventRow(t, 1, "status", "init"),
				leadUseRow(t, 2, "parent"),
				leadUseRow(t, 60, "read"),
				leadResultRow(t, 61, "read"),
				leadResultRow(t, 62, "parent"),
			},
			want: false,
		},
		{
			// (b) Codex concurrent delegation order, A still running.
			name: "codex order use A, use B, result B, use C, result C",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "A"),
				leadUseRow(t, 2, "B"),
				leadResultRow(t, 3, "B"),
				leadUseRow(t, 4, "C"),
				leadResultRow(t, 5, "C"),
			},
			want: true,
		},
		{
			name: "codex order then result A",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "A"),
				leadUseRow(t, 2, "B"),
				leadResultRow(t, 3, "B"),
				leadUseRow(t, 4, "C"),
				leadResultRow(t, 5, "C"),
				leadResultRow(t, 6, "A"),
			},
			want: false,
		},
		{
			// (c) delayed sibling: an older completed call X, then B is persisted, then
			// an unrelated call Y completes after it; B has no result. The unrelated
			// result must not end the scan.
			name: "delayed sibling B unmatched behind unrelated result",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "X"),
				leadResultRow(t, 2, "X"),
				leadUseRow(t, 3, "B"),
				leadUseRow(t, 4, "Y"),
				leadResultRow(t, 5, "Y"),
			},
			want: true,
		},
		{
			// (d) a result boundary (status kind) ends the scan: the older unmatched
			// use belongs to an earlier leg.
			name: "result status boundary hides older orphan",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "orphan"),
				leadEventRow(t, 2, "status", "result"),
				leadUseRow(t, 3, "done"),
				leadResultRow(t, 4, "done"),
			},
			want: false,
		},
		{
			name: "result error boundary hides older orphan",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "orphan"),
				leadEventRow(t, 2, "error", "result"),
				leadUseRow(t, 3, "done"),
				leadResultRow(t, 4, "done"),
			},
			want: false,
		},
		{
			// (e) an init boundary ends the scan the same way.
			name: "init boundary hides older orphan",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "orphan"),
				leadEventRow(t, 2, "status", "init"),
				leadUseRow(t, 3, "done"),
				leadResultRow(t, 4, "done"),
			},
			want: false,
		},
		{
			// (f) init as the newest lead row: a fresh leg with nothing dispatched yet.
			name: "init newest row",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "orphan"),
				leadEventRow(t, 2, "status", "init"),
			},
			want: false,
		},
		{
			// A boundary is kind AND event: an init-looking error or an unrelated
			// status event is not a boundary, so the orphan still counts.
			name: "non-boundary status and error init are not boundaries",
			rows: []store.ListRunLeadToolWindowRow{
				leadUseRow(t, 1, "open"),
				leadEventRow(t, 2, "status", "other"),
				leadEventRow(t, 3, "error", "init"),
			},
			want: true,
		},
		{
			// (g) a lone tool_result with no use is harmless.
			name: "lone tool_result",
			rows: []store.ListRunLeadToolWindowRow{
				leadResultRow(t, 1, "ghost"),
			},
			want: false,
		},
		{
			name: "empty",
			rows: nil,
			want: false,
		},
		{
			// A use with no id cannot be matched and never counts as in flight.
			name: "use without id",
			rows: []store.ListRunLeadToolWindowRow{
				{Seq: 1, Kind: "tool_use", Payload: mustJSON(t, map[string]any{"name": "Bash"})},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leadInFlight(rowsDesc(tc.rows...)); got != tc.want {
				t.Fatalf("leadInFlight = %v, want %v", got, tc.want)
			}
		})
	}
}

func fakeMsg(t *testing.T, seq int32, kind, instance string, payload map[string]any) fakeRunMessage {
	return fakeRunMessage{seq: seq, kind: kind, agentInstance: instance, payload: mustJSON(t, payload)}
}

// TestHealthStalledSuppressedWhileParentAgentOpenUnderNestedTraffic is the issue
// #1394 regression through detectRunHealth: the lead's parent `Agent` dispatch is
// open while its subagent completes 50 nested calls (more than the 40-row mixed
// window). The run is working, not stalled; once the parent's result lands, the
// same silence is genuinely stalled.
func TestHealthStalledSuppressedWhileParentAgentOpenUnderNestedTraffic(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(time.Hour)
	r.LastActivityAt = ago(20 * time.Minute)

	const parent = "parent-agent"
	msgs := []fakeRunMessage{
		fakeMsg(t, 1, "status", "", map[string]any{"event": "init", "model": "m"}),
		fakeMsg(t, 2, "tool_use", "", map[string]any{"id": parent, "name": "Agent", "input": map[string]any{"description": "work"}}),
	}
	seq := int32(3)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("nested-%d", i)
		msgs = append(msgs,
			fakeMsg(t, seq, "tool_use", parent, map[string]any{"id": id, "name": "Read", "input": map[string]any{"file_path": fmt.Sprintf("f%d.go", i)}}),
			fakeMsg(t, seq+1, "tool_result", parent, map[string]any{"tool_use_id": id}),
		)
		seq += 2
	}
	fs := &healthFakeStore{
		active:   []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: msgs},
	}
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, stall: 300})

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (parent Agent call still in flight)", n)
	}
	for _, w := range fs.writes {
		if w.ID == r.ID && w.Health == healthStalled {
			t.Fatalf("flagged stalled while the lead's parent Agent call is open: %+v", w)
		}
	}

	fs.writes = nil
	fs.messages[r.ID] = append(msgs, fakeMsg(t, seq, "tool_result", "", map[string]any{"tool_use_id": parent}))
	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 once the parent Agent call completed", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled after the parent result landed", w.Health)
	}
}
