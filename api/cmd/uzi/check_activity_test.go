package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Cross-check activity evidence in the CLI (PRD #3166): the `run logs` row wording and the
// `ACTIVITY` line of `run get`, keyed on the run's claim generation.

func statusMsg(seq int32, payload string) apitypes.MessageDTO {
	agent := "cross-checker"
	return apitypes.MessageDTO{Seq: seq, Kind: "status", Agent: &agent, Payload: json.RawMessage(payload)}
}

func summaryPayload(gen int, reads, searches int, complete bool) string {
	return fmt.Sprintf(`{"event":"check_activity_summary","generation":%d,"family":"claude","stage":"verdict",`+
		`"reads":{"ok":%d,"denied":0,"oversize":0,"limit":0,"error":0},`+
		`"searches":{"ok":%d,"denied":0,"oversize":0,"limit":0,"error":0},`+
		`"dropped":0,"untracked":0,"unsettled":0,"complete":%t}`, gen, reads, searches, complete)
}

func markerPayload(gen int) string {
	return fmt.Sprintf(`{"event":"check_activity_start","v":1,"cap":200,"generation":%d}`, gen)
}

func logsOutput(t *testing.T, payloads ...string) string {
	t.Helper()
	var msgs []apitypes.MessageDTO
	for i, p := range payloads {
		msgs = append(msgs, statusMsg(int32(i+1), p))
	}
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": msgs}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}

func TestRunLogsRendersActivityEvents(t *testing.T) {
	out := logsOutput(t,
		markerPayload(2),
		`{"event":"check_activity","op":"read","outcome":"ok","path":"api/x.go","lines":[1,200]}`,
		`{"event":"check_activity","op":"search","outcome":"ok","path":"api/internal/forge","matches":3}`,
		`{"event":"check_activity","op":"search","outcome":"ok","path":".","matches":1}`,
		`{"event":"check_activity","op":"search","outcome":"limit","path":"web","matches":50,"matches_truncated":true}`,
		`{"event":"check_activity","op":"read","outcome":"denied","category":"outside_checkout"}`,
		`{"event":"check_activity","op":"read","outcome":"oversize","path":"big.bin"}`,
		summaryPayload(2, 4, 0, true),
		summaryPayload(2, 1, 1, false),
	)
	for _, want := range []string{
		"activity capture started",
		"read api/x.go L1-200 · ok",
		"search api/internal/forge · 3 matches · ok",
		"search . · 1 match · ok",
		"search web · 50+ matches · limit",
		"read denied (outside checkout)",
		"read big.bin · oversize",
		"activity summary: 4 reads, 0 searches, 0 dropped, complete",
		"activity summary: 1 read, 1 search, 0 dropped, incomplete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run logs output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"event"`) {
		t.Errorf("activity rows must not fall back to the raw payload:\n%s", out)
	}
}

func TestRunLogsActivityPathIsInert(t *testing.T) {
	// ESC, a newline, a bidi override and a zero-width space inside the path.
	out := logsOutput(t,
		`{"event":"check_activity","op":"read","outcome":"ok","path":"a\u001b[2Jb\nFORGED\u202ec\u200bd.go","lines":[1,2]}`)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsAny(out, "\u202e\u200b") {
		t.Errorf("control or bidi characters reached the terminal: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("an injected newline split the row into %d lines: %q", len(lines), out)
	}
}

func TestRunLogsActivityUnknownValuesFallBackToGeneric(t *testing.T) {
	out := logsOutput(t,
		`{"event":"check_activity","op":"exec","outcome":"ok","path":"x"}`,
		`{"event":"check_activity","op":"read","outcome":"weird","path":"y"}`,
		// An event this client does not know renders through compactPayload, unchanged.
		`{"event":"check_activity_v2","detail":"future"}`,
	)
	for _, want := range []string{`"op":"exec"`, `"outcome":"weird"`, `"event":"check_activity_v2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected generic payload row containing %s:\n%s", want, out)
		}
	}
}

func TestRunLogsActivityUnknownCategoryDropped(t *testing.T) {
	out := logsOutput(t, `{"event":"check_activity","op":"read","outcome":"denied","category":"\u001b[31mevil"}`)
	if !strings.Contains(out, "read denied") || strings.Contains(out, "evil") {
		t.Errorf("unknown category must be dropped: %q", out)
	}
}

func TestRunLogsJSONModeUnchangedForActivity(t *testing.T) {
	payload := `{"event":"check_activity","op":"read","outcome":"ok","path":"api/x.go"}`
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {statusMsg(1, payload)}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, `"payload":`+payload) {
		t.Errorf("--json must carry the payload verbatim: %s", out)
	}
}

func activityGet(t *testing.T, run apitypes.RunDTO, payloads ...string) string {
	t.Helper()
	var msgs []apitypes.MessageDTO
	for i, p := range payloads {
		msgs = append(msgs, statusMsg(int32(i+1), p))
	}
	run.ID, run.Kind = "r1", "cross_check"
	fc := &uzicli.FakeClient{
		RunByID:  map[string]apitypes.RunDTO{"r1": run},
		LogsByID: map[string][]apitypes.MessageDTO{"r1": msgs},
	}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}

func activityRow(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "ACTIVITY") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "ACTIVITY"))
		}
	}
	return ""
}

func TestRunGetActivityStates(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	gen := func(n int64) *int64 { return &n }
	cases := []struct {
		name     string
		run      apitypes.RunDTO
		payloads []string
		want     string // "" = no ACTIVITY row
	}{
		{"complete", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 4, 0, true)}, "4 reads, 0 searches, 0 dropped, complete"},
		{"incomplete summary", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 1, 1, false)}, "1 read, 1 search, 0 dropped, incomplete"},
		{"marker only, terminal", apitypes.RunDTO{Status: "failed", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(1)}, "activity capture incomplete"},
		{"marker only, running", apitypes.RunDTO{Status: "running", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(1)}, "activity capture in progress"},
		{"nothing, terminal and claimed", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(1), FirstStartedAt: &started},
			nil, "activity capture unavailable"},
		{"nothing, running", apitypes.RunDTO{Status: "running", ClaimGeneration: gen(1), FirstStartedAt: &started},
			nil, ""},
		{"nothing, never claimed", apitypes.RunDTO{Status: "failed", ClaimGeneration: gen(0)},
			nil, ""},
		// REQUIRED regression: an older attempt's complete summary must never read as current.
		{"older attempt complete, current has nothing", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(2), FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 4, 0, true)},
			"activity capture unavailable; earlier attempt (generation 1): 4 reads, 0 searches, 0 dropped, complete"},
		{"older attempt, current running without marker", apitypes.RunDTO{Status: "running", ClaimGeneration: gen(2), FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 4, 0, true)},
			"earlier attempt (generation 1): 4 reads, 0 searches, 0 dropped, complete"},
		{"older attempt, current marker only", apitypes.RunDTO{Status: "running", ClaimGeneration: gen(2), FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 4, 0, true), markerPayload(2)},
			"activity capture in progress; earlier attempt (generation 1): 4 reads, 0 searches, 0 dropped, complete"},
		{"current summary wins over older", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(2), FirstStartedAt: &started},
			[]string{summaryPayload(1, 9, 9, true), markerPayload(2), summaryPayload(2, 2, 0, true)},
			"2 reads, 0 searches, 0 dropped, complete"},
		// A re-claim between the two reads: a higher-generation summary is not an earlier attempt.
		{"newer generation summary is ignored", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(2), summaryPayload(2, 4, 0, true)}, "activity capture unavailable"},
		{"newer marker is ignored", apitypes.RunDTO{Status: "running", ClaimGeneration: gen(1), FirstStartedAt: &started},
			[]string{markerPayload(2)}, ""},
		// JSON null generation is absent, not generation 0.
		{"null generation summary has no provenance", apitypes.RunDTO{Status: "completed", ClaimGeneration: gen(0), FirstStartedAt: &started},
			[]string{strings.Replace(summaryPayload(0, 4, 0, true), `"generation":0`, `"generation":null`, 1)}, "activity capture unavailable"},
		{"claim_generation absent", apitypes.RunDTO{Status: "completed", FirstStartedAt: &started},
			[]string{markerPayload(1), summaryPayload(1, 4, 0, true)},
			"historical; current capture unknown: 4 reads, 0 searches, 0 dropped, complete"},
		{"claim_generation absent, no summary", apitypes.RunDTO{Status: "completed", FirstStartedAt: &started},
			[]string{markerPayload(1)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := activityGet(t, tc.run, tc.payloads...)
			if got := activityRow(out); got != tc.want {
				t.Errorf("ACTIVITY = %q, want %q\n%s", got, tc.want, out)
			}
		})
	}
}

func TestRunGetActivitySkippedWhenNotApplicable(t *testing.T) {
	g := int64(1)
	started := time.Now()
	payloads := []apitypes.MessageDTO{statusMsg(1, summaryPayload(1, 4, 0, true))}
	run := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "completed", ClaimGeneration: &g, FirstStartedAt: &started}
	fc := &uzicli.FakeClient{
		RunByID:  map[string]apitypes.RunDTO{"r1": run},
		LogsByID: map[string][]apitypes.MessageDTO{"r1": payloads},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK || strings.Contains(out, "ACTIVITY") {
		t.Errorf("a non cross_check run must print no ACTIVITY row (exit %d):\n%s", code, out)
	}
	// JSON mode prints nothing extra.
	run.Kind = "cross_check"
	fc.RunByID["r1"] = run
	out, _, code = runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
	if code != uzicli.ExitOK || strings.Contains(out, "ACTIVITY") {
		t.Errorf("--json must print no ACTIVITY row (exit %d):\n%s", code, out)
	}
}

func TestRunGetActivityLogReadErrorPrintsNothingExtra(t *testing.T) {
	g := int64(1)
	started := time.Now()
	run := apitypes.RunDTO{ID: "r1", Kind: "cross_check", Status: "completed", ClaimGeneration: &g, FirstStartedAt: &started}
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"r1": run},
		RunLogsHook: func(string, int32) ([]apitypes.MessageDTO, error) {
			return nil, fmt.Errorf("boom")
		},
	}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK || strings.Contains(out, "ACTIVITY") || strings.Contains(stderr, "boom") {
		t.Errorf("a log read error must be silent (exit %d):\n%s\n%s", code, out, stderr)
	}
}

// A JSON null in a numeric slot is absent, not 0 (json.Unmarshal of null is a silent no-op).
func TestRunLogsActivityNullNumbersAreAbsent(t *testing.T) {
	out := logsOutput(t,
		`{"event":"check_activity","op":"search","outcome":"ok","path":"web","matches":null}`,
		`{"event":"check_activity","op":"read","outcome":"ok","path":"a.go","lines":[null,null]}`,
	)
	if strings.Contains(out, "0 matches") || strings.Contains(out, "L0-0") {
		t.Errorf("null must not render as 0:\n%s", out)
	}
	for _, want := range []string{"search web \u00b7 ok", "read a.go \u00b7 ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
