package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type interruptedFake struct {
	Store
	read func(context.Context, store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error)
}

func (f interruptedFake) InterruptedCallsWindow(ctx context.Context, p store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error) {
	return f.read(ctx, p)
}

func interruptedRow(seq int32, id string) store.InterruptedCallsWindowRow {
	return store.InterruptedCallsWindowRow{InspectedCount: 256, SourceGeneration: 1, Seq: pgtype.Int4{Int32: seq, Valid: true}, Kind: pgtype.Text{String: "tool_use", Valid: true}, Agent: "lead", ExplicitLead: true, NullLane: true, CallID: id, ToolName: "Bash", Command: "echo ok"}
}
func interruptedRead(t *testing.T, rows ...store.InterruptedCallsWindowRow) apitypes.InterruptedCallsResponse {
	t.Helper()
	s := &Service{q: interruptedFake{read: func(context.Context, store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error) {
		return rows, nil
	}}}
	raw, err := s.InterruptedCalls(context.Background(), store.Worker{ID: uuid.New(), UserID: uuid.New()}, uuid.New(), "nonce", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 16384 || !json.Valid(raw) {
		t.Fatalf("invalid envelope length=%d", len(raw))
	}
	var out apitypes.InterruptedCallsResponse
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestInterruptedCallsPairing(t *testing.T) {
	use := interruptedRow(10, "call")
	result := use
	result.Kind.String = "tool_result"
	result.Seq.Int32 = 11
	cases := []struct {
		name        string
		rows        []store.InterruptedCallsWindowRow
		entries     int
		unavailable bool
	}{
		{"unmatched", []store.InterruptedCallsWindowRow{use}, 1, false},
		{"completed including synthetic error", []store.InterruptedCallsWindowRow{use, result}, 0, false},
		{"late", []store.InterruptedCallsWindowRow{result, use}, 0, false},
		{"duplicate use", []store.InterruptedCallsWindowRow{use, use}, 0, true},
		{"duplicate result", []store.InterruptedCallsWindowRow{use, result, result}, 0, true},
	}
	before := result
	before.Seq.Int32 = 9
	cases = append(cases, struct {
		name        string
		rows        []store.InterruptedCallsWindowRow
		entries     int
		unavailable bool
	}{"result before use", []store.InterruptedCallsWindowRow{use, before}, 0, true})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := interruptedRead(t, tc.rows...)
			if len(out.Entries) != tc.entries || out.Unavailable != tc.unavailable {
				t.Fatalf("%+v", out)
			}
		})
	}
	t.Run("count duplicates before display eligibility", func(t *testing.T) {
		other := use
		other.ToolName = "Read"
		out := interruptedRead(t, use, other)
		if len(out.Entries) != 0 || !out.Unavailable {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("same role distinct instances", func(t *testing.T) {
		a := use
		a.NullLane = false
		a.AgentInstance = "dispatch-a"
		b := result
		b.NullLane = false
		b.AgentInstance = "dispatch-b"
		out := interruptedRead(t, a, b)
		if len(out.Entries) != 1 || *out.Entries[0].AgentInstance != "dispatch-a" {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("nonnull lane role lead settles", func(t *testing.T) {
		a := use
		a.NullLane = false
		a.AgentInstance = "dispatch"
		b := result
		b.NullLane = false
		b.AgentInstance = "dispatch"
		out := interruptedRead(t, a, b)
		if len(out.Entries) != 0 {
			t.Fatal(out)
		}
	})
	t.Run("null nonlead never settles lead", func(t *testing.T) {
		b := result
		b.Agent = "reviewer"
		b.ExplicitLead = false
		out := interruptedRead(t, use, b)
		if len(out.Entries) != 1 || !out.Unavailable {
			t.Fatal(out)
		}
	})
	t.Run("full long IDs never prefix match", func(t *testing.T) {
		a := use
		a.CallID = strings.Repeat("x", 1024)
		b := result
		b.CallID = strings.Repeat("x", 1023) + "y"
		out := interruptedRead(t, a, b)
		if len(out.Entries) != 1 {
			t.Fatal(out)
		}
	})
}
func TestInterruptedCallsRoleAttribution(t *testing.T) {
	use := interruptedRow(10, "call")
	use.NullLane = false
	use.AgentInstance = "dispatch"
	use.Agent = "reviewer"
	use.ExplicitLead = false
	result := use
	result.Kind.String = "tool_result"
	result.Seq.Int32 = 11

	t.Run("conflicting attribution is unavailable in either row order", func(t *testing.T) {
		result.Agent = "lead"
		result.ExplicitLead = true
		for _, rows := range [][]store.InterruptedCallsWindowRow{{use, result}, {result, use}} {
			out := interruptedRead(t, rows...)
			if len(out.Entries) != 0 || !out.Unavailable {
				t.Fatalf("%+v", out)
			}
		}
	})
	t.Run("role must not construct identity or hide duplicate uses", func(t *testing.T) {
		other := use
		other.Agent = "lead"
		other.ExplicitLead = true
		other.Command = ""
		out := interruptedRead(t, use, other)
		if len(out.Entries) != 0 || !out.Unavailable {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("consistent nonnull lead is distinct from null lead", func(t *testing.T) {
		lead := interruptedRow(12, "call")
		nonnull := lead
		nonnull.NullLane = false
		nonnull.AgentInstance = "dispatch"
		out := interruptedRead(t, lead, nonnull)
		if len(out.Entries) != 2 || out.Unavailable {
			t.Fatalf("%+v", out)
		}
		settled := nonnull
		settled.Kind.String = "tool_result"
		settled.Seq.Int32 = 13
		out = interruptedRead(t, lead, nonnull, settled)
		if len(out.Entries) != 1 || out.Entries[0].AgentInstance != nil || out.Unavailable {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("role never repairs an empty nonnull lane", func(t *testing.T) {
		lead := interruptedRow(12, "call")
		malformed := lead
		malformed.NullLane = false
		malformed.Kind.String = "tool_result"
		out := interruptedRead(t, lead, malformed)
		if len(out.Entries) != 1 || out.Entries[0].AgentInstance != nil || !out.Unavailable {
			t.Fatalf("%+v", out)
		}
	})
}

func TestInterruptedCallsProjectedAgentOmission(t *testing.T) {
	use := interruptedRow(1, "call")
	use.NullLane = false
	use.AgentInstance = "dispatch"
	use.Agent = ""
	use.AgentOmitted = true
	result := use
	result.Kind.String = "tool_result"
	result.Seq.Int32 = 2
	out := interruptedRead(t, use, result)
	if len(out.Entries) != 0 || !out.Unavailable {
		t.Fatal(out)
	}
}

func TestInterruptedCallsProjectedLabelOmission(t *testing.T) {
	r := interruptedRow(1, "call")
	// SQL omits the complete oversized label and projects only this boolean.
	r.AgentLabelOmitted = true
	out := interruptedRead(t, r)
	if len(out.Entries) != 1 || out.Entries[0].AgentLabel != nil || !out.Entries[0].DisplayOmitted || out.Entries[0].Command != "echo ok" || out.Unavailable {
		t.Fatalf("%+v", out)
	}
}

func TestInterruptedCallsPairsBeforeDisplayLimit(t *testing.T) {
	var rows []store.InterruptedCallsWindowRow
	for i := int32(1); i <= 4; i++ {
		rows = append(rows, interruptedRow(i, string(rune('a'+i))))
	}
	settled := rows[3]
	settled.Kind.String = "tool_result"
	settled.Seq.Int32 = 5
	rows = append(rows, settled)
	out := interruptedRead(t, rows...)
	if len(out.Entries) != 3 || out.Entries[0].Seq != 3 || out.Entries[2].Seq != 1 {
		t.Fatal("display limit applied before pairing")
	}
}

func TestInterruptedCallsKeysAndBounds(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("x", 1025), "a\nb", "a\u202eb", "a\u200bb", string([]byte{0xff})} {
		t.Run("invalid key "+id[:min(len(id), 12)], func(t *testing.T) {
			out := interruptedRead(t, interruptedRow(1, id))
			if len(out.Entries) != 0 || !out.Unavailable {
				t.Fatal(out)
			}
		})
	}
	for _, n := range []int{129, 1024} {
		out := interruptedRead(t, interruptedRow(1, strings.Repeat("x", n)))
		if len(out.Entries) != 1 {
			t.Fatal(out)
		}
	}
	for _, lane := range []string{"", strings.Repeat("界", 128)} {
		r := interruptedRow(1, "id")
		r.NullLane = false
		r.AgentInstance = lane
		if out := interruptedRead(t, r); len(out.Entries) != 0 || !out.Unavailable {
			t.Fatal(out)
		}
	}
	r := interruptedRow(1, "id")
	r.Command = strings.Repeat("界", 683)
	if out := interruptedRead(t, r); len(out.Entries) != 0 || !out.Truncated {
		t.Fatal(out)
	}
	r.Command = strings.Repeat("x", 2048)
	if out := interruptedRead(t, r); len(out.Entries) != 1 || out.Entries[0].Command != r.Command {
		t.Fatal(out)
	}
	r.Command = "echo ok"
	r.Agent = strings.Repeat("界", 129)
	if out := interruptedRead(t, r); len(out.Entries) != 1 || out.Entries[0].Agent != "[omitted]" || !out.Entries[0].DisplayOmitted {
		t.Fatal(out)
	}
	r.Command = "echo glpat-" + "abcdefghijklmnopqrst"
	if out := interruptedRead(t, r); len(out.Entries) != 1 || strings.Contains(out.Entries[0].Command, "abcdefghijklmnopqrst") {
		t.Fatal(out)
	}
}
func TestInterruptedCallsDisplayNormalization(t *testing.T) {
	r := interruptedRow(1, "call")
	// Assemble the fixture so no complete provider-shaped literal is tracked.
	r.AgentLabel = "label\n\t\x1b[31m\u202e glpat-" + "abcdefghij\nklmnopqrst"
	out := interruptedRead(t, r)
	if len(out.Entries) != 1 || out.Entries[0].AgentLabel == nil {
		t.Fatal("missing normalized display")
	}
	label := *out.Entries[0].AgentLabel
	if strings.ContainsAny(label, "\n\t\x1b\u202e") || strings.Contains(label, "abcdefghij") {
		t.Fatalf("unsafe display: %q", label)
	}
}

func TestInterruptedCallsWireBudget(t *testing.T) {
	var rows []store.InterruptedCallsWindowRow
	for i := int32(1); i <= 5; i++ {
		r := interruptedRow(i, string(rune('a'+i)))
		r.Command = strings.Repeat("<", 2048)
		rows = append(rows, r)
	}
	out := interruptedRead(t, rows...)
	if len(out.Entries) != 1 || out.Entries[0].Seq != 5 || out.Entries[0].Command != strings.Repeat("<", 2048) || !out.Truncated {
		t.Fatalf("%+v", out)
	}
	for i := range rows {
		rows[i].Command = "echo ok"
	}
	out = interruptedRead(t, rows...)
	if len(out.Entries) != 3 || out.Entries[0].Seq != 5 || out.Entries[2].Seq != 3 {
		t.Fatalf("%+v", out)
	}
	empty := interruptedRead(t, store.InterruptedCallsWindowRow{})
	raw, _ := json.Marshal(empty)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	want := []string{"format", "run_id", "worker_id", "register_nonce", "claim_generation", "source_generation", "inspected_count", "history_incomplete", "unavailable", "truncated", "entries"}
	if len(fields) != len(want) {
		t.Fatalf("%s", raw)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if string(fields["entries"]) != "[]" || string(fields["source_generation"]) != "null" || !empty.HistoryIncomplete || empty.Format != 1 {
		t.Fatalf("%s", raw)
	}
}
func TestInterruptedCallsDeadlineAndBinding(t *testing.T) {
	w := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	run := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{q: interruptedFake{read: func(got context.Context, p store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error) {
		if p.RunID != run || p.WorkerID != w.ID || p.OwnerID != w.UserID || p.RegisterNonce != "nonce" || p.ClaimGeneration != 2 {
			t.Fatalf("%+v", p)
		}
		deadline, ok := got.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("missing bounded deadline")
		}
		return nil, got.Err()
	}}}
	if _, err := s.InterruptedCalls(ctx, w, run, "nonce", 2); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.q = interruptedFake{read: func(context.Context, store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error) {
		return nil, nil
	}}
	if _, err := s.InterruptedCalls(context.Background(), w, run, "nonce", 2); !errors.Is(err, ErrRunNotOwned) {
		t.Fatal(err)
	}
	for _, nonce := range []string{"", strings.Repeat("n", 129)} {
		if _, err := s.InterruptedCalls(context.Background(), w, run, nonce, 2); !errors.Is(err, ErrInvalidState) {
			t.Fatal(err)
		}
	}
}
