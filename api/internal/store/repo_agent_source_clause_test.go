package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var repoAgentSourceInsertExempt = map[string]string{
	"CreateChatRun":             "chat runs have no repository",
	"CreateChatContinueRun":     "chat runs have no repository",
	"CreateJobRun":              "jobs run on the JobRunner",
	"CreateJudgeRun":            "judges run on the JudgeRunner",
	"CreateCodeCrossCheckChild": "cross-check children run on the CrossCheckRunner",
	"CreatePlanCrossCheckChild": "cross-check children run on the CrossCheckRunner",
	"CreateTaskReviewRun":       "task reviews run on the ReviewRunner",
}

func TestRepoAgentSourceRequiredStampedOnEveryRunInsert(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob queries: %v (%d files)", err, len(files))
	}
	insert := regexp.MustCompile(`(?i)INSERT\s+INTO\s+runs\s*\(`)
	seen := map[string]bool{}
	stamped := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
			name := strings.Fields(block)[0]
			loc := insert.FindStringIndex(block)
			if loc == nil {
				continue
			}
			seen[name] = true
			end := strings.Index(block[loc[1]:], ")")
			if end < 0 {
				t.Fatalf("%s (%s): unterminated INSERT INTO runs column list", name, f)
			}
			columns := block[loc[1] : loc[1]+end]
			sets := strings.Contains(columns, "repo_agent_source_required")
			if why, exempt := repoAgentSourceInsertExempt[name]; exempt {
				if sets {
					t.Errorf("%s (%s) is exempt (%s) but sets repo_agent_source_required", name, f, why)
				}
				continue
			}
			if !sets {
				t.Errorf("%s (%s) inserts a run without setting repo_agent_source_required; stamp it or add it to the exempt list", name, f)
				continue
			}
			stamped++
		}
	}
	for name := range repoAgentSourceInsertExempt {
		if !seen[name] {
			t.Errorf("exempt entry %s no longer inserts a run; drop it from the list", name)
		}
	}
	if stamped != 8 {
		t.Errorf("%d run INSERTs set repo_agent_source_required, want 8", stamped)
	}
}

func TestRepoAgentSourceClaimClauseCountPinned(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	clause := regexp.MustCompile(`\(NOT (?:r|run)\.repo_agent_source_required AND (?:r|run)\.repo_agent_source_commit IS NULL\) OR 'repo_agent_source_v1' = ANY\(`)
	guard := regexp.MustCompile(`NOT (?:r|run)\.ci_config_guard OR 'ci_config_guard_v1' = ANY\(`)
	got := map[string]int{}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		name := strings.Fields(block)[0]
		n := len(clause.FindAllString(block, -1))
		if n > 0 {
			got[name] = n
		}
		if g := len(guard.FindAllString(block, -1)); g != n {
			t.Errorf("%s mirrors ci_config_guard_v1 %d times but repo_agent_source_v1 %d times", name, g, n)
		}
	}
	want := map[string]int{
		"ClaimRun":                              2,
		"CountOnlineWorkersClaimableForRun":     1,
		"ListUnplaceableQueuedRunsForEphemeral": 1,
		"ListSaturationQueuedRunsForEphemeral":  2,
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s carries %d repo_agent_source_v1 clauses, want %d", name, got[name], n)
		}
	}
	for name, n := range got {
		if want[name] == 0 {
			t.Errorf("%s carries %d unexpected repo_agent_source_v1 clauses", name, n)
		}
	}
}
