package main

// run_lead_config_history_test.go covers `uzi run lead-config <run-id> [--json]` (PRD #3244 M3)
// against uzicli.FakeClient: the table (acceptance example 3: gen 2 listed before gen 1), the
// visible truncation note, the untrusted-text sanitizing, the empty and 404 cases and --json.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func leadHistoryEntry(gen int64, phase string, seq int, model, source string) apitypes.LeadConfigHistoryEntryDTO {
	effort := "medium"
	return apitypes.LeadConfigHistoryEntryDTO{
		ClaimGeneration: gen,
		LeadPhaseConfigDTO: apitypes.LeadPhaseConfigDTO{
			Phase: phase, PhaseEntry: 1, Seq: seq, Harness: "claude",
			RequestedModel: &model, RequestedModelSource: source, RequestedModelFixedAt: "claim",
			RequestedEffort: &effort, RequestedEffortMode: "explicit", RequestedEffortSource: "builtin", RequestedEffortFixedAt: "claim",
			ReportedAt: time.Date(2026, 10, 30, 8, 15, 0, 0, time.UTC),
		},
	}
}

func TestRunLeadConfigTable(t *testing.T) {
	obsModel, obsEv := "claude-opus-4", "claude_init"
	obsEffort := "high"
	gen2 := leadHistoryEntry(2, "plan", 1, "sonnet", "user_default")
	gen2.ObservedModel, gen2.ObservedModelEvidence = &obsModel, &obsEv
	gen2.ObservedEffort, gen2.ObservedEffortEvidence = &obsEffort, &obsEv
	gen1 := leadHistoryEntry(1, "plan", 1, "opus", "template")
	gen1.Truncated = true
	fc := &uzicli.FakeClient{LeadConfigHistoryByID: map[string]apitypes.LeadConfigHistoryDTO{
		"run1": {Entries: []apitypes.LeadConfigHistoryEntryDTO{gen2, gen1}},
	}}

	out, _, code := runCLI(t, fakeEnv(fc), "run", "lead-config", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "GEN PHASE ENTRY SEQ REQUESTED SOURCE OBSERVED EVIDENCE REPORTED" {
		t.Errorf("header = %q", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("want header plus 2 rows, got %d lines:\n%s", len(lines), out)
	}
	for _, want := range []string{"sonnet · medium", "model: your Claude default; effort: built-in", "claude-opus-4 · high",
		"Claude SDK init", "2026-10-30T08:15:00Z"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("gen 2 row missing %q:\n%s", want, lines[1])
		}
	}
	for _, want := range []string{"opus · medium", "model: lead template", "- · -", "model: not reported yet; effort: not reported yet",
		"(later changes not recorded)"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("gen 1 row missing %q:\n%s", want, lines[2])
		}
	}
	if strings.Index(out, "sonnet") > strings.Index(out, "opus") {
		t.Errorf("gen 2 must be listed before gen 1:\n%s", out)
	}
	if strings.Contains(out, "first 256 entries") {
		t.Errorf("an untruncated list must not print the truncation note:\n%s", out)
	}
}

func TestRunLeadConfigTruncationNoteAndJSON(t *testing.T) {
	h := apitypes.LeadConfigHistoryDTO{Entries: []apitypes.LeadConfigHistoryEntryDTO{leadHistoryEntry(1, "plan", 1, "opus", "user_default")}, Truncated: true}
	fc := &uzicli.FakeClient{LeadConfigHistoryByID: map[string]apitypes.LeadConfigHistoryDTO{"run1": h}}

	out, _, code := runCLI(t, fakeEnv(fc), "run", "lead-config", "run1")
	if code != uzicli.ExitOK || !strings.Contains(out, "showing the first 256 entries (newest attempt first); the rest are not listed") {
		t.Fatalf("table = %d, want the truncation note:\n%s", code, out)
	}

	out, _, code = runCLI(t, fakeEnv(fc), "--json", "run", "lead-config", "run1")
	var got apitypes.LeadConfigHistoryDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("--json = %d %q", code, out)
	}
	if !got.Truncated || len(got.Entries) != 1 || got.Entries[0].ClaimGeneration != 1 || got.Entries[0].Phase != "plan" {
		t.Errorf("--json body = %+v", got)
	}
}

func TestRunLeadConfigEmptyAndNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{LeadConfigHistoryByID: map[string]apitypes.LeadConfigHistoryDTO{"run1": {}}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "lead-config", "run1")
	if code != uzicli.ExitOK || !strings.Contains(out, "run run1 has no recorded lead config") {
		t.Fatalf("empty = %d %q", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "--json", "run", "lead-config", "run1")
	var got map[string]json.RawMessage
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || string(got["entries"]) != "[]" {
		t.Fatalf("empty --json = %d %q, want entries []", code, out)
	}
	if _, _, code = runCLI(t, fakeEnv(fc), "run", "lead-config", "other"); code != uzicli.ExitNotFound {
		t.Errorf("unknown run exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// Worker-attested model text cannot smuggle a control sequence or a forged line into the table.
func TestRunLeadConfigSanitizesUntrustedText(t *testing.T) {
	e := leadHistoryEntry(1, "plan", 1, "opus\x1b[31m\nforged line", "user_default")
	om, oev := "evil\x1b]0;title\x07\tmodel", "claude_init"
	e.ObservedModel, e.ObservedModelEvidence = &om, &oev
	fc := &uzicli.FakeClient{LeadConfigHistoryByID: map[string]apitypes.LeadConfigHistoryDTO{
		"run1": {Entries: []apitypes.LeadConfigHistoryEntryDTO{e}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "lead-config", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(out, "\x1b\x07\t") {
		t.Errorf("a control byte reached the terminal: %q", out)
	}
	if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 2 {
		t.Errorf("a newline in a model string forged a line (%d lines):\n%s", n, out)
	}
}
