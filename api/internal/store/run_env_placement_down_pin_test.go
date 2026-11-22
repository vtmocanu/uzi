package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const placementFunction = "fn_placement_admitting_workers"

// migrationSection returns the Up section (down false) or the Down section (down true) of one
// migration file; ok is false when a Down section is asked for and the file has none.
func migrationSection(t *testing.T, file string, down bool) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("migrations", file)) //nolint:gosec // fixed migrations directory
	if err != nil {
		t.Fatal(err)
	}
	up, dn, found := strings.Cut(string(raw), "-- +goose Down")
	if down {
		return dn, found
	}
	return up, true
}

// functionBlock returns the CREATE [OR REPLACE] FUNCTION <name>( ... $$; block inside section, or
// "" when the section does not define that function (a mention or a commented-out definition is
// not a definition).
func functionBlock(t *testing.T, file, section, name string) string {
	t.Helper()
	block, closed := findFunctionBlock(section, name)
	if block != "" && !closed {
		t.Fatalf("%s: the %s definition has no closing $$;", file, name)
	}
	return block
}

// findFunctionBlock is the pure half of functionBlock: the block runs from the start of the first
// uncommented definition to its closing $$;. closed is false when a definition has no closing $$;
// (the block then runs to the end of the section).
func findFunctionBlock(section, name string) (block string, closed bool) {
	def := regexp.MustCompile(`(?is)\bCREATE\s+(OR\s+REPLACE\s+)?FUNCTION\s+(public\.)?` + regexp.QuoteMeta(name) + `\s*\(`)
	for _, loc := range def.FindAllStringIndex(section, -1) {
		lineStart := strings.LastIndex(section[:loc[0]], "\n") + 1
		if strings.Contains(section[lineStart:loc[0]], "--") {
			continue
		}
		rest := section[loc[0]:]
		end := strings.Index(rest, "$$;")
		if end < 0 {
			return rest, false
		}
		return rest[:end+len("$$;")], true
	}
	return "", false
}

// fromFunctionKeyword is a definition block from its FUNCTION keyword on, so a CREATE FUNCTION
// predecessor and a CREATE OR REPLACE FUNCTION Down with the same body compare equal.
func fromFunctionKeyword(block string) string {
	loc := regexp.MustCompile(`(?i)\bFUNCTION\b`).FindStringIndex(block)
	if loc == nil {
		return block
	}
	return block[loc[0]:]
}

func TestFromFunctionKeyword(t *testing.T) {
	a := fromFunctionKeyword("CREATE FUNCTION fn_x() AS $$ x $$;")
	b := fromFunctionKeyword("CREATE OR REPLACE FUNCTION fn_x() AS $$ x $$;")
	if a != b || a != "FUNCTION fn_x() AS $$ x $$;" {
		t.Fatalf("got %q and %q", a, b)
	}
	if fromFunctionKeyword("CREATE FUNCTION fn_x() AS $$ y $$;") == b {
		t.Fatal("different bodies compare equal")
	}
}

func TestFindFunctionBlock(t *testing.T) {
	const body = "() RETURNS int AS $$ SELECT 1 $$;"
	cases := []struct {
		name, section, want string
	}{
		{"plain create function", "-- +goose Up\nCREATE FUNCTION fn_x" + body + "\nSELECT 2;", "CREATE FUNCTION fn_x" + body},
		{"or replace, schema, spacing", "create  or\treplace function public.fn_x  (" + ") AS $$ x $$;", "create  or\treplace function public.fn_x  (" + ") AS $$ x $$;"},
		{"mention only", "-- fn_x is defined elsewhere\nSELECT fn_x();\nCOMMENT ON FUNCTION fn_x() IS 'y';", ""},
		{"commented-out definition", "-- CREATE FUNCTION fn_x" + body + "\nSELECT 1;", ""},
		{"commented-out then real", "-- CREATE FUNCTION fn_x() $$ old $$;\nCREATE OR REPLACE FUNCTION fn_x() AS $$ new $$;", "CREATE OR REPLACE FUNCTION fn_x() AS $$ new $$;"},
		{"other function", "CREATE FUNCTION fn_xy" + body, ""},
	}
	for _, c := range cases {
		got, closed := findFunctionBlock(c.section, "fn_x")
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want != "" && !closed {
			t.Errorf("%s: want closed block", c.name)
		}
	}
}

