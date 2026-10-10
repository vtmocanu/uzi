package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// appendSummaryUsage appends the given messages through the real AppendMessages path for a run
// on the given harness and returns the fake store.
func appendSummaryUsage(t *testing.T, harness string, msgs ...IncomingMessage) *fakeStore {
	t.Helper()
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), Harness: harness}}
	svc := New(fs, newBox(t), testParams())
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	return fs
}

func summaryMsg(seq int32, payload string) IncomingMessage {
	return IncomingMessage{Seq: seq, Kind: "summary_usage", Agent: "worker", Payload: json.RawMessage(payload)}
}

func storedSummaryPayload(t *testing.T, payload string) map[string]any {
	t.Helper()
	fs := appendSummaryUsage(t, harnessClaude, summaryMsg(3, payload))
	if len(fs.insertedMessages) != 1 {
		t.Fatalf("inserted %d messages, want 1", len(fs.insertedMessages))
	}
	var out map[string]any
	if err := json.Unmarshal(fs.insertedMessages[0].Payload, &out); err != nil {
		t.Fatalf("stored payload is not JSON: %v", err)
	}
	return out
}

func TestSummaryUsageIngestKeepsOnlyAllowedKeys(t *testing.T) {
	got := storedSummaryPayload(t, `{
		"pass": "plan",
		"event": "result",
		"usage": {"input_tokens": 99},
		"modelUsage": {"claude-haiku-4-5-20251001": {"inputTokens": 99}},
		"text": "dropped",
		"model_usage": {"claude-haiku-4-5-20251001": {
			"inputTokens": 12, "outputTokens": 3, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0,
			"costUSD": 0.5, "service_tier": "standard", "unknown": 7}}
	}`)
	for _, banned := range []string{"event", "usage", "modelUsage", "text"} {
		if _, ok := got[banned]; ok {
			t.Fatalf("stored payload kept the %q key: %v", banned, got)
		}
	}
	if got["pass"] != "plan" {
		t.Fatalf("pass = %v, want plan", got["pass"])
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

// A hostile payload that claims event:"result" must not reach the result-frame fold: the
// only upsert is the summary_pass row, never a row keyed by the bare model.
func TestSummaryUsageEventResultIsNotFoldedAsResultFrame(t *testing.T) {
	fs := appendSummaryUsage(t, harnessClaude, summaryMsg(3, `{"pass":"intent","event":"result",
		"usage":{"input_tokens":5},"modelUsage":{"claude-opus-4-8":{"inputTokens":1000,"costUSD":9}},
		"model_usage":{"claude-haiku-4-5-20251001":{"inputTokens":10,"outputTokens":2}}}`))
	if len(fs.upsertedUsage) != 1 || fs.upsertedUsage[0].Model != "summary_pass:claude-haiku-4-5-20251001" {
		t.Fatalf("upserts = %+v, want exactly the summary_pass row", fs.upsertedUsage)
	}
}

func TestSummaryUsagePassAllowlist(t *testing.T) {
	cases := []struct{ in, want string }{
		{"intent", "intent"},
		{"plan", "plan"},
		{"pr_description", "pr_description"},
		{"Plan", ""},
		{"now", ""},
		{"intent‮", ""},
		{"", ""},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]any{"pass": c.in, "model_usage": map[string]any{"m": map[string]any{"inputTokens": 1}}})
		if got := storedSummaryPayload(t, string(raw))["pass"]; got != c.want {
			t.Errorf("pass %q stored as %v, want %q", c.in, got, c.want)
		}
	}
	for _, payload := range []string{`{"pass": 5}`, `{"pass": ["plan"]}`, `{}`} {
		if got := storedSummaryPayload(t, payload)["pass"]; got != "" {
			t.Errorf("payload %s stored pass %v, want empty", payload, got)
		}
	}
}

