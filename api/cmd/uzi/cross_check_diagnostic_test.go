package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Keep these expectations literal: the CLI displays the producer's text rather
// than deriving it from code, stage or harness.
func diagnosticLogFixtures(t *testing.T) []apitypes.MessageDTO {
	t.Helper()
	pairs := []struct{ code, text string }{
		{"provider_error", "Cross-check incomplete: provider reported an error."},
		{"checker_error_result", "Cross-check incomplete: checker returned an unsuccessful result."},
		{"transport_ended", "Cross-check incomplete: checker event stream ended without a terminal result."},
		{"no_complete_verdict", "Cross-check incomplete: terminal result contained no complete verdict."},
		{"tool_budget_exceeded", "Cross-check incomplete: Codex checker exceeded its tool-call budget."},
		{"unknown_execution_error", "Cross-check incomplete: execution error; specific cause not recorded."},
	}
	var msgs []apitypes.MessageDTO
	for _, stage := range []string{"plan", "code"} {
		for _, harness := range []string{"claude", "codex"} {
			for _, pair := range pairs {
				raw, err := json.Marshal(map[string]string{
					"event": "cross_check_diagnostic", "stage": stage, "harness": harness,
					"reason_class": "model_error", "code": pair.code, "text": pair.text,
				})
				if err != nil {
					t.Fatal(err)
				}
				m := statusMsg(int32(len(msgs)+1), string(raw))
				instance, label := "checker-instance-abcdef", "checker"
				m.AgentInstance, m.AgentLabel = &instance, &label
				m.CreatedAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
				msgs = append(msgs, m)
			}
		}
	}
	return msgs
}

func diagnosticHumanRow(m apitypes.MessageDTO, text string) string {
	actor := "cross-checker"
	if m.AgentInstance != nil {
		actor += "/abcdef · checker"
	}
	return fmt.Sprintf("#%-4d %-16s %-34s %s\n", m.Seq, m.Kind, actor, text)
}

func TestRunLogsCrossCheckDiagnosticCommands(t *testing.T) {
	msgs := diagnosticLogFixtures(t)
	for _, path := range []struct {
		name string
		args []string
	}{
		{"normal", nil},
		{"tail", []string{"--tail", "24"}},
		{"follow", []string{"--follow"}},
		{"tail-follow", []string{"--tail", "1", "--follow"}},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", path.name, asJSON), func(t *testing.T) {
				fc := &uzicli.FakeClient{
					LogsByID: map[string][]apitypes.MessageDTO{"r1": msgs},
					RunByID:  map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "failed"}},
				}
				wantMsgs := msgs
				var afters []int32
				if path.name == "tail-follow" {
					// Tail supplies one row; the terminal drain supplies a later
					// diagnostic immediately, with no polling timer or sleep.
					fc.LogsByID = map[string][]apitypes.MessageDTO{"r1": msgs[:1]}
					wantMsgs = msgs[:2]
					fc.RunLogsHook = func(_ string, after int32) ([]apitypes.MessageDTO, error) {
						afters = append(afters, after)
						if after == 1 {
							return msgs[1:2], nil
						}
						return nil, nil
					}
				}
				args := append([]string{"run", "logs", "r1"}, path.args...)
				if asJSON {
					args = append(args, "--json")
				}
				out, stderr, code := runCLI(t, fakeEnv(fc), args...)
				if code != uzicli.ExitOK || stderr != "" {
					t.Fatalf("exit=%d stderr=%q", code, stderr)
				}
				if path.name == "tail-follow" && !reflect.DeepEqual(afters, []int32{1, 2}) {
					t.Fatalf("follow cursors=%v, want [1 2]", afters)
				}
				if asJSON {
					lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
					if len(lines) != len(wantMsgs) {
						t.Fatalf("NDJSON rows=%d, want %d", len(lines), len(wantMsgs))
					}
					for i, line := range lines {
						// Whole-object comparison also pins seq, kind, actor metadata,
						// timestamp and exactly the original six payload fields.
						var got, want map[string]any
						if err := json.Unmarshal([]byte(line), &got); err != nil {
							t.Fatalf("row %d is not one JSON object: %v", i, err)
						}
						raw, err := json.Marshal(wantMsgs[i])
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(raw, &want); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("row %d DTO changed: got %v, want %v", i, got, want)
						}
					}
					return
				}
				var want strings.Builder
				for _, m := range wantMsgs {
					var payload map[string]string
					if err := json.Unmarshal(m.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					want.WriteString(diagnosticHumanRow(m, payload["text"]))
				}
				// Exact rows prove full safe text, stable sequence/kind/actor and
				// absence of compact JSON or arbitrary provider exception content.
				if out != want.String() {
					t.Fatalf("human rows differ:\ngot: %s\nwant: %s", out, want.String())
				}
			})
		}
	}
}

