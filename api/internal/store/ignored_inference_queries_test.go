package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The live test executes today's creators. This guard also covers a future
// creator that copies a repo hint but forgets the matching frozen policy.
func TestRepoHintCreatorsSnapshotIgnoredInference(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	nameRE := regexp.MustCompile(`(?m)^-- name: (\w+)`)
	columnsRE := regexp.MustCompile(`(?s)INSERT INTO runs\s*\((.*?)\)\s*(?:VALUES|SELECT)`)
	repoHintRE := regexp.MustCompile(`(?i)SELECT\s+(?:\w+\.)?required_capabilities\s+FROM\s+repos\b`)
	repoPolicyRE := regexp.MustCompile(`(?i)SELECT\s+(?:\w+\.)?ignored_inferred_capabilities\s+FROM\s+repos\b`)
	seenCreateRun := false
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
			if !strings.Contains(query, "INSERT INTO runs") || !repoHintRE.MatchString(query) {
				continue
			}
			name := string(raw[match[2]:match[3]])
			seenCreateRun = seenCreateRun || name == "CreateRun"
			columns := columnsRE.FindStringSubmatch(query)
			if len(columns) != 2 || !strings.Contains(columns[1], "ignored_inferred_capabilities") || !repoPolicyRE.MatchString(query) {
				t.Errorf("%s in %s copies the repo hint without its ignored-inference snapshot", name, path)
			}
		}
	}
	if !seenCreateRun {
		t.Fatal("CreateRun was not inspected; query discovery is broken")
	}
}
