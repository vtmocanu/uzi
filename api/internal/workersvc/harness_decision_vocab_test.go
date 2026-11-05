package workersvc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	harnessShapeAddRE   = regexp.MustCompile(`(?i)add\s+constraint\s+runs_harness_decision_shape\b`)
	harnessReasonListRE = regexp.MustCompile(`(?i)harness_decision_reason\s+(not\s+)?in\s*\(([^)]*)\)`)
	quotedReasonRE      = regexp.MustCompile(`'([a-z_]+)'`)
)

// TestHarnessDecisionReasonsMatchCheck guards the Go reason vocabulary against the
// runs_harness_decision_shape CHECK (#3845). It discovers the newest migration whose Up section
// adds that constraint, so a landing-time renumber does not stale it, and parses the reason
// lists instead of restating them. Each list is compared on its own: a reason dropped from the
// shape list alone must fail even though the auto_* clauses still name it.
func TestHarnessDecisionReasonsMatchCheck(t *testing.T) {
	path, body := latestHarnessDecisionShapeMigration(t, "../store/migrations")
	lists := harnessReasonListRE.FindAllStringSubmatch(body, -1)
	if len(lists) != 3 {
		t.Fatalf("%s: want exactly 3 harness_decision_reason lists (shape IN, reset IN, preferred NOT IN), got %d", path, len(lists))
	}
	auto := []string{harnessReasonAutoSoonestReset, harnessReasonAutoOnlyQualified, harnessReasonAutoFallback}
	for i, want := range []struct {
		not    bool
		values []string
	}{
		{false, harnessDecisionReasons}, // the shape's closed reason set
		{false, auto},                   // only auto_* decisions carry weekly resets
		{true, auto},                    // auto_* decisions record harness_preferred = 'claude'
	} {
		if got := lists[i][1] != ""; got != want.not {
			t.Fatalf("%s: list %d NOT = %v, want %v", path, i, got, want.not)
		}
		if got, w := sortedQuoted(lists[i][2]), sortedCopy(want.values); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Fatalf("harness decision vocabulary drifted in list %d of %s.\n  Go:  %v\n  SQL: %v", i, path, w, got)
		}
	}
}

func sortedQuoted(s string) []string {
	var out []string
	for _, m := range quotedReasonRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// latestHarnessDecisionShapeMigration returns the highest-numbered migration whose Up section
// adds runs_harness_decision_shape, and that Up section with SQL line comments stripped.
func latestHarnessDecisionShapeMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	best, bestBody, bestNum := "", "", -1
	for _, file := range files {
		num, ok := migrationNumber(filepath.Base(file))
		if !ok {
			continue
		}
		raw, err := os.ReadFile(file) //nolint:gosec // G304: test reads migration files from the fixed repo-relative ../store/migrations dir, never user input
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		up := string(raw)
		if i := strings.Index(up, "-- +goose Down"); i >= 0 {
			up = up[:i]
		}
		lines := strings.Split(up, "\n")
		for i, l := range lines {
			if j := strings.Index(l, "--"); j >= 0 {
				lines[i] = l[:j]
			}
		}
		up = strings.Join(lines, "\n")
		if !harnessShapeAddRE.MatchString(up) {
			continue
		}
		if num > bestNum {
			best, bestBody, bestNum = file, up, num
		}
	}
	if best == "" {
		t.Fatalf("no migration under %s adds runs_harness_decision_shape in its Up section", dir)
	}
	return best, bestBody
}
