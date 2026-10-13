package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// storedNotePayload appends one progress_note message through the real AppendMessages path and
// returns the payload the store was asked to persist.
func storedNotePayload(t *testing.T, payload string) map[string]any {
	t.Helper()
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}
	svc := New(fs, newBox(t), testParams())
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
		[]IncomingMessage{{Seq: 3, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.insertedMessages) != 1 {
		t.Fatalf("inserted %d messages, want 1", len(fs.insertedMessages))
	}
	var out map[string]any
	if err := json.Unmarshal(fs.insertedMessages[0].Payload, &out); err != nil {
		t.Fatalf("stored payload is not JSON: %v", err)
	}
	return out
}

func TestProgressNoteIngestSanitisesAndCapsText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"plain text is kept", "Running the agent gate", "Running the agent gate"},
		{"control bytes are stripped", "Run\x00ning\x07 the\x1b gate\r\n", "Running the gate"},
		{"ANSI escape loses its ESC and stays inert text", "\x1b[31mred\x1b[0m alert", "[31mred[0m alert"},
		{"bidi overrides and isolates are stripped", "safe\u202egnp.exe\u2066 file\u200b", "safegnp.exe file"},
		{"markup stays as inert plain text", "<img src=x onerror=alert(1)> **bold** [x](javascript:y)", "<img src=x onerror=alert(1)> **bold** [x](javascript:y)"},
		{"surrounding whitespace is trimmed", "  hello  ", "hello"},
		{"capped at 120 runes", strings.Repeat("a", 119) + "bcd", strings.Repeat("a", 119) + "b"},
		{"the cap counts runes, not bytes", strings.Repeat("\U0001F600", 130), strings.Repeat("\U0001F600", 120)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"text": c.text, "milestone_id": "m1"})
			got := storedNotePayload(t, string(raw))
			if got["text"] != c.want {
				t.Fatalf("stored text = %q, want %q", got["text"], c.want)
			}
		})
	}
}

func TestProgressNoteIngestKeepsOnlyAllowedKeys(t *testing.T) {
	got := storedNotePayload(t, `{
		"text": "Reviewing",
		"milestone_id": "m\u202e1`+strings.Repeat("x", 100)+`",
		"event": "result",
		"usage": {"input_tokens": 99},
		"modelUsage": {"claude-haiku-4-5-20251001": {"inputTokens": 99}},
		"extra": "dropped",
		"model_usage": {"claude-haiku-4-5-20251001": {
			"inputTokens": 12, "outputTokens": 3, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0,
			"costUSD": 0.5, "service_tier": "standard", "unknown": 7}}
	}`)
	for _, banned := range []string{"event", "usage", "modelUsage", "extra"} {
		if _, ok := got[banned]; ok {
			t.Fatalf("stored payload kept the %q key: %v", banned, got)
		}
	}
	if id, _ := got["milestone_id"].(string); id != "m1"+strings.Repeat("x", 62) {
		t.Fatalf("milestone_id = %q, want bidi stripped and 64 runes", id)
	}
	mu, ok := got["model_usage"].(map[string]any)
	if !ok || len(mu) != 1 {
		t.Fatalf("model_usage = %v", got["model_usage"])
	}
	entry := mu["claude-haiku-4-5-20251001"].(map[string]any)
	if _, ok := entry["unknown"]; ok {
		t.Fatalf("model_usage entry kept an unknown key: %v", entry)
	}
	if entry["inputTokens"] != float64(12) || entry["costUSD"] != 0.5 || entry["service_tier"] != "standard" {
		t.Fatalf("model_usage entry lost an allowed field: %v", entry)
	}
}

func TestProgressNoteIngestNonObjectPayloadBecomesEmptyNote(t *testing.T) {
	for _, payload := range []string{`[1,2]`, `"text"`, `42`, `null`, `{"text": 5, "milestone_id": []}`} {
		got := storedNotePayload(t, payload)
		if got["text"] != "" || got["milestone_id"] != "" {
			t.Fatalf("payload %s stored as %v, want an empty note", payload, got)
		}
	}
}

