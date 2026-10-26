package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func cliCoverageFixture(t *testing.T) apitypes.CodeCrossCheckCoverage {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/code-cross-check/coverage-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var account apitypes.CodeCrossCheckCoverage
	if json.Unmarshal(raw, &account) != nil {
		t.Fatal("invalid coverage fixture")
	}
	return account
}

func TestCodeCrossCheckCoverageFullDisplayAndJSON(t *testing.T) {
	account := cliCoverageFixture(t)
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	summary := &apitypes.CodeCrossCheck{Stage: "code", Outcome: "completed", Findings: json.RawMessage("[]"), Coverage: raw}
	out := codeCheckCommand(t, summary)
	for _, expected := range []string{"12 stored paths: 9 examined, 0 excluded, 3 unassessed", "deleted.ts", "Reviewed patch", "0 unknown, 0 invalid, 0 omitted"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	var decoded apitypes.RunDTO
	if json.Unmarshal([]byte(codeCheckCommand(t, summary, "--json")), &decoded) != nil || decoded.CodeCrossCheckSummary == nil {
		t.Fatal("invalid run JSON")
	}
	var got apitypes.CodeCrossCheckCoverage
	if json.Unmarshal(decoded.CodeCrossCheckSummary.Coverage, &got) != nil || !reflect.DeepEqual(got, account) {
		t.Fatal("JSON lost full coverage account")
	}
}

func TestCodeCrossCheckCoverageIncompleteAndMalformed(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		if got := codeCoverageRows("CODE_CHECK", raw); got[0][1] != "coverage not reported" {
			t.Fatal("legacy coverage invented")
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage("{}"), json.RawMessage("false"), json.RawMessage(`{"version":1,"entries":[null]}`)} {
		if got := codeCoverageRows("CODE_CHECK", raw); got[0][1] != "Coverage unavailable" {
			t.Fatal("invalid account rendered as complete")
		}
	}
	for _, tc := range []struct {
		exact   bool
		count   int64
		message string
	}{
		{true, 7, "7 paths omitted"}, {false, 7, "at least 7 paths omitted; total unknown"}, {false, 0, "omitted path count unknown"},
	} {
		account := cliCoverageFixture(t)
		account.InventoryIncomplete = true
		account.OmittedCountExact = tc.exact
		account.OmittedPaths = tc.count
		raw, _ := json.Marshal(account)
		found := false
		for _, row := range codeCoverageRows("EARLIER_CODE_CHECK", raw) {
			if strings.Contains(row[1], tc.message) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing omission notice %q", tc.message)
		}
	}
}

func TestCodeCrossCheckCoverageFullFieldsAreTerminalSafe(t *testing.T) {
	account := cliCoverageFixture(t)
	account.Entries = []apitypes.CodeCrossCheckCoverageEntry{{Path: strings.Repeat("z", 512), Status: "excluded", Reason: strings.Repeat("r", 256)}}
	raw, _ := json.Marshal(account)
	rows := codeCoverageRows("CODE_CHECK", raw)
	fullPath, fullReason := false, false
	for _, row := range rows {
		fullPath = fullPath || row[1] == strings.Repeat("z", 512)
		fullReason = fullReason || row[1] == strings.Repeat("r", 256)
	}
	if !fullPath || !fullReason {
		t.Fatal("bounded stored fields were truncated")
	}
	account.Entries[0].Path = "<img>" + string(rune(27)) + "[31m" + string(rune(0x202e))
	account.Entries[0].Reason = "[bad](javascript:alert(1))" + string(rune(27)) + "]0;hidden" + string(rune(7)) + string(rune(0x202e))
	raw, _ = json.Marshal(account)
	for _, row := range codeCoverageRows("EARLIER_CODE_CHECK", raw) {
		if strings.ContainsAny(row[1], string([]rune{27, 7, 0x202e})) {
			t.Fatal("terminal control escaped renderer")
		}
	}
}

func codeCheckCommand(t *testing.T, s *apitypes.CodeCrossCheck, args ...string) string {
	t.Helper()
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Kind: "issue", CodeCrossCheckRequired: true, CodeCrossCheckSummary: s},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run", "get", "r1"}, args...)...)
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}

