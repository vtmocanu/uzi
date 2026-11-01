package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The run_lead_configs table is append-only (PRD #3244 section 1): a BEFORE UPDATE trigger rejects an
// UPDATE, and this scanner proves that no query or hand-written store code issues an UPDATE or a DELETE
// against it. Deletion happens only through the runs foreign-key cascade. The scan covers the sqlc query
// files and the hand-written store Go files; generated *.sql.go files are derived from the query files,
// and *_test.go files are excluded because the trigger and cascade tests must issue those statements.
var (
	leadConfigUpdateRe = regexp.MustCompile(`(?i)\bUPDATE\s+(public\.)?run_lead_configs\b`)
	leadConfigDeleteRe = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(public\.)?run_lead_configs\b`)
)

// stripLineComments drops whole-line SQL (--) and Go (//) comments, so prose that mentions the forbidden
// statements does not trip the scan.
func stripLineComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--") || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestRunLeadConfigsAppendOnlyScanner(t *testing.T) {
	// Calibrate first: the patterns must match the shapes they exist to forbid.
	for _, canary := range []string{"update  public.run_lead_configs", "DELETE\nFROM run_lead_configs"} {
		if !leadConfigUpdateRe.MatchString(canary) && !leadConfigDeleteRe.MatchString(canary) {
			t.Fatalf("scanner patterns do not match canary %q", canary)
		}
	}
	if !leadConfigUpdateRe.MatchString("update  public.run_lead_configs") || !leadConfigDeleteRe.MatchString("DELETE\nFROM run_lead_configs") {
		t.Fatal("each pattern must match its own canary")
	}
	if got := stripLineComments("-- UPDATE run_lead_configs\n\t// DELETE FROM run_lead_configs\nSELECT 1"); leadConfigUpdateRe.MatchString(got) || leadConfigDeleteRe.MatchString(got) {
		t.Fatalf("comment lines must be stripped before matching: %q", got)
	}

	var files []string
	sqlFiles, err := filepath.Glob("queries/*.sql")
	if err != nil || len(sqlFiles) == 0 {
		t.Fatalf("no query files found: %v", err)
	}
	files = append(files, sqlFiles...)
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range goFiles {
		if strings.HasSuffix(f, ".sql.go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		files = append(files, f)
	}
	if len(files) < 10 {
		t.Fatalf("scanner found only %d files: %v", len(files), files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: files enumerated by Glob in this package directory
		if err != nil {
			t.Fatal(err)
		}
		src := stripLineComments(string(b))
		if leadConfigUpdateRe.MatchString(src) {
			t.Errorf("%s issues an UPDATE against run_lead_configs, which is append-only", f)
		}
		if leadConfigDeleteRe.MatchString(src) {
			t.Errorf("%s issues a DELETE against run_lead_configs, which is append-only", f)
		}
	}
}

// The ON CONFLICT target is named so a later unique index on the table cannot be absorbed as a duplicate
// of the primary key (PRD #3244 section 4).
func TestInsertRunLeadConfigFencedNamesItsConflictTarget(t *testing.T) {
	const want = "ON CONFLICT (run_id, claim_generation, phase, seq) DO NOTHING"
	if !strings.Contains(insertRunLeadConfigFenced, want) {
		t.Fatalf("InsertRunLeadConfigFenced lost its named conflict target %q:\n%s", want, insertRunLeadConfigFenced)
	}
	if !strings.Contains(insertRunLeadConfigFenced, "FOR NO KEY UPDATE") {
		t.Fatal("InsertRunLeadConfigFenced must lock the run row FOR NO KEY UPDATE")
	}
}
