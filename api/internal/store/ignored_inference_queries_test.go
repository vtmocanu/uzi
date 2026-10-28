package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The live test executes today's creators. This guard also covers a future
// creator that copies a repo hint but forgets frozen hint origin or policy.
func TestRepoHintCreatorsSnapshotIgnoredInference(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	nameRE := regexp.MustCompile(`(?m)^-- name: (\w+)`)
	columnsRE := regexp.MustCompile(`(?s)INSERT INTO runs\s*\((.*?)\)\s*(?:VALUES|SELECT)`)
	repoHintRE := regexp.MustCompile(`(?i)SELECT\s+(?:\w+\.)?required_capabilities\s+FROM\s+repos\b`)
	repoPolicyRE := regexp.MustCompile(`(?i)SELECT\s+(?:\w+\.)?ignored_inferred_capabilities\s+FROM\s+repos\b`)
	aliasedReadRE := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+(repos|runs)\s+(?:AS\s+)?(\w+)`)
	seenCreateRun := false
	seenPlanChild := false
	for _, path := range files {
		raw, err := os.ReadFile(path) //nolint:gosec // Paths come only from the fixed queries directory.
		if err != nil {
			t.Fatal(err)
		}
		matches := nameRE.FindAllStringSubmatchIndex(string(raw), -1)
		for i, match := range matches {
			end := len(raw)
			if i+1 < len(matches) {
				end = matches[i+1][0]
			}
			query := string(raw[match[0]:end])
			copiesRepoHint := repoHintRE.MatchString(query)
			copiesPolicy := repoPolicyRE.MatchString(query)
			for _, source := range aliasedReadRE.FindAllStringSubmatch(query, -1) {
				if strings.EqualFold(source[1], "repos") && strings.Contains(query, source[2]+".required_capabilities") {
					copiesRepoHint = true
				}
				if strings.Contains(query, source[2]+".ignored_inferred_capabilities") {
					copiesPolicy = true
				}
			}
			if !strings.Contains(query, "INSERT INTO runs") || !copiesRepoHint {
				continue
			}
			name := string(raw[match[2]:match[3]])
			seenCreateRun = seenCreateRun || name == "CreateRun"
			seenPlanChild = seenPlanChild || name == "CreatePlanCrossCheckChild"
			columns := columnsRE.FindStringSubmatch(query)
			if len(columns) != 2 || !strings.Contains(columns[1], "ignored_inferred_capabilities") || !copiesPolicy || !strings.Contains(columns[1], "hinted_capabilities") {
				t.Errorf("%s in %s copies the repo hint without its frozen hint/policy snapshots", name, path)
			}
		}
	}
	if !seenCreateRun {
		t.Fatal("CreateRun was not inspected; query discovery is broken")
	}
	if !seenPlanChild {
		t.Fatal("CreatePlanCrossCheckChild was not inspected; JOIN discovery is broken")
	}
}