// predecessorPlacementFunction returns the Up definition of the placement function from the
// highest-numbered file in earlier (sorted ascending) that defines it, and that file's name.
func predecessorPlacementFunction(t *testing.T, placement string, earlier []string) (block, file string) {
	t.Helper()
	for i := len(earlier) - 1; i >= 0; i-- {
		up, _ := migrationSection(t, earlier[i], false)
		if b := functionBlock(t, earlier[i], up, placementFunction); b != "" {
			return b, earlier[i]
		}
	}
	t.Fatalf("no migration below %s defines %s in its Up section", placement, placementFunction)
	return "", ""
}

// The run env placement migration's Down restores fn_placement_admitting_workers exactly as the
// latest earlier migration that defines it left it. That predecessor is derived, not named: the
// run env placement migration is the one file matching *_run_env_placement.sql, and the
// predecessor is the highest-numbered earlier migration whose Up section defines the function.
func TestRunEnvPlacementDownRestoresPredecessorFunction(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("migrations", "*_run_env_placement.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one migrations/*_run_env_placement.sql, got %v", matches)
	}
	placement := filepath.Base(matches[0])

	all, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var earlier []string
	for _, m := range all {
		if base := filepath.Base(m); base < placement {
			earlier = append(earlier, base)
		}
	}
	sort.Strings(earlier)

	prior, priorFile := predecessorPlacementFunction(t, placement, earlier)
	t.Logf("%s Down is compared with the Up of %s", placement, priorFile)

	downSection, found := migrationSection(t, placement, true)
	if !found {
		t.Fatalf("%s has no Down section", placement)
	}
	down := functionBlock(t, placement, downSection, placementFunction)
	if down == "" {
		t.Fatalf("%s Down does not define %s", placement, placementFunction)
	}
	if !regexp.MustCompile(`(?is)^CREATE\s+OR\s+REPLACE\s+FUNCTION\b`).MatchString(down) {
		t.Fatalf("%s Down must redefine %s with CREATE OR REPLACE FUNCTION", placement, placementFunction)
	}
	if fromFunctionKeyword(down) != fromFunctionKeyword(prior) {
		t.Fatalf("%s Down function (%d bytes) differs from %s Up function (%d bytes)", placement, len(down), priorFile, len(prior))
	}
}

// The run env placement migration's Up is the predecessor's Up function plus exactly the
// run_env_v1 claim clause (its comment lines and its one AND line), so a rebuilt Up cannot drop a
// predecessor clause. Removing those lines from the Up function must give the predecessor's Up
// function from the FUNCTION keyword on.
func TestRunEnvPlacementUpAddsOnlyRunEnvClause(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("migrations", "*_run_env_placement.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one migrations/*_run_env_placement.sql, got %v", matches)
	}
	placement := filepath.Base(matches[0])
	all, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var earlier []string
	for _, m := range all {
		if base := filepath.Base(m); base < placement {
			earlier = append(earlier, base)
		}
	}
	sort.Strings(earlier)
	prior, priorFile := predecessorPlacementFunction(t, placement, earlier)

	upSection, _ := migrationSection(t, placement, false)
	up := functionBlock(t, placement, upSection, placementFunction)
	if up == "" {
		t.Fatalf("%s Up does not define %s", placement, placementFunction)
	}
	// Remove the clause line and the contiguous comment lines directly above it.
	lines := strings.Split(up, "\n")
	var stripped []string
	removedClause := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "AND ('run_env_v1' = ANY(") {
			removedClause++
			for len(stripped) > 0 && strings.HasPrefix(strings.TrimSpace(stripped[len(stripped)-1]), "--") {
				stripped = stripped[:len(stripped)-1]
			}
			continue
		}
		stripped = append(stripped, line)
	}
	if removedClause != 1 {
		t.Fatalf("%s Up must carry exactly one run_env_v1 claim clause, found %d", placement, removedClause)
	}
	if got, want := fromFunctionKeyword(strings.Join(stripped, "\n")), fromFunctionKeyword(prior); got != want {
		t.Fatalf("%s Up minus the run_env_v1 clause (%d bytes) differs from %s Up function (%d bytes)", placement, len(got), priorFile, len(want))
	}
}
