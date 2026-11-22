package store_test

import (
	"os"
	"path/filepath"
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

// functionBlock returns the CREATE OR REPLACE FUNCTION <name>( ... $$; block inside section, or
// "" when the section does not define that function (a mention is not a definition).
func functionBlock(t *testing.T, file, section, name string) string {
	t.Helper()
	head := "CREATE OR REPLACE FUNCTION " + name + "("
	_, rest, ok := strings.Cut(section, head)
	if !ok {
		return ""
	}
	end := strings.Index(rest, "$$;")
	if end < 0 {
		t.Fatalf("%s: the %s definition has no closing $$;", file, name)
	}
	return head + rest[:end+len("$$;")]
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

	var prior, priorFile string
	for i := len(earlier) - 1; i >= 0; i-- {
		up, _ := migrationSection(t, earlier[i], false)
		if block := functionBlock(t, earlier[i], up, placementFunction); block != "" {
			prior, priorFile = block, earlier[i]
			break
		}
	}
	if prior == "" {
		t.Fatalf("no migration below %s defines %s in its Up section", placement, placementFunction)
	}
	t.Logf("%s Down is compared with the Up of %s", placement, priorFile)

	downSection, found := migrationSection(t, placement, true)
	if !found {
		t.Fatalf("%s has no Down section", placement)
	}
	down := functionBlock(t, placement, downSection, placementFunction)
	if down == "" {
		t.Fatalf("%s Down does not define %s", placement, placementFunction)
	}
	if down != prior {
		t.Fatalf("%s Down function (%d bytes) differs from %s Up function (%d bytes)", placement, len(down), priorFile, len(prior))
	}
}
