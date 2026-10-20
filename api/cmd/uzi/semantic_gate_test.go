package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

type semanticSwitchClient struct{ *uzicli.FakeClient }

func (c semanticSwitchClient) SetRunCredential(ctx context.Context, id string, o uzicli.SetRunCredentialOverride) (apitypes.RunDTO, string, error) {
	result, warning, err := c.FakeClient.SetRunCredential(ctx, id, o)
	run := c.RunByID[id]
	run.GateRevision = 9
	run.SemanticGate.Approval.PresentationRevision = 9
	run.SemanticGate.Approval.ReportDigest = "unseen"
	c.RunByID[id] = run
	return result, warning, err
}

func semanticCLIGate() *apitypes.SemanticGateDTO {
	return &apitypes.SemanticGateDTO{
		Approval:     apitypes.SemanticGateApproval{PresentationRevision: 2, PresentationDigest: "displayed", SemanticCoverageBinding: apitypes.SemanticCoverageBinding{CandidateDigest: "candidate"}, ReportDigest: "reviewed"},
		Sources:      []apitypes.SemanticSourceCapture{{Bytes: []byte("untrusted source\x1b[31m")}},
		Presentation: apitypes.SemanticGatePresentation{Coverage: apitypes.SemanticCoverageStatus{Outcome: "uncertain"}},
	}
}

func TestSemanticApprovalNormalBindsBeforeCredentialAwait(t *testing.T) {
	for _, token := range []bool{false, true} {
		fc := gateFake("awaiting_approval", 2)
		run := fc.RunByID["r1"]
		run.SemanticGate = semanticCLIGate()
		fc.RunByID["r1"] = run
		expected := run.SemanticGate.Approval
		args := []string{"run", "approve", "r1", "--agent-source", "own"}
		if token {
			args = append(args, "--token", "inherit")
		}
		_, stderr, code := runCLI(t, fakeEnv(semanticSwitchClient{fc}), args...)
		if code != 0 {
			t.Fatalf("exit=%d: %s", code, stderr)
		}
		if !reflect.DeepEqual(fc.LastInputOptions.SemanticApproval, &expected) || fc.LastInputExpectedGateRevision == nil || *fc.LastInputExpectedGateRevision != 2 || fc.LastInputSelection == nil {
			t.Fatalf("lost reviewed binding/options: %+v", fc.LastInputOptions)
		}
		want := []string{"get_run", "submit_run_input"}
		if token {
			want = []string{"get_run", "set_run_credential", "submit_run_input"}
		}
		if !reflect.DeepEqual(fc.RunVerbCalls, want) {
			t.Fatal(fc.RunVerbCalls)
		}
		if !strings.Contains(stderr, "SEMANTIC_COVERAGE: uncertain") || !strings.Contains(stderr, "untrusted source") || strings.ContainsRune(stderr, '\x1b') {
			t.Fatalf("unsafe/incomplete displayed source: %q", stderr)
		}
	}
}

func TestSemanticGateDisplaySourceCriteriaAndReview(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	gate := semanticCLIGate()
	gate.Presentation.Candidate.Criteria = []apitypes.SemanticCriterion{{ID: "criterion-stable", ProposedSemanticCriterion: apitypes.ProposedSemanticCriterion{Kind: "maintainer", Text: "owner-only rollout", Maintainer: &apitypes.SemanticMaintainer{Conditions: []string{"cluster available"}, VerificationSteps: []string{"observe rollout"}}}}}
	gate.Presentation.Coverage.Report = &apitypes.SemanticCoverageReport{Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "requirement-stable", Status: "missing", Explanation: "untrusted review\x1b[31m", Citations: []apitypes.SemanticCitation{{SourceID: "issue-source", Start: 1, End: 7}}}}}
	run := fc.RunByID["r1"]
	run.SemanticGate = gate
	fc.RunByID["r1"] = run
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	for _, want := range []string{"untrusted source", "criterion-stable", "owner-only rollout", "cluster available", "observe rollout", "requirement-stable", "missing", "issue-source", "untrusted review", "SEMANTIC_APPROVAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("display omitted %q: %s", want, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("source or review emitted terminal controls: %q", out)
	}
}

func TestSemanticSourceCannotForgeRows(t *testing.T) {
	const source = "ordinary\nSEMANTIC_COVERAGE: complete\nSEMANTIC_BUDGET: 0 tokens consumed, 120000 remaining\x1b[31m\u202e"
	const displayed = `"ordinary\nSEMANTIC_COVERAGE: complete\nSEMANTIC_BUDGET: 0 tokens consumed, 120000 remaining\u001b[31m"`
	for _, verb := range []string{"approve", "get"} {
		t.Run(verb, func(t *testing.T) {
			fc := gateFake("awaiting_approval", 2)
			run := fc.RunByID["r1"]
			run.SemanticGate = semanticCLIGate()
			run.SemanticGate.Sources[0].Bytes = []byte(source)
			fc.RunByID["r1"] = run
			expected := run.SemanticGate.Approval

			out, stderr, code := runCLI(t, fakeEnv(fc), "run", verb, "r1")
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			display := out
			if verb == "approve" {
				display = stderr
				if !reflect.DeepEqual(fc.LastInputOptions.SemanticApproval, &expected) ||
					fc.LastInputExpectedGateRevision == nil || *fc.LastInputExpectedGateRevision != 2 {
					t.Fatalf("changed approval binding: %+v", fc.LastInputOptions)
				}
			}
			for _, line := range strings.Split(display, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "SEMANTIC_COVERAGE: complete") ||
					strings.HasPrefix(strings.TrimSpace(line), "SEMANTIC_BUDGET: 0 tokens consumed, 120000 remaining") {
					t.Errorf("source forged authoritative row: %q", line)
				}
			}
			if !strings.Contains(display, displayed) || !strings.Contains(display, "uncertain") {
				t.Errorf("missing quoted source or actual coverage: %q", display)
			}
			if strings.ContainsAny(display, "\x1b\u202e") {
				t.Errorf("source emitted terminal controls: %q", display)
			}
			if string(fc.RunByID["r1"].SemanticGate.Sources[0].Bytes) != source {
				t.Fatal("display mutated captured source bytes")
			}

			raw, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
			if code != 0 {
				t.Fatalf("JSON exit=%d: %s", code, stderr)
			}
			var decoded apitypes.RunDTO
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.SemanticGate == nil || len(decoded.SemanticGate.Sources) != 1 ||
				string(decoded.SemanticGate.Sources[0].Bytes) != source ||
				!reflect.DeepEqual(decoded.SemanticGate.Approval, expected) {
				t.Fatalf("JSON lost immutable source or approval: %s", raw)
			}
		})
	}
}