func TestSummaryUsageNonObjectPayloadBecomesEmpty(t *testing.T) {
	for _, payload := range []string{`[1,2]`, `"x"`, `42`, `null`} {
		fs := appendSummaryUsage(t, harnessClaude, summaryMsg(3, payload))
		if len(fs.upsertedUsage) != 0 {
			t.Fatalf("payload %s folded %d rows, want none", payload, len(fs.upsertedUsage))
		}
		var out map[string]any
		if err := json.Unmarshal(fs.insertedMessages[0].Payload, &out); err != nil || out["pass"] != "" {
			t.Fatalf("payload %s stored as %s", payload, fs.insertedMessages[0].Payload)
		}
	}
}

// Each pass folds under its OWN key (summary_pass:<model>, epoch = its seq), so it collapses
// neither into the run's result frame, nor into a progress_note naming the same model, nor
// into another pass of the same leg; two pr_description passes are two rows.
func TestSummaryUsageFoldsUnderItsOwnKey(t *testing.T) {
	const haiku = "claude-haiku-4-5-20251001"
	entry := func(in, out int) string {
		b, _ := json.Marshal(map[string]any{haiku: map[string]any{"inputTokens": in, "outputTokens": out}})
		return string(b)
	}
	fs := appendSummaryUsage(t, harnessClaude,
		IncomingMessage{Seq: 1, Kind: "status", Agent: "lead", Payload: json.RawMessage(`{"event":"init","model":"claude-opus-4-8"}`)},
		IncomingMessage{Seq: 5, Kind: "status", Agent: "lead", Payload: json.RawMessage(`{"event":"result","modelUsage":{"` + haiku + `":{"inputTokens":700,"outputTokens":300,"costUSD":0.001}}}`)},
		IncomingMessage{Seq: 6, Kind: "progress_note", Agent: "worker", Payload: json.RawMessage(`{"text":"x","milestone_id":"m1","model_usage":` + entry(400, 40) + `}`)},
		summaryMsg(7, `{"pass":"intent","model_usage":`+entry(1200, 150)+`}`),
		summaryMsg(11, `{"pass":"pr_description","model_usage":`+entry(3000, 500)+`}`),
		summaryMsg(14, `{"pass":"pr_description","model_usage":`+entry(3500, 700)+`}`),
	)
	type key struct {
		model string
		epoch int32
	}
	got := map[key]store.UpsertRunUsageParams{}
	for _, u := range fs.upsertedUsage {
		k := key{u.Model, u.LineageEpoch}
		if _, dup := got[k]; dup {
			t.Fatalf("two upserts share the key %+v: they would collapse", k)
		}
		got[k] = u
	}
	want := []struct {
		k  key
		in int64
	}{
		{key{haiku, 1}, 700},
		{key{"progress_note:" + haiku, 6}, 400},
		{key{"summary_pass:" + haiku, 7}, 1200},
		{key{"summary_pass:" + haiku, 11}, 3000},
		{key{"summary_pass:" + haiku, 14}, 3500},
	}
	if len(got) < len(want) {
		t.Fatalf("got %d rows (%v), want at least %d", len(got), got, len(want))
	}
	for _, w := range want {
		u, ok := got[w.k]
		if !ok {
			t.Fatalf("no row for %+v", w.k)
		}
		if u.InputTokens != w.in {
			t.Errorf("%+v input = %d, want %d", w.k, u.InputTokens, w.in)
		}
	}
	for k, u := range got {
		if len(k.model) > len("summary_pass:") && k.model[:len("summary_pass:")] == "summary_pass:" {
			if u.LineageIndex != 0 || u.UsageBasis != usageBasisPerLeg || u.Harness != harnessClaude {
				t.Errorf("%+v index/basis/harness = %d/%s/%s", k, u.LineageIndex, u.UsageBasis, u.Harness)
			}
		}
	}
}

