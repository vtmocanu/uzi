package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createFunctionBlock returns the first CREATE OR REPLACE FUNCTION ... $$ ... $$; block of one
// section of a migration file (the Up section when down is false, the Down section otherwise).
func createFunctionBlock(t *testing.T, file string, down bool) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("migrations", file)) //nolint:gosec // fixed migrations directory
	if err != nil {
		t.Fatal(err)
	}
	up, dn, found := strings.Cut(string(raw), "-- +goose Down")
	section := up
	if down {
		if !found {
			t.Fatalf("%s has no Down section", file)
		}
		section = dn
	}
	_, rest, ok := strings.Cut(section, "CREATE OR REPLACE FUNCTION")
	if !ok {
		t.Fatalf("%s has no CREATE OR REPLACE FUNCTION in its %s section", file, map[bool]string{false: "Up", true: "Down"}[down])
	}
	end := strings.Index(rest, "$$;")
	if end < 0 {
		t.Fatalf("%s: the function has no closing $$;", file)
	}
	return "CREATE OR REPLACE FUNCTION" + rest[:end+len("$$;")]
}

// The run env placement migration's Down restores the placement function exactly as the phase
// models migration created it: the Down body equals 00499's Up body.
func TestRunEnvPlacementDownRestoresPhaseModelsFunction(t *testing.T) {
	down := createFunctionBlock(t, "00502_run_env_placement.sql", true)
	prior := createFunctionBlock(t, "00499_placement_phase_models.sql", false)
	if down != prior {
		t.Fatalf("00502 Down function (%d bytes) differs from 00499 Up function (%d bytes)", len(down), len(prior))
	}
}