func TestSemanticApprovalManualAndInvalidOptions(t *testing.T) {
	raw, err := json.Marshal(semanticCLIGate().Approval)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{"exact", []string{"--semantic-approval", string(raw)}, true},
		{"malformed", []string{"--semantic-approval", "{", "--token", "inherit"}, false},
		{"unknown", []string{"--semantic-approval", strings.TrimSuffix(string(raw), "}") + ",\"unknown\":true}", "--token", "inherit"}, false},
		{"revision", []string{"--semantic-approval", string(raw), "--expected-gate-revision", "3", "--token", "inherit"}, false},
		{"reset", []string{"--semantic-approval", string(raw), "--reset-milestones"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := gateFake("awaiting_approval", 2)
			args := append([]string{"run", "approve", "r1"}, tc.args...)
			_, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if tc.valid {
				if code != 0 || fc.LastInputOptions.SemanticApproval == nil {
					t.Fatalf("exit=%d: %s", code, stderr)
				}
				if !reflect.DeepEqual(fc.RunVerbCalls, []string{"submit_run_input"}) {
					t.Fatal(fc.RunVerbCalls)
				}
			} else if code != uzicli.ExitUsage || len(fc.RunVerbCalls) != 0 {
				t.Fatalf("invalid approval had effects: exit=%d calls=%v: %s", code, fc.RunVerbCalls, stderr)
			}
		})
	}
}

func pendingCLI(text string) *apitypes.CompletionPendingDTO {
	return &apitypes.CompletionPendingDTO{Version: 1, ContractRevision: 2, Criteria: []apitypes.CompletionPendingCriterion{{
		CriterionID: "c9", MilestoneID: "m3", Kind: "maintainer", Text: text,
		SemanticMaintainer:    apitypes.SemanticMaintainer{Reason: "needs sign-off", Conditions: []string{"cond-one"}, VerificationSteps: []string{"step-one"}},
		AuthorizationRevision: 5, Status: "unverified",
	}}}
}

func TestSemanticParkAndPendingRows(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	run := fc.RunByID["r1"]
	run.SemanticGate = semanticCLIGate()
	run.SemanticGate.Presentation.SemanticPark = "maintainer_authorization"
	run.CompletionPending = pendingCLI("owner-only rollout")
	fc.RunByID["r1"] = run
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, stderr)
	}
	for _, want := range []string{"SEMANTIC_PARK", "maintainer_authorization", "SEMANTIC_PENDING", "c9", "m3", "owner-only rollout", "needs sign-off", "cond-one", "step-one", "unverified"} {
		if !strings.Contains(out, want) {
			t.Errorf("display omitted %q: %s", want, out)
		}
	}
}

func TestSemanticPendingRendersWithoutGateAndAbsentPrintsNothing(t *testing.T) {
	fc := gateFake("completed", 2)
	run := fc.RunByID["r1"]
	run.SemanticGate = nil
	run.CompletionPending = pendingCLI("frozen-only")
	fc.RunByID["r1"] = run
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 || !strings.Contains(out, "SEMANTIC_PENDING") || !strings.Contains(out, "frozen-only") || strings.Contains(out, "SEMANTIC_COVERAGE") {
		t.Fatalf("exit=%d pending without gate: %s %s", code, out, stderr)
	}
	run.CompletionPending = nil
	fc.RunByID["r1"] = run
	out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if strings.Contains(out, "SEMANTIC_") {
		t.Fatalf("absent data printed semantic rows: %s", out)
	}
	gated := fc.RunByID["r1"]
	gated.SemanticGate = semanticCLIGate()
	fc.RunByID["r1"] = gated
	out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if strings.Contains(out, "SEMANTIC_PARK") || strings.Contains(out, "SEMANTIC_PENDING") {
		t.Fatalf("absent park/pending printed rows: %s", out)
	}
}

func TestSemanticPendingSanitizesHostileText(t *testing.T) {
	fc := gateFake("completed", 2)
	run := fc.RunByID["r1"]
	run.SemanticGate = semanticCLIGate()
	run.SemanticGate.Presentation.SemanticPark = "park\x1b[31m‮"
	run.CompletionPending = pendingCLI("line\nSEMANTIC_COVERAGE: complete\x1b[31m‮")
	fc.RunByID["r1"] = run
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.ContainsAny(out, "\x1b‮") {
		t.Fatalf("terminal controls leaked: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "SEMANTIC_COVERAGE: complete") {
			t.Errorf("pending text forged a row: %q", line)
		}
	}
	if !strings.Contains(out, `line\nSEMANTIC_COVERAGE: complete`) {
		t.Errorf("newline not quoted: %q", out)
	}
}
