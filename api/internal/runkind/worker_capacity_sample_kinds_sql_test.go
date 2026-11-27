package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// worker_capacity_sample_kinds_sql_test.go pins the audited-kind allowlist inside
// SampleWorkerCapacityBatch (store/queries/worker_capacity_samples.sql, issue #4693) to All().
//
// The query records a worker sample's occupancy as attribution 'owner' only when every
// slot-holding run has a kind in that list. A kind missing from it would silently record
// 'unavailable' for any worker holding such a run, so this test makes a new kind fail the build
// until the owner-attribution audit (PRD #4693 Decision Log) has been repeated for it and the
// list extended. It reads the query file, which lives inside this module, so Go's test cache
// rechecks it.
func TestWorkerCapacitySampleKindAllowlistMatchesRunkind(t *testing.T) {
	path := filepath.Join("..", "store", "queries", "worker_capacity_samples.sql")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a fixed repo-relative query file path
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	var code []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	src := strings.Join(code, "\n")

	// The allowlist is the one `kind IN (...)` list of string literals; the other kind
	// comparisons in the statement are single `=` / `<>` tests.
	lists := regexp.MustCompile(`(?is)\bkind\s+IN\s*\(([^)]*)\)`).FindAllStringSubmatch(src, -1)
	if len(lists) != 1 {
		t.Fatalf("expected exactly one `kind IN (...)` allowlist in %s, found %d — did the query change?", path, len(lists))
	}
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(lists[0][1], -1) {
		got[m[1]] = true
	}
	var gotList, wantList []string
	for k := range got {
		gotList = append(gotList, k)
	}
	want := map[string]bool{}
	for _, k := range All() {
		want[k] = true
		wantList = append(wantList, k)
	}
	sort.Strings(gotList)
	sort.Strings(wantList)
	equal := len(got) == len(want)
	for k := range want {
		if !got[k] {
			equal = false
		}
	}
	if !equal {
		t.Errorf("SampleWorkerCapacityBatch audited kinds = %v; runkind.All() = %v. Audit every writer that can put the new kind in a slot status "+
			"(owner-gated claim or slot move keeping worker_id), then extend the list", gotList, wantList)
	}
}