func TestRunGetCodeCrossCheckStates(t *testing.T) {
	now := time.Now()
	badHead := "not-a-sha"
	for _, tc := range []struct {
		name string
		s    *apitypes.CodeCrossCheck
		want string
	}{
		{"missing", nil, "Pending"},
		{"pending", &apitypes.CodeCrossCheck{Outcome: "pending"}, "Pending"},
		{"both NULL failed", &apitypes.CodeCrossCheck{Outcome: "failed"}, "Incomplete"},
		{"malformed head", &apitypes.CodeCrossCheck{Outcome: "failed", HeadCommit: &badHead}, "Incomplete"},
		{"no findings", &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage("[]")}, "No findings recorded"},
		{"historical", &apitypes.CodeCrossCheck{Outcome: "completed", InterruptedAt: &now, Findings: json.RawMessage("[]")}, "Earlier attempt evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := codeCheckCommand(t, tc.s)
			if !strings.Contains(out, tc.want) || strings.Contains(out, "CHECKED_SHA") {
				t.Fatalf("wrong evidence: %s", out)
			}
			if tc.name == "historical" && (strings.Contains(out, "Completed") || strings.Contains(out, "\nCODE_CHECK")) {
				t.Fatalf("history presented as active: %s", out)
			}
		})
	}
}

func TestRunGetCodeCrossCheckMissingTerminalOutcome(t *testing.T) {
	for _, status := range []string{"running", "awaiting_approval", "completed", "failed", "cancelled"} {
		for _, required := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/required=%t", status, required), func(t *testing.T) {
				fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
					"r1": {ID: "r1", Kind: "issue", Status: status, CodeCrossCheckRequired: required},
				}}
				out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
				if code != uzicli.ExitOK {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				if !required {
					if strings.Contains(out, "CODE_CHECK") {
						t.Fatalf("opt-out evidence invented: %s", out)
					}
					return
				}
				want := "Pending"
				if apitypes.IsTerminalRunStatus(status) {
					want = "outcome unavailable"
				}
				if !strings.Contains(out, want) || strings.Contains(out, "REASON") ||
					(apitypes.IsTerminalRunStatus(status) && strings.Contains(out, "Pending")) {
					t.Fatalf("missing-row outcome invented: %s", out)
				}
			})
		}
	}
}

func TestRunGetCodeCrossCheckMetadata(t *testing.T) {
	family, model, effort, child, head := "codex", "recorded-model", "high", "checker-child", strings.Repeat("a", 40)
	for _, usage := range []*apitypes.UsageDTO{nil, {CostStatus: "metered", CostUSD: 1.25}, {CostStatus: "subscription"}, {CostStatus: "unreported"}} {
		for _, deleted := range []bool{false, true} {
			s := &apitypes.CodeCrossCheck{Outcome: "completed", CheckerHarness: &family, CheckerModel: &model, CheckerEffort: &effort, HeadCommit: &head, Usage: usage, Findings: json.RawMessage("[]")}
			if !deleted {
				s.CheckerRunID = &child
			}
			out := codeCheckCommand(t, s)
			wantCost := "cost unavailable"
			if usage != nil && usage.CostStatus == "metered" {
				wantCost = "$1.25"
			}
			if usage != nil && usage.CostStatus == "subscription" {
				wantCost = "no estimate"
			}
			for _, want := range []string{family, model, effort, head, wantCost} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %s", want, out)
				}
			}
			if strings.Contains(out, child) == deleted || strings.Contains(out, "$0.00") {
				t.Fatalf("invented metadata: %s", out)
			}
		}
	}
}

func TestRunGetCodeCrossCheckHostileBounds(t *testing.T) {
	hostile := "visible\nFORGED_ROW\t\x1b[31m\x07\u202e"
	items := make([]map[string]any, 21)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprintf("f%d", i), "severity": hostile, "path": hostile, "line": 12, "title": hostile, "detail": strings.Repeat("x", 9000)}
	}
	items[0]["detail"] = hostile
	items[1]["id"] = "f0"
	items[2]["id"] = "bad\nkey"
	items[3]["id"] = "bad\x1bkey"
	items[4]["id"] = "bad\u202ekey"
	items[20]["title"] = "excluded-finding"
	// Keep the serialized payload under the parse bound while testing field caps.
	for i := 1; i < 20; i++ {
		items[i]["detail"] = strings.Repeat("x", 500)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	s := &apitypes.CodeCrossCheck{Outcome: "completed", Findings: raw, CheckerModel: &hostile, CheckerHarness: &hostile, CheckerEffort: &hostile, CheckerRunID: &hostile, ReasonClass: &hostile}
	out := codeCheckCommand(t, s)
	if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, "\nFORGED_ROW") || strings.Contains(out, "excluded-finding") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unsafe or unbounded output: %q", out)
	}
	if strings.Count(out, "ID unavailable") != 4 || !strings.Contains(out, "1 findings omitted") {
		t.Fatalf("ID/cap handling: %s", out)
	}
	for _, invalid := range []json.RawMessage{json.RawMessage("null"), json.RawMessage("{}"), json.RawMessage("broken"), json.RawMessage("[" + strings.Repeat(" ", 65536) + "]")} {
		s.Findings = invalid
		if got := codeCheckCommand(t, s); !strings.Contains(got, "Findings unavailable") {
			t.Fatalf("legacy/raw cap: %s", got)
		}
	}
}

