package store_test

import (
	"fmt"
	"io/fs"
	"os"
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
	queries := os.DirFS("queries")
	entries, err := fs.ReadDir(queries, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatal("no query files")
	}
	insert := regexp.MustCompile(`(?i)INSERT\s+INTO\s+runs\s*\(`)
	seen := map[string]bool{}
	stamped := 0
	for _, f := range files {
		raw, err := fs.ReadFile(queries, f)
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
			if err := assertInsertsTrue(columns, block[loc[1]+end+1:], "repo_agent_source_required"); err != nil {
				t.Errorf("%s (%s): %v", name, f, err)
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

// assertInsertsTrue checks that column is inserted as the literal true. rest is the
// statement text after the INSERT column list: a VALUES tuple or an INSERT ... SELECT.
func assertInsertsTrue(columnList, rest, column string) error {
	var columns []string
	for _, c := range strings.Split(columnList, ",") {
		columns = append(columns, strings.TrimSpace(c))
	}
	index := -1
	for i, c := range columns {
		if c == column {
			index = i
		}
	}
	if index < 0 {
		return fmt.Errorf("%s is not a listed column", column)
	}
	values, err := insertedValues(rest)
	if err != nil {
		return err
	}
	if len(values) != len(columns) {
		return fmt.Errorf("%d inserted values for %d columns", len(values), len(columns))
	}
	if got := strings.TrimSpace(values[index]); !strings.EqualFold(got, "true") {
		return fmt.Errorf("inserts %s = %q, want true", column, got)
	}
	return nil
}

// insertedValues splits the top-level value expressions of a VALUES tuple or of an
// INSERT ... SELECT list, skipping string literals and -- comments.
func insertedValues(rest string) ([]string, error) {
	text := strings.TrimSpace(rest)
	upper := strings.ToUpper(text)
	var depth int
	var start int
	switch {
	case strings.HasPrefix(upper, "VALUES"):
		open := strings.Index(text, "(")
		if open < 0 {
			return nil, fmt.Errorf("VALUES without a tuple")
		}
		start, depth = open+1, 1
	case strings.HasPrefix(upper, "SELECT"):
		start, depth = len("SELECT"), 0
	default:
		return nil, fmt.Errorf("unrecognised INSERT body")
	}
	base := depth
	var values []string
	itemStart := start
	stop := regexp.MustCompile(`(?i)^\s(FROM|WHERE|RETURNING|ON\s+CONFLICT)\b`)
	for i := start; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\'':
			for i++; i < len(text) && text[i] != '\''; i++ {
			}
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < base {
				return append(values, text[itemStart:i]), nil
			}
		case c == ',' && depth == base:
			values = append(values, text[itemStart:i])
			itemStart = i + 1
		case c == ';' && depth == base:
			return append(values, text[itemStart:i]), nil
		case depth == base && base == 0 && stop.MatchString(text[i:]):
			return append(values, text[itemStart:i]), nil
		}
	}
	return nil, fmt.Errorf("unterminated INSERT body")
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