// A usage-only note (empty text) still folds its usage: the spend is real even when the text
// is discarded.
func TestProgressNoteFoldUsageOnlyNote(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}
	svc := New(fs, newBox(t), testParams())
	payload := `{"text":"","milestone_id":"m1","model_usage":{"claude-haiku-4-5-20251001":{"inputTokens":100,"outputTokens":10}}}`
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
		[]IncomingMessage{{Seq: 7, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
		t.Fatal(err)
	}
	if len(fs.upsertedUsage) != 1 {
		t.Fatalf("upserts = %d, want 1", len(fs.upsertedUsage))
	}
	u := fs.upsertedUsage[0]
	if u.Model != "progress_note:claude-haiku-4-5-20251001" || u.LineageEpoch != 7 || u.LineageIndex != 0 || u.UsageBasis != "per_leg" {
		t.Fatalf("usage key = %q epoch %d index %d basis %s", u.Model, u.LineageEpoch, u.LineageIndex, u.UsageBasis)
	}
}

func TestProgressNoteClaudePricingAndUnreported(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		entry      string
		wantStatus string
		wantMicros int64
	}{
		{"priced from the standard table without a provider cost", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40}`, "metered", 600},
		{"a provider cost wins over the table", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40,"costUSD":0.01}`, "metered", 10000},
		{"an unknown model is unreported, never a metered zero", "claude-unknown-9",
			`{"inputTokens":400,"outputTokens":40}`, "unreported", 0},
		{"a non-standard tier is unreported", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40,"service_tier":"priority"}`, "unreported", 0},
		{"cache writes without a split are unreported", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50}`, "unreported", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := worker()
			fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}
			svc := New(fs, newBox(t), testParams())
			payload := `{"text":"x","milestone_id":"m1","model_usage":{"` + c.model + `":` + c.entry + `}}`
			if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
				[]IncomingMessage{{Seq: 4, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
				t.Fatal(err)
			}
			if len(fs.upsertedUsage) != 1 {
				t.Fatalf("upserts = %d, want 1", len(fs.upsertedUsage))
			}
			u := fs.upsertedUsage[0]
			if u.CostStatus != c.wantStatus {
				t.Fatalf("cost_status = %s, want %s", u.CostStatus, c.wantStatus)
			}
			if got := u.CostUsd.Int.Int64(); got != c.wantMicros {
				t.Fatalf("cost micros = %d, want %d", got, c.wantMicros)
			}
			if u.InputTokens != 400 || u.OutputTokens != 40 {
				t.Fatalf("tokens must survive an unreported cost, got in=%d out=%d", u.InputTokens, u.OutputTokens)
			}
		})
	}
}

// A Codex run honours the closed costStatus marker through deriveUsageCost, and a missing
// marker or cost is unreported, never a metered zero.
func TestProgressNoteCodexMarkerMatrix(t *testing.T) {
	cases := []struct {
		name       string
		entry      string
		wantStatus string
		wantMicros int64
	}{
		{"metered with cost", `{"inputTokens":100,"outputTokens":10,"costUSD":0.002,"costStatus":"metered"}`, "metered", 2000},
		{"subscription", `{"inputTokens":100,"outputTokens":10,"costStatus":"subscription"}`, "subscription", 0},
		{"no marker", `{"inputTokens":100,"outputTokens":10,"costUSD":0.002}`, "unreported", 0},
		{"metered without a cost", `{"inputTokens":100,"outputTokens":10,"costStatus":"metered"}`, "unreported", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := worker()
			fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), Harness: "codex"}}
			svc := New(fs, newBox(t), testParams())
			payload := `{"text":"x","milestone_id":"m1","model_usage":{"gpt-6-luna":` + c.entry + `}}`
			if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
				[]IncomingMessage{{Seq: 4, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
				t.Fatal(err)
			}
			u := fs.upsertedUsage[0]
			if u.Model != "progress_note:gpt-6-luna" || u.CostStatus != c.wantStatus || u.CostUsd.Int.Int64() != c.wantMicros {
				t.Fatalf("row = %s %s %d micros, want progress_note:gpt-6-luna %s %d", u.Model, u.CostStatus, u.CostUsd.Int.Int64(), c.wantStatus, c.wantMicros)
			}
			if u.Harness != "codex" {
				t.Fatalf("harness = %s", u.Harness)
			}
		})
	}
}

