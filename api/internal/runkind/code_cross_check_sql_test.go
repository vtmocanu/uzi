package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCodeCrossCheckableKinds(t *testing.T) {
	want := map[string]bool{Issue: true, Prompt: true, SelfImprove: true, CIFix: true, MRRework: true, Task: true}
	for _, kind := range All() {
		if got := CodeCrossCheckable(kind); got != want[kind] {
			t.Errorf("CodeCrossCheckable(%q)=%v want=%v", kind, got, want[kind])
		}
	}
}

func TestCodeCrossCheckInsertParity(t *testing.T) {
	eligible := map[string]bool{
		"CreateRun": true, "CreatePromptRun": true, "CreateSelfImproveRun": true, "CreateCIFixRun": true,
		"CreateAutoMRReworkRun": true, "CreateManualMRReworkRunAndAdvance": true,
		"CreateTaskRun": true, "CreateThenFixRun": true, "CreateTaskReviewRun": true,
	}
	others := map[string]bool{"CreateChatRun": true, "CreateChatContinueRun": true, "CreateJobRun": true, "CreateJudgeRun": true, "CreatePlanCrossCheckChild": true, "CreateCodeCrossCheckChild": true}
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
			var sql strings.Builder
			for _, line := range strings.Split(body, "\n") {
				if at := strings.Index(line, "--"); at >= 0 {
					line = line[:at]
				}
				sql.WriteString(line)
				sql.WriteByte('\n')
			}
			at := insert.FindStringIndex(sql.String())
			if at == nil {
				continue
			}
			if seen[name] {
				t.Errorf("duplicate runs INSERT %s", name)
			}
			seen[name] = true
			if !eligible[name] && !others[name] {
				t.Errorf("unenumerated runs INSERT %s", name)
			}
			columns := strings.SplitN(sql.String()[at[0]:], ")", 2)[0]
			got := strings.Contains(columns, "code_cross_check_required")
			if got != eligible[name] {
				t.Errorf("%s: snapshot column=%v eligible=%v", name, got, eligible[name])
			}
			if got && !strings.Contains(sql.String(), "code_cross_check_enabled") {
				t.Errorf("%s: snapshot does not read owner consent", name)
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
			t.Errorf("missing excluded INSERT %s", name)
		}
	}
}
