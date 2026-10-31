package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func gatedRunGet(t *testing.T, r apitypes.RunDTO) string {
	t.Helper()
	r.ID, r.Kind = "r1", "issue"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit %d: %s", code, stderr)
	}
	return out
}

// The shared "passed" rule: gated, at the gate, no reason, latest row approve, not historical.
func TestRunGetGatedPassedRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     *string
		verdict    string
		historical bool
		want       string // substring in output; "" means no cross-check line at all
		notWant    string
	}{
		{"pass", nil, "approve", false, "plan cross-check: passed after 2 rounds", ""},
		{"pass then human revise", nil, "approve", true, "", "passed after"},
		{"block then human revise", nil, "block", true, "", "passed after"},
		{"worker_unsupported then human revise", nil, "approve", true, "", "passed after"},
		{"block at gate", strp("blocked"), "block", false, "plan cross-check: blocked", "passed after"},
		{"worker_unsupported at gate", strp("worker_unsupported"), "", false, "plan cross-check: worker unsupported", "passed after"},
		{"revise latest", nil, "revise", false, "", "passed after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckGated: true, PlanCrossCheckGateReason: tc.reason}
			if tc.verdict != "" {
				r.PlanCrossCheckSummary = &apitypes.PlanCrossCheckSummaryDTO{Round: 2, Verdict: tc.verdict, Historical: tc.historical}
			}
			out := gatedRunGet(t, r)
			if !strings.Contains(out, "PLAN_CHECK_MODE") {
				t.Errorf("missing mode row:\n%s", out)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("missing %q:\n%s", tc.want, out)
			}
			if tc.notWant != "" && strings.Contains(out, tc.notWant) {
				t.Errorf("unexpected %q:\n%s", tc.notWant, out)
			}
		})
	}
}

func TestRunGetGatedAbsentFieldsRenderNothing(t *testing.T) {
	out := gatedRunGet(t, apitypes.RunDTO{Status: "awaiting_approval",
		PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{Round: 1, Verdict: "revise"}})
	for _, bad := range []string{"PLAN_CHECK_MODE ", "_ROUNDS", "_DEADLINE", "_ROUND_", "RESPONSE"} {
		if strings.Contains(out, bad) {
			t.Errorf("older server rendered %q:\n%s", bad, out)
		}
	}
}

func TestRunGetGatedThreeRoundsEachWithOwnResponse(t *testing.T) {
	deadline := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var rounds []apitypes.PlanCrossCheckRoundDTO
	for n := int32(1); n <= 3; n++ {
		verdict := "revise"
		if n == 3 {
			verdict = "approve"
		}
		rd := apitypes.PlanCrossCheckRoundDTO{Round: n, Verdict: verdict,
			Findings: &apitypes.PlanCrossCheckFindingsDTO{Summary: fmt.Sprintf("sum%d", n),
				Items: []apitypes.PlanCrossCheckFindingDTO{{File: fmt.Sprintf("file%d.go", n), Severity: "major",
					Summary: fmt.Sprintf("issue%d", n), Rationale: fmt.Sprintf("why%d", n)}}},
			Response: apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseReported, Text: fmt.Sprintf("answer-for-round-%d", n)}}
		rounds = append(rounds, rd)
	}
	rounds[2].Response = apitypes.PlanCrossCheckRoundResponseDTO{State: apitypes.PlanCrossCheckResponseNoFollowingCandidate}
	out := gatedRunGet(t, apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckGated: true,
		PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{Round: 3, Verdict: "approve",
			DeadlineAt: &deadline, Rounds: rounds}})
	for _, want := range []string{
		"PLAN_CHECK_ROUNDS", "3", "2026-10-31T12:00:00Z",
		"PLAN_CHECK_ROUND_1_RESULT", "PLAN_CHECK_ROUND_2_RESULT", "PLAN_CHECK_ROUND_3_RESULT",
		"PLAN_CHECK_ROUND_1_1_FILE", "PLAN_CHECK_ROUND_2_1_SEVERITY", "PLAN_CHECK_ROUND_3_1_SUMMARY", "PLAN_CHECK_ROUND_1_1_RATIONALE",
		"PLAN_CHECK_ROUND_1_RESPONSE_STATE", "PLAN_CHECK_ROUND_3_RESPONSE_STATE", "no_following_candidate",
		"lead's response (unverified claims)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for n := 1; n <= 2; n++ {
		line := rowLine(out, fmt.Sprintf("PLAN_CHECK_ROUND_%d_RESPONSE ", n))
		if !strings.Contains(line, fmt.Sprintf("answer-for-round-%d", n)) {
			t.Errorf("round %d response row = %q", n, line)
		}
		for m := 1; m <= 3; m++ {
			if m != n && strings.Contains(line, fmt.Sprintf("answer-for-round-%d", m)) {
				t.Errorf("round %d row carries round %d response: %q", n, m, line)
			}
		}
	}
	if strings.Contains(rowLine(out, "PLAN_CHECK_ROUND_3_RESPONSE "), "answer-for") {
		t.Errorf("round 3 (no following candidate) must show no response text:\n%s", out)
	}
}