// quietSpy records the QuietActivity flag of each UpdateRunLastSeq call.
type quietSpy struct {
	*fakeStore
	quiet []bool
}

func (s *quietSpy) UpdateRunLastSeq(ctx context.Context, arg store.UpdateRunLastSeqParams) (int64, error) {
	s.quiet = append(s.quiet, arg.QuietActivity)
	return s.fakeStore.UpdateRunLastSeq(ctx, arg)
}

func TestProgressNoteOnlyBatchDoesNotCountAsActivity(t *testing.T) {
	note := json.RawMessage(`{"text":"x","milestone_id":"m1"}`)
	text := json.RawMessage(`{"text":"hi"}`)
	cases := []struct {
		name      string
		msgs      []IncomingMessage
		wantQuiet bool
	}{
		{"only notes", []IncomingMessage{{Seq: 5, Kind: "progress_note", Payload: note}, {Seq: 6, Kind: "progress_note", Payload: note}}, true},
		{"a note and a text frame", []IncomingMessage{{Seq: 5, Kind: "progress_note", Payload: note}, {Seq: 6, Kind: "text", Payload: text}}, false},
		{"only a text frame", []IncomingMessage{{Seq: 5, Kind: "text", Payload: text}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := worker()
			spy := &quietSpy{fakeStore: &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}}
			svc := New(spy, newBox(t), testParams())
			if err := svc.AppendMessages(context.Background(), w, spy.runOwned.ID, c.msgs); err != nil {
				t.Fatal(err)
			}
			if len(spy.quiet) != 1 || spy.quiet[0] != c.wantQuiet {
				t.Fatalf("QuietActivity calls = %v, want one call with %v", spy.quiet, c.wantQuiet)
			}
			if spy.lastSeqUpdated == nil || *spy.lastSeqUpdated != c.msgs[len(c.msgs)-1].Seq {
				t.Fatalf("last_seq must advance to the batch's highest seq, got %v", spy.lastSeqUpdated)
			}
		})
	}
}

// A Claude note with cache writes and the 5m/1h split, and no provider cost, is priced from the
// table (metered); the same note without the split stays unreported.
func TestProgressNoteClaudeCacheWriteSplitPricing(t *testing.T) {
	run := func(entry string) (string, int64) {
		t.Helper()
		w := worker()
		fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}
		svc := New(fs, newBox(t), testParams())
		payload := `{"text":"x","milestone_id":"m1","model_usage":{"claude-haiku-4-5-20251001":` + entry + `}}`
		if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
			[]IncomingMessage{{Seq: 4, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
			t.Fatal(err)
		}
		if len(fs.upsertedUsage) != 1 {
			t.Fatalf("upserts = %d, want 1", len(fs.upsertedUsage))
		}
		u := fs.upsertedUsage[0]
		return u.CostStatus, u.CostUsd.Int.Int64()
	}
	status, micros := run(`{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50,"cacheCreation5mInputTokens":30,"cacheCreation1hInputTokens":20}`)
	if status != "metered" || micros <= 600 {
		t.Fatalf("with split: status=%s micros=%d, want metered and above the no-cache price", status, micros)
	}
	if status, micros = run(`{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50}`); status != "unreported" || micros != 0 {
		t.Fatalf("without split: status=%s micros=%d, want unreported/0", status, micros)
	}
}

