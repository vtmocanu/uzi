package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRecoveryBlockerVisibility(t *testing.T) {
	const reason = "decoded prerequisite history exceeds 1 GiB; source custody retained"
	for _, specific := range []bool{false, true} {
		for _, format := range []string{"human", "quiet", "json"} {
			t.Run(format+map[bool]string{false: "/owner", true: "/run"}[specific], func(t *testing.T) {
				fc := &uzicli.FakeClient{
					RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
						{ID: "h1", RunID: "r1", State: "open", Attention: "source_only"},
						{ID: "h2", RunID: "r1", State: "open", Attention: "needs_action", HasAvailableCapture: true},
						{ID: "settled", RunID: "other", State: "released", Attention: "settled"},
					}},
					RunByID: map[string]apitypes.RunDTO{"r1": {Status: "failed", FailureReason: new(reason)}},
				}
				args := []string{"run", "recovery"}
				if specific {
					args = append(args, "r1")
				}
				if format == "json" {
					args = append(args, "--json")
				}
				if format == "quiet" {
					args = append(args, "--quiet")
				}
				out, stderr, code := runCLI(t, fakeEnv(fc), args...)
				if code != uzicli.ExitOK || stderr != "" {
					t.Fatalf("exit=%d stderr=%s", code, stderr)
				}
				if len(fc.RunVerbCalls) != 1 {
					t.Fatalf("run reads=%v", fc.RunVerbCalls)
				}
				if format != "json" {
					if strings.Count(out, "run r1: "+reason) != 1 {
						t.Fatalf("missing/doubled blocker: %s", out)
					}
					if format == "human" && (!strings.Contains(out, "custody") || !strings.Contains(out, "uzi run export")) {
						t.Fatalf("custody/archive guidance lost: %s", out)
					}
					return
				}
				var rows []map[string]any
				if specific {
					if err := json.Unmarshal([]byte(out), &rows); err != nil {
						t.Fatal(err)
					}
				} else {
					var owner struct{ Holds []map[string]any }
					if err := json.Unmarshal([]byte(out), &owner); err != nil {
						t.Fatal(err)
					}
					rows = owner.Holds
				}
				if rows[0]["failure_reason"] != reason || rows[1]["failure_reason"] != reason ||
					rows[0]["attention"] != "source_only" || rows[1]["has_available_capture"] != true {
					t.Fatalf("JSON changed custody or lost reason: %s", out)
				}
				if !specific {
					if _, ok := rows[2]["failure_reason"]; ok {
						t.Fatalf("settled run enriched: %s", out)
					}
				}
			})
		}
	}
}

func TestRecoveryBlockerUnavailable(t *testing.T) {
	for _, readErr := range []error{
		uzicli.Exitf(uzicli.ExitNotFound, "deleted /private/path"),
		uzicli.Exitf(uzicli.ExitAuth, "provider secret"),
		errors.New("outage\nforged line /private/path"),
	} {
		for _, specific := range []bool{false, true} {
			for _, asJSON := range []bool{false, true} {
				fc := &uzicli.FakeClient{
					RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
						{ID: "h1", RunID: "r1", State: "open", Attention: "source_only"},
						{ID: "h2", RunID: "r2", State: "open", Attention: "source_only"},
					}},
					GetRunHook: func(string) (apitypes.RunDTO, error) { return apitypes.RunDTO{}, readErr },
				}
				args := []string{"run", "recovery"}
				if specific {
					args = append(args, "r1")
				}
				if asJSON {
					args = append(args, "--json")
				}
				out, stderr, code := runCLI(t, fakeEnv(fc), args...)
				if code != uzicli.ExitOK || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "failure reason unavailable") {
					t.Fatalf("exit=%d stderr=%q", code, stderr)
				}
				if strings.Contains(stderr, "provider") || strings.Contains(stderr, "/private") || strings.Contains(stderr, "forged") {
					t.Fatalf("raw error leaked: %q", stderr)
				}
				if !strings.Contains(out, "h1") || !strings.Contains(out, "source_only") || strings.Contains(out, "failure_reason") {
					t.Fatalf("holds lost or invented reason: %s", out)
				}
				if asJSON {
					if !json.Valid([]byte(out)) {
						t.Fatalf("invalid JSON: %s", out)
					}
				} else if !strings.Contains(out, "no recovery archive") || strings.Contains(out, "uzi run export") {
					t.Fatalf("source custody changed: %s", out)
				}
			}
		}
	}
}

