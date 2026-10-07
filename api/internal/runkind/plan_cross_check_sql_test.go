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
// statements may set the snapshot; the other seven rely on its false default.
func TestPlanCrossCheckInsertParity(t *testing.T) {
	eligible := map[string]bool{
		"CreateRun": true, "CreatePromptRun": true, "CreateSelfImproveRun": true,
		"CreateCIFixRun": true, "CreateAutoMRReworkRun": true,
		"CreateManualMRReworkRunAndAdvance": true,
	}
	others := map[string]bool{
		"CreateChatRun": true, "CreateChatContinueRun": true, "CreateJobRun": true,
		"CreateJudgeRun": true, "CreateTaskRun": true, "CreateThenFixRun": true,
		"CreateTaskReviewRun": true, "CreatePlanCrossCheckChild": true,
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
