package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guardBlock returns the fail-closed admission guard: from the lock_timeout statement through the
// end of the DO block, with goose annotation lines and blank lines removed and every line
// trimmed, so the three copies compare byte for byte.
func guardBlock(t *testing.T, body string) string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "-- +goose") {
			continue
		}
		lines = append(lines, trimmed)
	}
	text := strings.Join(lines, "\n")
	start := strings.Index(text, "SET LOCAL lock_timeout")
	if start < 0 {
		t.Fatal("no lock_timeout statement")
	}
	opener := strings.Index(text[start:], "DO $$")
	if opener < 0 {
		t.Fatal("no DO block")
	}
	closer := strings.Index(text[start+opener+len("DO $$"):], "$$;")
	if closer < 0 {
		t.Fatal("DO block is not terminated")
	}
	return text[start : start+opener+len("DO $$")+closer+len("$$;")]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repository paths
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The operator's drain script and the guard at the top of both Downs are the same check; a drift
// would let a rollback pass one and trip the other.
func TestGatedCrossCheckGuardTextParity(t *testing.T) {
	script := guardBlock(t, readFile(t, filepath.Join("..", "..", "..", "scripts", "gated-cross-check-drain-check.sql")))
	if !strings.Contains(script, "LOCK TABLE runs IN SHARE ROW EXCLUSIVE MODE;") ||
		!strings.Contains(script, "status NOT IN ('completed', 'failed', 'cancelled')") {
		t.Fatalf("drain script lost its lock or terminal set:\n%s", script)
	}
	for _, name := range []string{"00387_plan_cross_check_gated.sql", "00388_plan_cross_check_gated_validate.sql"} {
		body := readFile(t, filepath.Join("migrations", name))
		down := body[strings.Index(body, "-- +goose Down"):]
		if got := guardBlock(t, down); got != script {
			t.Errorf("%s Down guard differs from the drain script:\n--- migration\n%s\n--- script\n%s", name, got, script)
		}
		// The guard is the first statement: nothing mutates before it.
		before := down[:strings.Index(down, "DO $$")]
		for _, mutation := range []string{"ALTER ", "UPDATE ", "DROP ", "DELETE "} {
			if strings.Contains(before, mutation) {
				t.Errorf("%s: %q precedes the Down guard", name, mutation)
			}
		}
	}
}