func TestRunGetCodeCrossCheckDispositions(t *testing.T) {
	now := time.Now()
	findings := json.RawMessage(`[{"id":"KEEP_id-1","title":"first"},{"id":"f2","title":"second"},{"id":"f3","title":"third"}]`)
	batch := json.RawMessage(`[{"finding_id":"f2","disposition":"declined","reason":"verified second"},{"finding_id":"KEEP_id-1","disposition":"addressed","reason":"fixed first"},{"finding_id":"f3","disposition":"not_reported","reason":""}]`)
	s := &apitypes.CodeCrossCheck{Outcome: "completed", Findings: findings, Dispositions: batch, FinalizedAt: &now}
	out := codeCheckCommand(t, s)
	for _, want := range []string{"1 addressed, 1 declined, 1 not reported", "CODE_CHECK_1_DISPOSITION", "fixed first", "verified second", "Finalized", "KEEP_id-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	rowValues := make(map[string]string)
	for _, row := range codeCrossCheckRows(apitypes.RunDTO{CodeCrossCheckSummary: s}) {
		rowValues[row[0]] = row[1]
	}
	for key, want := range map[string]string{
		"CODE_CHECK_1_DISPOSITION":        "addressed",
		"CODE_CHECK_1_DISPOSITION_REASON": "fixed first",
		"CODE_CHECK_2_DISPOSITION":        "declined",
		"CODE_CHECK_2_DISPOSITION_REASON": "verified second",
		"CODE_CHECK_3_DISPOSITION":        "not_reported",
		"CODE_CHECK_3_DISPOSITION_REASON": "",
	} {
		if got, ok := rowValues[key]; !ok || got != want {
			t.Errorf("%s: present=%t, got %q, want %q", key, ok, got, want)
		}
	}
	s.InterruptedAt = &now
	out = codeCheckCommand(t, s)
	if strings.Contains(out, "_COUNTS") || !strings.Contains(out, "EARLIER_CODE_CHECK_1_DISPOSITION") || !strings.Contains(out, "fixed first") {
		t.Fatalf("history: %s", out)
	}
	s.InterruptedAt = nil
	s.FinalizedAt = nil
	out = codeCheckCommand(t, s)
	if !strings.Contains(out, "Pending report") || strings.Contains(out, "_COUNTS") {
		t.Fatalf("pending: %s", out)
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		s.Dispositions = raw
		out = codeCheckCommand(t, s)
		if !strings.Contains(out, "Dispositions unavailable") || strings.Contains(out, "_COUNTS") || strings.Contains(out, "not reported") {
			t.Fatalf("legacy: %s", out)
		}
	}
	s.FinalizedAt = &now
	s.Dispositions = batch
	rawJSON := codeCheckCommand(t, s, "--json")
	var decoded apitypes.RunDTO
	if json.Unmarshal([]byte(rawJSON), &decoded) != nil || decoded.CodeCrossCheckSummary == nil {
		t.Fatalf("JSON: %s", rawJSON)
	}
	expected, _ := json.Marshal(s)
	actual, _ := json.Marshal(decoded.CodeCrossCheckSummary)
	if string(expected) != string(actual) {
		t.Fatalf("JSON evidence changed: %s", rawJSON)
	}
}

func TestRunGetCodeCrossCheckDispositionValidation(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                   string
		findings, dispositions string
	}{
		{"unknown", `[{"id":"f1"}]`, `[{"finding_id":"f2","disposition":"addressed","reason":"hidden reason"}]`},
		{"duplicate disposition", `[{"id":"f1"}]`, `[{"finding_id":"f1","disposition":"addressed","reason":"hidden reason"},{"finding_id":"f1","disposition":"declined","reason":""}]`},
		{"duplicate finding", `[{"id":"f1"},{"id":"f1"}]`, `[{"finding_id":"f1","disposition":"addressed","reason":"hidden reason"}]`},
		{"forged ID", `[{"id":"f1\n"}]`, `[{"finding_id":"f1","disposition":"addressed","reason":"hidden reason"}]`},
		{"bad status", `[{"id":"f1"}]`, `[{"finding_id":"f1","disposition":"accepted","reason":"hidden reason"}]`},
		{"missing reason", `[{"id":"f1"}]`, `[{"finding_id":"f1","disposition":"addressed","reason":null}]`},
		{"incomplete finalized", `[{"id":"f1"}]`, `[]`},
		{"null", `[{"id":"f1"}]`, `null`},
		{"object", `[{"id":"f1"}]`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage(tc.findings), Dispositions: json.RawMessage(tc.dispositions), FinalizedAt: &now})
			if !strings.Contains(out, "Dispositions unavailable") || strings.Contains(out, "_COUNTS") || strings.Contains(out, "hidden reason") {
				t.Fatalf("invalid evidence: %s", out)
			}
		})
	}
	for _, reason := range []string{strings.Repeat("界", 342), strings.Repeat("x", 1025), strings.Repeat("界", 341) + "a", "safe\nFORGED_ROW\t\x1b[31m\x07\u202e"} {
		raw, _ := json.Marshal([]map[string]string{{"finding_id": "f1", "disposition": "addressed", "reason": reason}})
		out := codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage(`[{"id":"f1"}]`), Dispositions: raw, FinalizedAt: &now})
		if len(reason) > 1024 {
			if !strings.Contains(out, "Dispositions unavailable") || strings.Contains(out, "_COUNTS") {
				t.Fatalf("byte cap: %s", out)
			}
		} else if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, "\nFORGED_ROW") || !strings.Contains(out, "1 addressed, 0 declined, 0 not reported") {
			t.Fatalf("unsafe: %q", out)
		}
	}
	rawInvalid := []byte(`[{"finding_id":"f1","disposition":"addressed","reason":"`)
	rawInvalid = append(rawInvalid, byte(0xff))
	rawInvalid = append(rawInvalid, []byte(`"}]`)...)
	out := codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage(`[{"id":"f1"}]`), Dispositions: rawInvalid, FinalizedAt: &now})
	if !strings.Contains(out, "Dispositions unavailable") || strings.Contains(out, "_COUNTS") {
		t.Fatalf("invalid UTF8: %s", out)
	}
	for _, count := range []int{20, 21} {
		findings := make([]map[string]string, count)
		batch := make([]map[string]string, count)
		for i := range count {
			id := fmt.Sprintf("f%d", i)
			findings[i] = map[string]string{"id": id}
			batch[i] = map[string]string{"finding_id": id, "disposition": "addressed", "reason": strings.Repeat("x", 1024)}
		}
		f, _ := json.Marshal(findings)
		d, _ := json.Marshal(batch)
		out := codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: f, Dispositions: d, FinalizedAt: &now})
		if count == 20 && !strings.Contains(out, "20 addressed") || count == 21 && (!strings.Contains(out, "Dispositions unavailable") || strings.Contains(out, "_COUNTS")) {
			t.Fatalf("item cap %d: %s", count, out)
		}
	}
}