func TestSummaryUsageClaudePricingAndUnreported(t *testing.T) {
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
		{"cache writes without a split are unreported", "claude-haiku-4-5-20251001",
			`{"inputTokens":400,"outputTokens":40,"cacheCreationInputTokens":50}`, "unreported", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := appendSummaryUsage(t, harnessClaude, summaryMsg(4, `{"pass":"plan","model_usage":{"`+c.model+`":`+c.entry+`}}`))
			if len(fs.upsertedUsage) != 1 {
				t.Fatalf("upserts = %d, want 1", len(fs.upsertedUsage))
			}
			u := fs.upsertedUsage[0]
			if u.CostStatus != c.wantStatus || u.CostUsd.Int.Int64() != c.wantMicros {
				t.Fatalf("status=%s micros=%d, want %s/%d", u.CostStatus, u.CostUsd.Int.Int64(), c.wantStatus, c.wantMicros)
			}
			if u.InputTokens != 400 || u.OutputTokens != 40 {
				t.Fatalf("tokens must survive an unreported cost, got in=%d out=%d", u.InputTokens, u.OutputTokens)
			}
			// The stored entry carries the resolved cost and re-folds to the same row.
			var stored summaryUsagePayload
			if err := json.Unmarshal(fs.insertedMessages[0].Payload, &stored); err != nil {
				t.Fatal(err)
			}
			refolded := appendSummaryUsage(t, harnessClaude, summaryMsg(4, string(fs.insertedMessages[0].Payload)))
			r := refolded.upsertedUsage[0]
			if r.CostStatus != u.CostStatus || r.CostUsd.Int.Cmp(u.CostUsd.Int) != 0 || r.InputTokens != u.InputTokens {
				t.Fatalf("fold(stored) = %s/%v/%d, fold(raw) = %s/%v/%d", r.CostStatus, r.CostUsd.Int, r.InputTokens, u.CostStatus, u.CostUsd.Int, u.InputTokens)
			}
		})
	}
}

func TestSummaryUsageCodexMarkerMatrix(t *testing.T) {
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
			fs := appendSummaryUsage(t, "codex", summaryMsg(4, `{"pass":"pr_description","model_usage":{"gpt-6-luna":`+c.entry+`}}`))
			u := fs.upsertedUsage[0]
			if u.Model != "summary_pass:gpt-6-luna" || u.CostStatus != c.wantStatus || u.CostUsd.Int.Int64() != c.wantMicros || u.Harness != "codex" {
				t.Fatalf("row = %s %s %d %s", u.Model, u.CostStatus, u.CostUsd.Int.Int64(), u.Harness)
			}
		})
	}
}

// The model name is capped so the prefixed key still fits maxUsageModelRunes.
func TestSummaryUsageLongModelNameKeepsPrefixedKeyWithinCap(t *testing.T) {
	long := ""
	for i := 0; i < maxUsageModelRunes+20; i++ {
		long += "a"
	}
	fs := appendSummaryUsage(t, harnessClaude, summaryMsg(4, `{"pass":"plan","model_usage":{"`+long+`":{"inputTokens":5}}}`))
	if len(fs.upsertedUsage) != 1 {
		t.Fatalf("upserts = %d", len(fs.upsertedUsage))
	}
	if m := fs.upsertedUsage[0].Model; len(m) != maxUsageModelRunes || m[:len("summary_pass:")] != "summary_pass:" {
		t.Fatalf("model key len %d = %q", len(m), m)
	}
}

func TestSummaryUsageOnlyBatchDoesNotCountAsActivity(t *testing.T) {
	sum := json.RawMessage(`{"pass":"plan","model_usage":{"m":{"inputTokens":1}}}`)
	note := json.RawMessage(`{"text":"x","milestone_id":"m1"}`)
	text := json.RawMessage(`{"text":"hi"}`)
	cases := []struct {
		name      string
		msgs      []IncomingMessage
		wantQuiet bool
	}{
		{"only summary usage", []IncomingMessage{{Seq: 5, Kind: "summary_usage", Payload: sum}, {Seq: 6, Kind: "summary_usage", Payload: sum}}, true},
		{"summary usage and a note", []IncomingMessage{{Seq: 5, Kind: "summary_usage", Payload: sum}, {Seq: 6, Kind: "progress_note", Payload: note}}, true},
		{"summary usage and a text frame", []IncomingMessage{{Seq: 5, Kind: "summary_usage", Payload: sum}, {Seq: 6, Kind: "text", Payload: text}}, false},
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
		})
	}
}
