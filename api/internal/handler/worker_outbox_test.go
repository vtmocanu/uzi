package handler

// PRD #1391 M5 — parseWorkerOutbox, the heartbeat's defensive second-step parse of
// the untrusted, isolated `outbox` field. The contract is drop-not-fail: no shape of
// input may ever return an error (there is none to return), and a bad entry must
// never take a good one down with it. These pin that.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseWorkerOutboxValid(t *testing.T) {
	wid := uuid.New()
	r1 := uuid.New()
	raw := json.RawMessage(`[{"run_id":"` + r1.String() + `","pending_messages":3,"pending_terminal":0,"stale_retired":1,"since":1699999999999}]`)
	out := parseWorkerOutbox(raw, wid)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	e := out[0]
	if e.RunID != r1 || e.PendingMessages != 3 || e.PendingTerminal != 0 || e.StaleRetired != 1 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Since.UnixMilli() != 1699999999999 {
		t.Fatalf("since = %d ms, want 1699999999999 (epoch ms, not RFC3339)", e.Since.UnixMilli())
	}
}

func TestParseWorkerOutboxDropNotFail(t *testing.T) {
	wid := uuid.New()
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"absent", ``},
		{"null", `null`},
		{"empty array", `[]`},
		{"malformed", `[{`},
		{"object not array", `{"run_id":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := parseWorkerOutbox(json.RawMessage(tc.raw), wid); len(out) != 0 {
				t.Fatalf("out = %+v, want empty (drop-not-fail)", out)
			}
		})
	}
}

func TestParseWorkerOutboxOversizeDropsWholeReport(t *testing.T) {
	wid := uuid.New()
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; b.Len() <= maxOutboxReportBytes+1024; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"run_id":"` + uuid.New().String() + `","pending_messages":1,"pending_terminal":0,"stale_retired":0,"since":1}`)
	}
	b.WriteByte(']')
	if b.Len() <= maxOutboxReportBytes {
		t.Fatalf("fixture not over the byte cap (%d <= %d)", b.Len(), maxOutboxReportBytes)
	}
	if out := parseWorkerOutbox(json.RawMessage(b.String()), wid); out != nil {
		t.Fatalf("oversize report not dropped whole: got %d entries", len(out))
	}
}

func TestParseWorkerOutboxTooManyEntriesDropsWholeReport(t *testing.T) {
	wid := uuid.New()
	var b strings.Builder
	b.WriteByte('[')
	// One over the entry cap, with tiny entries so the byte cap is not what trips.
	for i := 0; i < maxOutboxEntries+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"run_id":"` + uuid.New().String() + `","pending_messages":1,"pending_terminal":0,"stale_retired":0,"since":1}`)
	}
	b.WriteByte(']')
	if b.Len() > maxOutboxReportBytes {
		t.Fatalf("fixture tripped the byte cap (%d) rather than the entry cap; this test must isolate the entry cap", b.Len())
	}
	if out := parseWorkerOutbox(json.RawMessage(b.String()), wid); out != nil {
		t.Fatalf("over-entry-cap report not dropped whole: got %d entries", len(out))
	}
}

func TestParseWorkerOutboxPerEntryDrops(t *testing.T) {
	wid := uuid.New()
	good := uuid.New()
	raw := `[
	  {"run_id":"not-a-uuid","pending_messages":1,"pending_terminal":0,"stale_retired":0,"since":1},
	  {"run_id":"` + uuid.New().String() + `","pending_messages":-1,"pending_terminal":0,"stale_retired":0,"since":1},
	  {"run_id":"` + uuid.New().String() + `","pending_messages":1,"pending_terminal":0,"stale_retired":0,"since":-5},
	  {"run_id":"` + uuid.New().String() + `","pending_messages":2000000000000,"pending_terminal":0,"stale_retired":0,"since":1},
	  {"run_id":"` + good.String() + `","pending_messages":2,"pending_terminal":0,"stale_retired":0,"since":100}
	]`
	out := parseWorkerOutbox(json.RawMessage(raw), wid)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1: bad uuid, negative count, negative since and an absurd count each drop only THAT entry", len(out))
	}
	if out[0].RunID != good || out[0].PendingMessages != 2 {
		t.Fatalf("survivor = %+v, want the one valid entry (%s, pending 2)", out[0], good)
	}
}

func TestParseWorkerOutboxWrongTypedEntryDropsOnlyThatEntry(t *testing.T) {
	wid := uuid.New()
	good := uuid.New()
	// A wrong-typed element must fail only its OWN per-entry decode, not abort the whole
	// array and discard the valid sibling with it. Here run_id is a JSON number and
	// blocked_reason is a JSON object — both are type errors against the entry struct.
	raw := `[
	  {"run_id":123,"pending_messages":1,"pending_terminal":0,"stale_retired":0,"since":1},
	  {"run_id":"` + uuid.New().String() + `","pending_messages":1,"pending_terminal":0,"stale_retired":0,"blocked_reason":{},"since":1},
	  {"run_id":"` + good.String() + `","pending_messages":4,"pending_terminal":0,"stale_retired":0,"since":100}
	]`
	out := parseWorkerOutbox(json.RawMessage(raw), wid)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1: a numeric run_id and an object blocked_reason each drop only THAT entry, not the whole report", len(out))
	}
	if out[0].RunID != good || out[0].PendingMessages != 4 {
		t.Fatalf("survivor = %+v, want the one valid entry (%s, pending 4)", out[0], good)
	}
}

func TestParseWorkerOutboxSanitizesBlockedReason(t *testing.T) {
	wid := uuid.New()
	r := uuid.New()
	// A bidi override (U+202E) must not survive into a CLI/web sink. Built from an
	// escape so no invisible byte lands in this source; json.Marshal produces the wire
	// bytes carrying the real rune.
	raw, err := json.Marshal([]map[string]any{{
		"run_id":           r.String(),
		"pending_messages": 1,
		"pending_terminal": 0,
		"stale_retired":    0,
		"blocked_reason":   "ab\u202ec",
		"since":            1,
	}})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	out := parseWorkerOutbox(raw, wid)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if got := out[0].BlockedReason; got != "abc" {
		t.Fatalf("blocked_reason = %q, want %q (control + format chars stripped)", got, "abc")
	}
}
