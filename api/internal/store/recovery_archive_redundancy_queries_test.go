package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Exactly one statement in queries/*.sql writes reason 'published_redundant'. The capture guard
// checks the proof's shape and binding, not that the forge proof ran, so the narrow set of
// writers is part of the boundary: a second writer would be a second way to spend the exemption.
func TestPublishedRedundantHasOneWriter(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries: %v %v", files, err)
	}
	name := regexp.MustCompile(`(?m)^-- name: (\w+)`)
	var writers []string
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatal(err)
		}
		// Statements start at their name line; comment lines are not code.
		parts := name.Split(string(raw), -1)
		names := name.FindAllStringSubmatch(string(raw), -1)
		for i, n := range names {
			var code []string
			for _, line := range strings.Split(parts[i+1], "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "--") {
					code = append(code, line)
				}
			}
			if strings.Contains(strings.Join(code, "\n"), "published_redundant") {
				writers = append(writers, filepath.Base(f)+":"+n[1])
			}
		}
	}
	if len(writers) != 1 || writers[0] != "recovery.sql:ExpireRedundantCapture" {
		t.Fatalf("statements naming published_redundant: %v, want exactly recovery.sql:ExpireRedundantCapture", writers)
	}
}
