package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const placementFunctionCall = "fn_placement_admitting_workers("

var placementFunctionHeader = regexp.MustCompile(`(?i)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+fn_placement_admitting_workers\(`)

func stripSQLCommentLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// placementFunctionBody returns the comment-stripped body (between the $$ markers) of the LAST
// CREATE [OR REPLACE] FUNCTION fn_placement_admitting_workers in the store migrations (sorted by
// name, Down sections ignored). Test-only mirror of the helper in the store package's tests, which
// this package cannot import.
func placementFunctionBody(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	body := ""
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixed migrations directory
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		locs := placementFunctionHeader.FindAllStringIndex(up, -1)
		if len(locs) == 0 {
			continue
		}
		_, afterOpen, ok := strings.Cut(up[locs[len(locs)-1][1]:], "$$")
		if !ok {
			t.Fatalf("%s: fn_placement_admitting_workers has no opening $$", name)
		}
		inner, _, ok := strings.Cut(afterOpen, "$$")
		if !ok {
			t.Fatalf("%s: fn_placement_admitting_workers has no closing $$", name)
		}
		body = stripSQLCommentLines(inner)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("fn_placement_admitting_workers body not found (or empty) in migrations")
	}
	return body
}

// expandPlacementFunction appends the placement function body once per call of
// fn_placement_admitting_workers( outside comment lines, so a textual pin over a query that moved
// its per-worker block into the function still sees the clauses that execute.
func expandPlacementFunction(t *testing.T, text string) string {
	t.Helper()
	n := strings.Count(stripSQLCommentLines(text), placementFunctionCall)
	if n == 0 {
		return text
	}
	return text + strings.Repeat("\n"+placementFunctionBody(t), n)
}

// expandedQueryBlocks is namedQueryBlocks with each block's placement function calls expanded.
func expandedQueryBlocks(t *testing.T, src string) map[string]string {
	t.Helper()
	blocks := namedQueryBlocks(src)
	for name, body := range blocks {
		blocks[name] = expandPlacementFunction(t, body)
	}
	return blocks
}
