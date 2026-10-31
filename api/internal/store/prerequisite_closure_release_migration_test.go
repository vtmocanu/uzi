package store_test

import (
	"os"
	"strings"
	"testing"
)

const closureGuardStart = "-- +goose StatementBegin\nCREATE OR REPLACE FUNCTION recovery_inventory_hold_guard()"
const closureGuardEnd = "-- +goose StatementEnd"

func closureMigrationText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// closureGuardBlock returns the first recovery_inventory_hold_guard definition in s.
func closureGuardBlock(t *testing.T, s string) string {
	t.Helper()
	start := strings.Index(s, closureGuardStart)
	if start < 0 {
		t.Fatal("guard definition missing")
	}
	s = s[start:]
	return s[:strings.Index(s, closureGuardEnd)+len(closureGuardEnd)]
}

func TestPrerequisiteClosureReleaseMigrationPreservesGuard(t *testing.T) {
	preceding := closureGuardBlock(t, closureMigrationText(t, "00340_terminal_held_publication.sql"))
	parts := strings.Split(closureMigrationText(t, "00389_prerequisite_closure_release.sql"), "-- +goose Down\n")
	if len(parts) != 2 {
		t.Fatal("migration must have exactly one Down section")
	}
	// Down restores the preceding live guard byte for byte.
	if got := closureGuardBlock(t, parts[1]); got != preceding {
		t.Fatal("Down does not restore the 00340 guard body")
	}
	// Up differs from the preceding guard only by the two intended changes.
	up := closureGuardBlock(t, parts[0])
	const branchStart = "        -- Prerequisite closure release:"
	const branchEnd = "        IF NEW.final_disposition = 'archive' AND NEW.release_evidence = 'archive'"
	a := strings.Index(up, branchStart)
	b := strings.Index(up, branchEnd)
	if a < 0 || b < a || strings.Count(up, branchStart) != 1 || strings.Count(up, "prerequisite_closure") < 2 {
		t.Fatal("prerequisite_closure branch missing or misplaced")
	}
	if !strings.Contains(up[a:b], "NEW.release_evidence = 'prerequisite_closure'") {
		t.Fatal("new branch does not decide prerequisite_closure evidence")
	}
	rest := up[:a] + up[b:]
	for _, tuple := range []string{", NEW.final_closure_id)", ", OLD.final_closure_id)"} {
		if strings.Count(rest, tuple) != 1 {
			t.Fatalf("immutability tuple must carry %q exactly once", tuple)
		}
		rest = strings.Replace(rest, tuple, ")", 1)
	}
	if strings.Contains(rest, "final_closure_id") {
		t.Fatal("final_closure_id appears outside the immutability tuple and the new branch")
	}
	if rest != preceding {
		t.Fatal("unrelated guard branches changed")
	}
}

func TestPrerequisiteClosureReleaseMigrationRestoresEvidenceCheck(t *testing.T) {
	const addCheck = "ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_release_evidence_check\n"
	statement := func(s string) string {
		t.Helper()
		a := strings.Index(s, addCheck)
		if a < 0 {
			t.Fatal("evidence check statement missing")
		}
		s = s[a:]
		return s[:strings.Index(s, ";")+1]
	}
	want := statement(closureMigrationText(t, "00338_merged_predecessor_discard.sql"))
	if !strings.Contains(want, "'merged_predecessor_discard'") {
		t.Fatal("00338 Up evidence list unexpected")
	}
	parts := strings.Split(closureMigrationText(t, "00389_prerequisite_closure_release.sql"), "-- +goose Down\n")
	if len(parts) != 2 {
		t.Fatal("migration must have exactly one Down section")
	}
	if got := statement(parts[1]); got != want {
		t.Fatalf("Down evidence list differs from 00338:\n%s", got)
	}
	if strings.Contains(want, "prerequisite_closure") || !strings.Contains(parts[0], "'held_publication', 'merged_predecessor_discard', 'prerequisite_closure'))") {
		t.Fatal("Up must add prerequisite_closure to the evidence list")
	}
}
