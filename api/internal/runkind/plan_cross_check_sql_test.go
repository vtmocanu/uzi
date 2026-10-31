package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPlanCrossCheckableKinds(t *testing.T) {
	want := map[string]bool{Issue: true, Prompt: true, SelfImprove: true, CIFix: true, MRRework: true}
	for _, kind := range All() {
		if got := PlanCrossCheckable(kind); got != want[kind] {
			t.Errorf("PlanCrossCheckable(%q) = %t, want %t", kind, got, want[kind])
		}
	}
}

// Every production INSERT into runs is enumerated here. Only the six eligible
// statements may set the required snapshot; the other nine rely on its false default.
// PRD #3708: only the three statements that take a caller-supplied auto_approve (issue,
// schedule prompt, ci_fix) may also set the gated twin; the rest hard-code auto_approve=true
// or write no plan cross-check column, so they can never enroll as gated.
func TestPlanCrossCheckInsertParity(t *testing.T) {
	eligible := map[string]bool{
		"CreateRun": true, "CreatePromptRun": true, "CreateSelfImproveRun": true,
		"CreateCIFixRun": true, "CreateAutoMRReworkRun": true,
		"CreateManualMRReworkRunAndAdvance": true,
	}
	gatedEligible := map[string]bool{"CreateRun": true, "CreatePromptRun": true, "CreateCIFixRun": true}
	others := map[string]bool{
		"CreateChatRun": true, "CreateChatContinueRun": true, "CreateJobRun": true,
		"CreateJudgeRun": true, "CreateTaskRun": true, "CreateThenFixRun": true,
		"CreateTaskReviewRun": true, "CreatePlanCrossCheckChild": true, "CreateCodeCrossCheckChild": true,
	}
	paths, err := filepath.Glob(filepath.Join("..", "store", "queries", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	insert := regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+runs\s*\(`)
	seen := map[string]bool{}
	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // fixed query directory
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range namedQueryBlocks(string(raw)) {
			// Strip SQL comments before finding statements, so documentation cannot
			// masquerade as another INSERT.
			var sql strings.Builder
			for _, line := range strings.Split(body, "\n") {
				if at := strings.Index(line, "--"); at >= 0 {
					line = line[:at]
				}
				sql.WriteString(line)
				sql.WriteByte('\n')
			}
			if !insert.MatchString(sql.String()) {
				continue
			}
			if seen[name] {
				t.Errorf("duplicate runs INSERT %s", name)
			}
			seen[name] = true
			if !eligible[name] && !others[name] {
				t.Errorf("unenumerated runs INSERT %s", name)
			}
			// The child SELECT checks its lead's flag without setting its own.
			at := insert.FindStringIndex(sql.String())
			columns := strings.SplitN(sql.String()[at[0]:], ")", 2)[0]
			got := strings.Contains(columns, "plan_cross_check_required")
			if got != eligible[name] {
				t.Errorf("%s: snapshot column = %t, eligible = %t", name, got, eligible[name])
			}
			if got && !strings.Contains(sql.String(), "plan_cross_check_enabled") {
				t.Errorf("%s: snapshot does not read owner setting", name)
			}
			gotGated := strings.Contains(columns, "plan_cross_check_gated")
			if gotGated != gatedEligible[name] {
				t.Errorf("%s: gated column = %t, gated eligible = %t", name, gotGated, gatedEligible[name])
			}
			if gotGated {
				// The gated twin is the exact complement of the required snapshot on
				// auto_approve, reading the same owner setting.
				if !strings.Contains(sql.String(), "(NOT @auto_approve") {
					t.Errorf("%s: gated snapshot does not require NOT @auto_approve", name)
				}
				if strings.Count(sql.String(), "plan_cross_check_enabled") != 2 {
					t.Errorf("%s: required and gated snapshots must each read the owner setting once", name)
				}
				if !got {
					t.Errorf("%s: gated snapshot without the required snapshot", name)
				}
			}
		}
	}
	for name := range eligible {
		if !seen[name] {
			t.Errorf("missing eligible INSERT %s", name)
		}
	}
	for name := range others {
		if !seen[name] {
			t.Errorf("missing other INSERT %s", name)
		}
	}
}
