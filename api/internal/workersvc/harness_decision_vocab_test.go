package workersvc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestHarnessDecisionReasonsMatchCheck guards the Go reason vocabulary against the
// runs_harness_decision_shape CHECK (#3845). It discovers the newest migration whose Up section
// adds that constraint, so a landing-time renumber does not stale it, and parses the reason
// lists instead of restating them.
func TestHarnessDecisionReasonsMatchCheck(t *testing.T) {
	path, body := latestHarnessDecisionShapeMigration(t, "../store/migrations")
	lists := regexp.MustCompile(`harness_decision_reason\s+IN\s*\(([^)]*)\)`).FindAllStringSubmatch(body, -1)
	if len(lists) == 0 {
		t.Fatalf("no harness_decision_reason IN (...) list in %s; the guard is reading the wrong thing", path)
	}
	quoted := regexp.MustCompile(`'([a-z_]+)'`)
	seen := map[string]bool{}
	for _, l := range lists {
		for _, m := range quoted.FindAllStringSubmatch(l[1], -1) {
			seen[m[1]] = true
		}
	}
	fromSQL := make([]string, 0, len(seen))
	for v := range seen {
		fromSQL = append(fromSQL, v)
	}
	fromGo := append([]string(nil), harnessDecisionReasons...)
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("harness decision vocabulary drifted.\n  Go:  %v\n  SQL: %v (%s)", fromGo, fromSQL, path)
	}
	// The reset-carrying clause must name exactly the automatic reasons.
	auto := lists[len(lists)-1][1]
	var autoSQL []string
	for _, m := range quoted.FindAllStringSubmatch(auto, -1) {
		autoSQL = append(autoSQL, m[1])
	}
	sort.Strings(autoSQL)
	wantAuto := []string{harnessReasonAutoFallback, harnessReasonAutoOnlyQualified, harnessReasonAutoSoonestReset}
	sort.Strings(wantAuto)
	if strings.Join(autoSQL, ",") != strings.Join(wantAuto, ",") {
		t.Fatalf("weekly-reset clause reasons = %v, want %v (%s)", autoSQL, wantAuto, path)
	}
}

// latestHarnessDecisionShapeMigration returns the highest-numbered migration whose Up section
// adds runs_harness_decision_shape, and that Up section.
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
		if !strings.Contains(up, "ADD CONSTRAINT runs_harness_decision_shape") {
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