func TestRunGetCodeCrossCheckJSONUnchanged(t *testing.T) {
	s := &apitypes.CodeCrossCheck{Outcome: "pending", Findings: json.RawMessage("[]")}
	out := codeCheckCommand(t, s, "--json")
	var got apitypes.RunDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.CodeCrossCheckSummary == nil || got.CodeCrossCheckSummary.Outcome != "pending" || string(got.CodeCrossCheckSummary.Findings) != "[]" {
		t.Fatalf("JSON changed: %s", out)
	}
}

func TestRunGetCodeCrossCheckGuidanceRow(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef"
	out := codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage("[]"),
		CustomGuidanceApplied: true, GuidanceDigest: digest})
	if !strings.Contains(out, "custom guidance applied (sha256 0123456789ab)") || strings.Contains(out, "0123456789abc") {
		t.Fatalf("guidance row wrong: %s", out)
	}
	out = codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage("[]"), GuidanceDigest: digest})
	if strings.Contains(out, "custom guidance") || strings.Contains(out, "0123456789ab") {
		t.Fatalf("guidance row shown when not applied: %s", out)
	}
	out = codeCheckCommand(t, &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage("[]"),
		CustomGuidanceApplied: true, GuidanceDigest: "\x1b[31mab\x07\nZZ"})
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("hostile digest not sanitised: %q", out)
	}
}
