package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// runkind_sql_test.go pins the `kind NOT IN (...)` filter in
// store/queries/runtime.sql to Listed(). Four named query blocks carry the
// byte-identical filter today — CountInProgressRunsForUser, ListRunsForUser,
// ListActiveRunsAll, SweepRunningTimeout — and each block's excluded set must
// equal { k in All() : !Listed(k) } (i.e. {chat, judge}). All four are checked,
// not a representative one, so a divergence introduced in any sibling is red.
// This file lives inside the api/ module, so Go's test cache rechecks it.

func TestRuntimeSQLKindFilterMatchesListed(t *testing.T) {
	path := filepath.Join("..", "store", "queries", "runtime.sql")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative query file path
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	src := string(raw)

	// Expected excluded set: the kinds that are NOT Listed.
	want := map[string]bool{}
	var wantList []string
	for _, k := range All() {
		if !Listed(k) {
			want[k] = true
			wantList = append(wantList, k)
		}
	}
	sort.Strings(wantList)

	wantBlocks := []string{
		"CountInProgressRunsForUser",
		"ListRunsForUser",
		"ListActiveRunsAll",
		"SweepRunningTimeout",
	}

	// Split the file into named-query blocks on `-- name:` boundaries.
	blocks := namedQueryBlocks(src)

	// Column may be `kind` or `r.kind`.
	notInRe := regexp.MustCompile(`(?is)\b(?:\w+\.)?kind\s+NOT\s+IN\s*\(([^)]*)\)`)
	litRe := regexp.MustCompile(`'([^']+)'`)

	for _, name := range wantBlocks {
		body, ok := blocks[name]
		if !ok {
			t.Errorf("query block %q not found in %s", name, path)
			continue
		}
		m := notInRe.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("query block %q has no `kind NOT IN (...)` filter — did it change?", name)
			continue
		}
		got := map[string]bool{}
		var gotList []string
		for _, mm := range litRe.FindAllStringSubmatch(m[1], -1) {
			got[mm[1]] = true
			gotList = append(gotList, mm[1])
		}
		sort.Strings(gotList)

		if len(got) != len(want) {
			t.Errorf("query block %q excludes %v; expected %v (the non-Listed kinds)", name, gotList, wantList)
			continue
		}
		for k := range want {
			if !got[k] {
				t.Errorf("query block %q excludes %v; expected %v (the non-Listed kinds)", name, gotList, wantList)
				break
			}
		}
	}
}

// namedQueryBlocks splits a sqlc query file into a map from query name to the
// text of that block (everything up to the next `-- name:` boundary).
func namedQueryBlocks(src string) map[string]string {
	nameRe := regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	blocks := make(map[string]string, len(locs))
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		start := loc[0]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		blocks[strings.TrimSpace(name)] = src[start:end]
	}
	return blocks
}