// The model cap keeps a deterministic subset: the first four sanitised names in sorted order,
// the same on every normalisation of the same payload.
func TestProgressNoteModelCapIsDeterministic(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{` +
		`"m6":{"inputTokens":6},"m3":{"inputTokens":3},"m1":{"inputTokens":1},"m5":{"inputTokens":5},"m2":{"inputTokens":2},"m4":{"inputTokens":4}}}`)
	first := string(normalizeProgressNotePayload(raw, harnessClaude))
	for i := 0; i < 50; i++ {
		if got := string(normalizeProgressNotePayload(raw, harnessClaude)); got != first {
			t.Fatalf("normalisation differs between runs:\n%s\n%s", first, got)
		}
	}
	var p progressNotePayload
	if err := json.Unmarshal([]byte(first), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.ModelUsage) != 4 {
		t.Fatalf("kept %d models, want 4", len(p.ModelUsage))
	}
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		if _, ok := p.ModelUsage[m]; !ok {
			t.Fatalf("model %s missing from %v", m, p.ModelUsage)
		}
	}
}

// An entry whose numeric fields are all zero or malformed is dropped, not folded as a zero row.
func TestProgressNoteDropsEmptyUsageEntry(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{"a":{"inputTokens":0},"b":{"inputTokens":"lots","outputTokens":-3},"c":{"outputTokens":5}}}`)
	var p progressNotePayload
	if err := json.Unmarshal(normalizeProgressNotePayload(raw, harnessClaude), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.ModelUsage) != 1 || p.ModelUsage["c"].OutputTokens != 5 {
		t.Fatalf("model_usage = %+v, want only c", p.ModelUsage)
	}
}

// A zero-token entry for a KNOWN priced model with no worker costUSD is dropped too: the
// server-resolved table price ($0) must not make it look like a cost claim, and it must not
// consume a cap slot a real entry needs.
func TestProgressNoteDropsZeroTokenKnownModelEntry(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{` +
		`"claude-haiku-4-5-20251001":{"inputTokens":0},` +
		`"m1":{"inputTokens":1},"m2":{"inputTokens":2},"m3":{"inputTokens":3},"m4":{"inputTokens":4}}}`)
	var p progressNotePayload
	if err := json.Unmarshal(normalizeProgressNotePayload(raw, harnessClaude), &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ModelUsage["claude-haiku-4-5-20251001"]; ok {
		t.Fatalf("zero-token known-model entry kept: %+v", p.ModelUsage)
	}
	if len(p.ModelUsage) != 4 {
		t.Fatalf("kept %d models, want the 4 real ones: %+v", len(p.ModelUsage), p.ModelUsage)
	}
}

// Two raw keys that sanitise to the same name keep the first valid entry in sorted raw-key
// order (" claude-haiku" sorts before "claude-haiku"), on every normalisation.
func TestProgressNoteCollidingKeysAreDeterministic(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{` +
		`"claude-haiku":{"inputTokens":2}," claude-haiku":{"inputTokens":1},"  claude-haiku":{"inputTokens":0}}}`)
	first := string(normalizeProgressNotePayload(raw, harnessClaude))
	for i := 0; i < 200; i++ {
		if got := string(normalizeProgressNotePayload(raw, harnessClaude)); got != first {
			t.Fatalf("normalisation differs between runs:\n%s\n%s", first, got)
		}
	}
	var p progressNotePayload
	if err := json.Unmarshal([]byte(first), &p); err != nil {
		t.Fatal(err)
	}
	// "  claude-haiku" sorts first but is invalid (all zero), so " claude-haiku" is the first valid.
	if len(p.ModelUsage) != 1 || p.ModelUsage["claude-haiku"].InputTokens != 1 {
		t.Fatalf("model_usage = %+v, want one claude-haiku with inputTokens 1", p.ModelUsage)
	}
}