func rowLine(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), strings.TrimSpace(prefix)+" ") || strings.HasPrefix(strings.TrimSpace(l), strings.TrimSpace(prefix)+"\t") {
			return l
		}
	}
	return ""
}

func TestRunGetGatedEveryResponseState(t *testing.T) {
	for _, state := range []string{
		apitypes.PlanCrossCheckResponseReported, apitypes.PlanCrossCheckResponseTruncated,
		apitypes.PlanCrossCheckResponseNotReported, apitypes.PlanCrossCheckResponseUnknown,
		apitypes.PlanCrossCheckResponseReplannedAfterInterrupted, apitypes.PlanCrossCheckResponseReportedInPresentedPlan,
		apitypes.PlanCrossCheckResponseNoFollowingCandidate,
	} {
		t.Run(state, func(t *testing.T) {
			out := gatedRunGet(t, apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckGated: true,
				PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{Round: 1, Verdict: "revise",
					Rounds: []apitypes.PlanCrossCheckRoundDTO{{Round: 1, Verdict: "revise",
						Response: apitypes.PlanCrossCheckRoundResponseDTO{State: state, Text: "body-" + state}}}}})
			if !strings.Contains(rowLine(out, "PLAN_CHECK_ROUND_1_RESPONSE_STATE "), state) {
				t.Errorf("state row missing %q:\n%s", state, out)
			}
			if !strings.Contains(out, "body-"+state) {
				t.Errorf("response text missing:\n%s", out)
			}
		})
	}
}

func TestRunGetGatedResponseTextIsTerminalSafe(t *testing.T) {
	hostile := "ok\n\t\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\x1b[2J\u202e" + strings.Repeat("A", 20000)
	out := gatedRunGet(t, apitypes.RunDTO{Status: "awaiting_approval", PlanCrossCheckGated: true,
		PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{Round: 1, Verdict: "revise",
			Rounds: []apitypes.PlanCrossCheckRoundDTO{{Round: 1, Verdict: hostile,
				Findings: &apitypes.PlanCrossCheckFindingsDTO{Summary: hostile},
				Response: apitypes.PlanCrossCheckRoundResponseDTO{State: hostile, Text: hostile}}}}})
	if strings.ContainsAny(out, "\x1b\x07\u202e") {
		t.Fatalf("unsafe terminal output: %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("response text vanished: %q", out)
	}
	if len(out) > 5000 {
		t.Fatalf("unbounded output: %d bytes", len(out))
	}
}

func TestRunGetGatedJSONPassesDTOThrough(t *testing.T) {
	r := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "awaiting_approval", PlanCrossCheckGated: true,
		PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{Round: 1, Verdict: "revise",
			Rounds: []apitypes.PlanCrossCheckRoundDTO{{Round: 1, Verdict: "revise",
				Response: apitypes.PlanCrossCheckRoundResponseDTO{State: "reported", Text: "raw-lead-text"}}}}}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": r}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{`"plan_cross_check_gated": true`, `"rounds"`, `"raw-lead-text"`, `"reported"`} {
		if !strings.Contains(out, want) && !strings.Contains(strings.ReplaceAll(out, `": `, `":`), strings.ReplaceAll(want, `": `, `":`)) {
			t.Errorf("--json missing %s:\n%s", want, out)
		}
	}
}
