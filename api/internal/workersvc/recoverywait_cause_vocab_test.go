package workersvc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestRecoveryWaitCauseVocabularyMatchesCheck (PRD #1590 C2) pins the worker-reportable
// recoveryWaitCauses UNION the server-only serverRecoveryWaitCauses to the value list of
// runs_recovery_wait_cause_check, parsed from the LATEST migration whose Up section declares
// that constraint (the TestFailOriginVocabularyMatchesCheck pattern: discover the migration,
// never pin its number, never restate the list). A cause Go writes that the CHECK rejects is a
// 23514 at the park write; a CHECK member neither set names is a promise nothing keeps. It
// also pins the two sets as disjoint, so a server-only cause can never become reportable.
func TestRecoveryWaitCauseVocabularyMatchesCheck(t *testing.T) {
	path, body := latestRecoveryWaitCauseCheckMigration(t, "../store/migrations")
	var fromSQL []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(body, -1) {
		fromSQL = append(fromSQL, m[1])
	}
	if len(fromSQL) == 0 {
		t.Fatalf("parsed no quoted values out of runs_recovery_wait_cause_check in %s", path)
	}
	var fromGo []string
	for c := range recoveryWaitCauses {
		if serverRecoveryWaitCauses[c] {
			t.Fatalf("cause %q is in BOTH the worker-reportable and the server-only set", c)
		}
		fromGo = append(fromGo, c)
	}
	for c := range serverRecoveryWaitCauses {
		fromGo = append(fromGo, c)
	}
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the recovery_wait cause vocabulary has drifted from %s.\n  Go (worker ∪ server): %v\n  SQL: %v",
			filepath.Base(path), fromGo, fromSQL)
	}
}

var recoveryWaitCauseCheckRE = regexp.MustCompile(
	`(?s)CONSTRAINT[[:space:]]+runs_recovery_wait_cause_check[[:space:]]+CHECK[[:space:]]*\((.*?)\)[[:space:]]*\)`,
)

// latestRecoveryWaitCauseCheckMigration returns the highest-numbered migration whose Up
// section (comment lines stripped, Down excluded) adds runs_recovery_wait_cause_check, and
// that constraint's CHECK body.
func latestRecoveryWaitCauseCheckMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	best, bestBody, bestNum := "", "", -1
	for _, file := range files {
		num, ok := migrationNumber(filepath.Base(file))
		if !ok || num <= bestNum {
			continue
		}
		raw, err := os.ReadFile(file) //nolint:gosec // G304: test reads migration files from the fixed repo-relative ../store/migrations dir, never user input
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if body, ok := upSectionRecoveryWaitCauseCheck(string(raw)); ok {
			best, bestBody, bestNum = file, body, num
		}
	}
	if best == "" {
		t.Fatalf("no migration under %s adds runs_recovery_wait_cause_check in its Up section; "+
			"the scan is reading the wrong directory or the CHECK vanished", dir)
	}
	return best, bestBody
}

func upSectionRecoveryWaitCauseCheck(raw string) (string, bool) {
	up := strings.Index(raw, "-- +goose Up")
	if up < 0 {
		return "", false
	}
	section := raw[up:]
	if down := strings.Index(section, "-- +goose Down"); down >= 0 {
		section = section[:down]
	}
	var stripped strings.Builder
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	m := recoveryWaitCauseCheckRE.FindStringSubmatch(stripped.String())
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// TestUpSectionRecoveryWaitCauseCheckIgnoresDown proves the parser reads the Up-section
// constraint, not the Down section's narrower restore.
func TestUpSectionRecoveryWaitCauseCheckIgnoresDown(t *testing.T) {
	raw := `-- +goose Up
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN ('up_a', 'up_b'));
-- +goose Down
ALTER TABLE runs ADD CONSTRAINT runs_recovery_wait_cause_check
    CHECK (recovery_wait_cause IS NULL OR recovery_wait_cause IN ('down_only'));
`
	body, ok := upSectionRecoveryWaitCauseCheck(raw)
	if !ok || !strings.Contains(body, "'up_a', 'up_b'") || strings.Contains(body, "down_only") {
		t.Fatalf("parsed body = %q ok=%v, want the Up-section values only", body, ok)
	}
}