// An invalid entry that sorts first does not consume a cap slot.
func TestProgressNoteInvalidEntryDoesNotConsumeCapSlot(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{` +
		`"a0":{"inputTokens":0},"m1":{"inputTokens":1},"m2":{"inputTokens":2},"m3":{"inputTokens":3},"m4":{"inputTokens":4},"m5":{"inputTokens":5}}}`)
	var p progressNotePayload
	if err := json.Unmarshal(normalizeProgressNotePayload(raw, harnessClaude), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.ModelUsage) != 4 {
		t.Fatalf("kept %d models, want 4: %+v", len(p.ModelUsage), p.ModelUsage)
	}
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		if _, ok := p.ModelUsage[m]; !ok {
			t.Fatalf("model %s missing from %+v", m, p.ModelUsage)
		}
	}
}

// A zero-token entry that carries a provider cost is kept.
func TestProgressNoteKeepsZeroTokenEntryWithCost(t *testing.T) {
	raw := json.RawMessage(`{"text":"x","milestone_id":"m","model_usage":{"m":{"costUSD":0.01}}}`)
	var p progressNotePayload
	if err := json.Unmarshal(normalizeProgressNotePayload(raw, harnessClaude), &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ModelUsage["m"]; !ok || len(p.ModelUsage) != 1 {
		t.Fatalf("model_usage = %+v, want the costed zero-token entry kept", p.ModelUsage)
	}
}

// Exact cache-write split prices for claude-haiku-4-5-20251001 (USD/MTok: in 1, out 5, 5m write
// 1.25, 1h write 2), in micro-dollars. Swapping the 5m and 1h fields changes the price.
func TestProgressNoteCacheWriteSplitExactPrice(t *testing.T) {
	cases := []struct {
		name       string
		entry      string
		wantStatus string
		wantMicros int64
	}{
		// 400*1 + 40*5 + 30*1.25 + 20*2 = 677.5, rounded to 678.
		{"both halves", `{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50,"cacheCreation5mInputTokens":30,"cacheCreation1hInputTokens":20}`, "metered", 678},
		// 400 + 200 + 40*1.25 = 650.
		{"lone 5m covering the total", `{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":40,"cacheCreation5mInputTokens":40}`, "metered", 650},
		// 400 + 200 + 40*2 = 680.
		{"lone 1h covering the total", `{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":40,"cacheCreation1hInputTokens":40}`, "metered", 680},
		// A lone half that does not account for the whole cache-write total cannot be priced.
		{"lone 5m short of the total", `{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50,"cacheCreation5mInputTokens":30}`, "unreported", 0},
		{"lone 1h short of the total", `{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50,"cacheCreation1hInputTokens":30}`, "unreported", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := worker()
			fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID)}}
			svc := New(fs, newBox(t), testParams())
			payload := `{"text":"x","milestone_id":"m1","model_usage":{"claude-haiku-4-5-20251001":` + c.entry + `}}`
			if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
				[]IncomingMessage{{Seq: 4, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
				t.Fatal(err)
			}
			if len(fs.upsertedUsage) != 1 {
				t.Fatalf("upserts = %d, want 1", len(fs.upsertedUsage))
			}
			u := fs.upsertedUsage[0]
			if u.CostStatus != c.wantStatus || u.CostUsd.Int.Int64() != c.wantMicros {
				t.Fatalf("status=%s micros=%d, want %s/%d", u.CostStatus, u.CostUsd.Int.Int64(), c.wantStatus, c.wantMicros)
			}
		})
	}
}

