package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// runkind_sql_test.go pins the `kind NOT IN (...)` filters in store/queries/runtime.sql to
// the runkind property helpers. Two families carry a byte-identical filter today:
//
//   - the LISTING blocks CountInProgressRunsForUser, ListRunsForUser and ListActiveRunsAll:
//     excluded set must equal { k in All() : !Listed(k) } (i.e. {chat, judge}; a job is listed);
//   - the WALL-PARK blocks RequestWallParks and ParkRunsAtWall (PRD #1497 M1, replacing the
//     retired SweepRunningTimeout): excluded set must equal { k in All() : !WallTimed(k) }.
//
// All blocks are checked, not a representative one, so a divergence in any sibling is red.
// This file lives inside the api/ module, so Go's test cache rechecks it.

// jobWallExclusionDeferred is true while the wall-park blocks do not yet exclude 'job'. PRD
// #1908 M1 adds the kind and the WallTimed property but deliberately does not touch the park
// queries (D-E, the job-never-parks change, lands with M3). While true, the wall blocks may omit
// exactly the job kind from WallTimed's excluded set; M3 flips this to false, after which the
// blocks must match WallTimed exactly.
const jobWallExclusionDeferred = true

func TestRuntimeSQLKindFilterMatchesListed(t *testing.T) {
	assertKindFilters(t, []string{"CountInProgressRunsForUser", "ListRunsForUser", "ListActiveRunsAll"},
		func(k string) bool { return !Listed(k) }, nil)
}

func TestRuntimeSQLWallParkKindFilterMatchesWallTimed(t *testing.T) {
	var deferred map[string]bool
	if jobWallExclusionDeferred {
		deferred = map[string]bool{Job: true}
	}
	assertKindFilters(t, []string{"RequestWallParks", "ParkRunsAtWall"},
		func(k string) bool { return !WallTimed(k) }, deferred)
}

// TestKindPropertyHelpersForJob pins the job kind's row in the property table.
func TestKindPropertyHelpersForJob(t *testing.T) {
	if !Listed(Job) {
		t.Error("a job must be Listed (it is a user-visible run)")
	}
	if PlanningCapable(Job) {
		t.Error("a job must not be PlanningCapable (no plan gate)")
	}
	if WallTimed(Job) {
		t.Error("a job must not be WallTimed (it fails at its wall, never parks)")
	}
	if JudgeEligible(Job) {
		t.Error("a job must not be JudgeEligible")
	}
	for _, k := range All() {
		if k == Chat || k == Judge || k == Job {
			continue
		}
		if !Listed(k) || !PlanningCapable(k) || !WallTimed(k) {
			t.Errorf("kind %q must stay Listed, PlanningCapable and WallTimed", k)
		}
	}
}

// assertKindFilters checks that each named block's `kind NOT IN (...)` set equals
// { k in All() : excluded(k) } minus the allowedMissing kinds.
func assertKindFilters(t *testing.T, wantBlocks []string, excluded func(string) bool, allowedMissing map[string]bool) {
	t.Helper()
	path := filepath.Join("..", "store", "queries", "runtime.sql")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative query file path
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	src := string(raw)

	want := map[string]bool{}
	var wantList []string
	for _, k := range All() {
		if excluded(k) && !allowedMissing[k] {
			want[k] = true
			wantList = append(wantList, k)
		}
	}
	sort.Strings(wantList)

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
			t.Errorf("query block %q excludes %v; expected %v", name, gotList, wantList)
			continue
		}
		for k := range want {
			if !got[k] {
				t.Errorf("query block %q excludes %v; expected %v", name, gotList, wantList)
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
