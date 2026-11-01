package codexselect

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestReasonVocabularyMatchesCheck pins the Go vocabulary to the runs_codex_select_reason_check
// IN-list of the migration that adds it. The migration is found by its name suffix, not its
// number, because migration numbers are renumbered at landing. It parses the SQL rather than
// restating the list: a second hand-typed copy is what it exists to prevent.
func TestReasonVocabularyMatchesCheck(t *testing.T) {
	matches, err := filepath.Glob("../store/migrations/[0-9][0-9][0-9][0-9][0-9]_codex_account_autoselect.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one codex_account_autoselect migration, got %v (err %v)", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var stmt strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stmt.WriteString(line + "\n")
	}
	// Only the Up section: Down never re-adds this constraint.
	up := strings.SplitN(stmt.String(), "+goose Down", 2)[0]
	anchor := regexp.MustCompile(`codex_select_reason IN \(([^)]*)\)`)
	in := anchor.FindStringSubmatch(up)
	if in == nil {
		t.Fatalf("no codex_select_reason IN-list in %s; the guard is reading the wrong thing", matches[0])
	}
	var fromSQL, fromGo []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(in[1], -1) {
		fromSQL = append(fromSQL, m[1])
	}
	for _, r := range AllReasons() {
		fromGo = append(fromGo, string(r))
	}
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("reason vocabulary drifted.\n  Go:  %v\n  SQL: %v", fromGo, fromSQL)
	}
}

func TestAllReasonsIsAFreshSlice(t *testing.T) {
	a := AllReasons()
	a[0] = "mutated"
	if AllReasons()[0] == "mutated" {
		t.Fatal("AllReasons shares backing storage between calls")
	}
	if len(AllReasons()) != 6 {
		t.Fatalf("want 6 reasons, got %d", len(AllReasons()))
	}
}