// The stored note entry carries the server-resolved cost (costStatus always, costUSD only when
// metered, never a zero on an unreported entry), and re-feeding the stored payload through
// AppendMessages (normalise again, then fold) gives the same run_usage row as the raw one:
// fold(normalize(stored)) == fold(raw). The RefoldRunUsage read path itself is covered by the
// LiveDB TestProgressNoteRefoldEquivalenceLiveDB.
func TestProgressNoteStoredEntryCarriesResolvedCost(t *testing.T) {
	cases := []struct {
		name       string
		harness    string
		model      string
		entry      string
		wantStatus string
		wantUSD    any // nil: costUSD must be absent
	}{
		{"claude priced from the table", "claude", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40}`, "metered", 0.0006},
		{"claude provider cost wins", "claude", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40,"costUSD":0.01}`, "metered", 0.01},
		{"claude unpriced model", "claude", "claude-unknown-9",
			`{"inputTokens":400,"outputTokens":40}`, "unreported", nil},
		{"codex metered with cost", "codex", "gpt-6-luna",
			`{"inputTokens":100,"outputTokens":10,"costUSD":0.002,"costStatus":"metered"}`, "metered", 0.002},
		{"codex subscription", "codex", "gpt-6-luna",
			`{"inputTokens":100,"outputTokens":10,"costUSD":0.002,"costStatus":"subscription"}`, "subscription", nil},
		{"codex unreported", "codex", "gpt-6-luna",
			`{"inputTokens":100,"outputTokens":10}`, "unreported", nil},
	}
	appendNote := func(t *testing.T, harness, payload string) (json.RawMessage, store.UpsertRunUsageParams) {
		t.Helper()
		w := worker()
		fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), Harness: harness}}
		svc := New(fs, newBox(t), testParams())
		if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID,
			[]IncomingMessage{{Seq: 4, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(payload)}}); err != nil {
			t.Fatal(err)
		}
		if len(fs.insertedMessages) != 1 || len(fs.upsertedUsage) != 1 {
			t.Fatalf("inserted %d messages, %d upserts; want 1 and 1", len(fs.insertedMessages), len(fs.upsertedUsage))
		}
		return fs.insertedMessages[0].Payload, fs.upsertedUsage[0]
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := `{"text":"x","milestone_id":"m1","model_usage":{"` + c.model + `":` + c.entry + `}}`
			stored, row := appendNote(t, c.harness, raw)
			var top map[string]json.RawMessage
			if err := json.Unmarshal(stored, &top); err != nil {
				t.Fatal(err)
			}
			var mu map[string]map[string]any
			if err := json.Unmarshal(top["model_usage"], &mu); err != nil {
				t.Fatal(err)
			}
			e := mu[c.model]
			if e["costStatus"] != c.wantStatus {
				t.Fatalf("stored costStatus = %v, want %s (payload %s)", e["costStatus"], c.wantStatus, stored)
			}
			// An absent costUSD is stored as JSON null (resultModelUsage.CostUSD has no omitempty).
			got := e["costUSD"]
			if c.wantUSD == nil && got != nil {
				t.Fatalf("stored costUSD = %v on a %s entry, want none", got, c.wantStatus)
			}
			if c.wantUSD != nil && got != c.wantUSD {
				t.Fatalf("stored costUSD = %v, want %v", got, c.wantUSD)
			}
			_, refolded := appendNote(t, c.harness, string(stored))
			if refolded.CostStatus != row.CostStatus || refolded.CostUsd.Int.Cmp(row.CostUsd.Int) != 0 ||
				refolded.InputTokens != row.InputTokens || refolded.OutputTokens != row.OutputTokens {
				t.Fatalf("fold(stored) = %s/%v/%d/%d, fold(raw) = %s/%v/%d/%d",
					refolded.CostStatus, refolded.CostUsd.Int, refolded.InputTokens, refolded.OutputTokens,
					row.CostStatus, row.CostUsd.Int, row.InputTokens, row.OutputTokens)
			}
			if row.CostStatus != c.wantStatus {
				t.Fatalf("row cost_status = %s, want %s", row.CostStatus, c.wantStatus)
			}
		})
	}
}