func TestRunLogsCrossCheckDiagnosticLegacyFallback(t *testing.T) {
	cases := []struct{ kind, raw string }{
		{"status", `{"event":"cross_check_diagnostic","code":"provider_error"}`},
		{"status", `{"event":"cross_check_diagnostic","text":null}`},
		{"status", `{"event":"cross_check_diagnostic","text":7}`},
		{"status", `{"event":"cross_check_diagnostic","text":true}`},
		{"status", `{"event":"cross_check_diagnostic","text":{}}`},
		{"status", `{"event":"cross_check_diagnostic","text":[]}`},
		{"status", `{"event":"future_event","text":"provider_error"}`},
		{"status", `{"text":"legacy","reason_class":"model_error"}`},
		{"status", `{"event":null,"text":"legacy"}`},
		{"assistant", `{"event":"cross_check_diagnostic","text":"provider_error"}`},
		{"status", `"provider_error"`},
		{"status", `null`},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			m := statusMsg(9, tc.raw)
			m.Kind = tc.kind
			fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {m}}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
			if code != uzicli.ExitOK || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
			if want := diagnosticHumanRow(m, tc.raw); out != want {
				t.Fatalf("legacy row changed: got %q, want %q", out, want)
			}
		})
	}
}

func TestRunLogsCrossCheckDiagnosticIgnoresExceptionMetadata(t *testing.T) {
	text := "Cross-check incomplete: execution error; specific cause not recorded."
	private := "glpat-" + strings.Repeat("X", 20) + " https://example.invalid/private /private/path excerpt\x1b\n"
	raw, err := json.Marshal(map[string]any{
		"event": "cross_check_diagnostic", "stage": "code", "harness": "claude",
		"reason_class": "model_error", "code": "unknown_execution_error", "text": text,
		"error": private, "name": private, "stack": private,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := logsOutput(t, string(raw))
	if want := diagnosticHumanRow(statusMsg(1, string(raw)), text); out != want {
		t.Fatalf("exception metadata changed the display: got %q, want %q", out, want)
	}
}

func TestRunLogsCrossCheckDiagnosticTextDisplay(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"sanitized", "before\x1b[2Jafter\u202e\u200b\x00\x7f\nnext\tend", "before[2Jafter next end"},
		{"unicode-cap", strings.Repeat("界", 198) + "😀éTAIL", strings.Repeat("界", 198) + "😀…"},
		{"empty-string", "", ""},
		{"no-code-reclassification", "provider_error", "provider_error"},
		{"no-constructor-parsing", "Error: checker event stream ended", "Error: checker event stream ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"event": "cross_check_diagnostic", "text": tc.text,
				"code": "future_code", "stage": "future_stage", "harness": nil,
			})
			if err != nil {
				t.Fatal(err)
			}
			out := logsOutput(t, string(raw))
			if want := diagnosticHumanRow(statusMsg(1, string(raw)), tc.want); out != want {
				t.Fatalf("display got %q, want %q", out, want)
			}
		})
	}
}
