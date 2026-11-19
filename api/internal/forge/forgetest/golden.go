package forgetest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UpdateGoldenEnv, when set to 1, makes AssertGolden rewrite the golden file
// instead of comparing against it.
const UpdateGoldenEnv = "UZI_UPDATE_GOLDEN"

// AssertGolden compares lines (one recorded forge call per line) with
// testdata/<name> in the calling test's package directory. With
// UZI_UPDATE_GOLDEN=1 it writes the file instead. A missing golden file fails
// the test and names the variable, so a new golden is never created silently.
func AssertGolden(t testing.TB, name string, lines []string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	got := strings.Join(lines, "\n")
	if len(lines) > 0 {
		got += "\n"
	}
	if os.Getenv(UpdateGoldenEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("golden %s: mkdir: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("golden %s: write: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under the calling test package testdata dir
	if err != nil {
		t.Fatalf("golden %s unreadable (%v); generate it with %s=1", path, err, UpdateGoldenEnv)
	}
	if string(want) != got {
		t.Fatalf("forge calls differ from golden %s (regenerate only if the change is intended, with %s=1)\n--- want\n%s--- got\n%s", path, UpdateGoldenEnv, want, got)
	}
}