func TestRecoveryBlockerUntrustedAndAbsent(t *testing.T) {
	reason := "cap\nINJECT\ttext\r" + string(rune(27)) + "[31m"
	runID := "r\nINJECT\t" + string(rune(27))
	for _, specific := range []bool{false, true} {
		fc := &uzicli.FakeClient{
			RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
				{ID: "h", RunID: runID, State: "open", Attention: "source_only"},
			}},
			RunByID: map[string]apitypes.RunDTO{runID: {Status: "failed", FailureReason: &reason}},
		}
		// Quiet suppresses existing hints so this checks the new diagnostic seam.
		args := []string{"run", "recovery", "--quiet"}
		if specific {
			args = append(args, runID)
		}
		out, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK || strings.Contains(out, "\nINJECT") || strings.ContainsRune(out, rune(27)) ||
			!strings.Contains(out, "run r INJECT: cap INJECT text[31m") {
			t.Fatalf("unsafe or missing reason: %q exit=%d", out, code)
		}
		out, _, code = runCLI(t, fakeEnv(fc), append(args, "--json")...)
		if code != uzicli.ExitOK || !json.Valid([]byte(out)) {
			t.Fatalf("JSON: %q exit=%d", out, code)
		}
		var raw any
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if specific {
			row = raw.([]any)[0].(map[string]any)
		} else {
			row = raw.(map[string]any)["holds"].([]any)[0].(map[string]any)
		}
		if row["failure_reason"] != reason {
			t.Fatalf("JSON should preserve untrusted DTO value: %q", out)
		}
	}
	for _, status := range []string{"failed", "running"} {
		fc := &uzicli.FakeClient{
			RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{{ID: "h", RunID: "r1", State: "open"}}},
			RunByID:             map[string]apitypes.RunDTO{"r1": {Status: status}},
		}
		if status == "running" {
			fc.RunByID["r1"] = apitypes.RunDTO{Status: status, FailureReason: &reason}
		}
		out, stderr, code := runCLI(t, fakeEnv(fc), "run", "recovery", "--json")
		if code != uzicli.ExitOK || stderr != "" || strings.Contains(out, "failure_reason") {
			t.Fatalf("absent/nonfailed: %s %s %d", out, stderr, code)
		}
	}
}

func TestRecoveryBlockerUnavailableSibling(t *testing.T) {
	const reason = "decoded prerequisite history exceeds 1 GiB"
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
			{ID: "deleted", RunID: "gone", State: "open", Attention: "source_only"},
			{ID: "blocked", RunID: "r1", State: "open", Attention: "source_only"},
		}},
		RunByID: map[string]apitypes.RunDTO{"r1": {Status: "failed", FailureReason: new(reason)}},
	}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "recovery")
	if code != uzicli.ExitOK || len(fc.RunVerbCalls) != 2 || !strings.Contains(out, "run r1: "+reason) ||
		!strings.Contains(out, "hold deleted: no recovery archive") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("sibling skipped or custody lost: %s stderr=%s exit=%d reads=%v", out, stderr, code, fc.RunVerbCalls)
	}
	// A default fake 404 changes only stderr, never the existing human custody view.
	fc.RunByID = nil
	missing, _, code := runCLI(t, fakeEnv(fc), "run", "recovery")
	fc.RunByID = map[string]apitypes.RunDTO{"gone": {}, "r1": {}}
	empty, stderr, emptyCode := runCLI(t, fakeEnv(fc), "run", "recovery")
	if code != uzicli.ExitOK || emptyCode != uzicli.ExitOK || missing != empty || stderr != "" {
		t.Fatalf("missing lookup changed human holds: missing=%s empty=%s stderr=%s", missing, empty, stderr)
	}
	// Run-specific lookup never reads other runs, even if they have open holds.
	fc.RunVerbCalls = nil
	_, _, code = runCLI(t, fakeEnv(fc), "run", "recovery", "r1")
	if code != uzicli.ExitOK || len(fc.RunVerbCalls) != 1 {
		t.Fatalf("run filter failed: reads=%v exit=%d", fc.RunVerbCalls, code)
	}
}

func TestRecoveryBlockerNoHoldsAndCancellation(t *testing.T) {
	for _, args := range [][]string{{"run", "recovery"}, {"run", "recovery", "r1", "--json"}} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK || len(fc.RunVerbCalls) != 0 {
			t.Fatalf("empty list reads run: %v exit=%d", fc.RunVerbCalls, code)
		}
	}
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{{ID: "h", RunID: "r1", State: "open"}}},
		GetRunHook:          func(string) (apitypes.RunDTO, error) { return apitypes.RunDTO{}, context.Canceled },
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "--json")
	if code == uzicli.ExitOK || out != "" {
		t.Fatalf("cancellation swallowed: out=%q exit=%d", out, code)
	}
}
