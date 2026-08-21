package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRepo lays out a minimal repo tree under t.TempDir() so run() can be
// exercised end to end against FIXTURE rosters (never the live ones). product
// and devteam are role stems; accepted is the raw TSV body.
func fixtureRepo(t *testing.T, product, devteam []string, accepted string) string {
	t.Helper()
	root := t.TempDir()
	writeRoster(t, filepath.Join(root, "api", "internal", "agenttmpl", "builtins"), product)
	writeRoster(t, filepath.Join(root, ".claude", "agents"), devteam)
	mkdir(t, filepath.Join(root, "scripts"))
	if err := os.WriteFile(filepath.Join(root, "scripts", "role-parity-accepted.tsv"), []byte(accepted), 0o644); err != nil {
		t.Fatalf("write accepted: %v", err)
	}
	return root
}

func writeRoster(t *testing.T, dir string, stems []string) {
	t.Helper()
	mkdir(t, dir)
	for _, s := range stems {
		if err := os.WriteFile(filepath.Join(dir, s+".md"), []byte("# "+s+"\n"), 0o644); err != nil {
			t.Fatalf("write roster file %s: %v", s, err)
		}
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// TestRunExitCodeDiscipline is the guard for the load-bearing invariant: the
// default path ALWAYS returns 0 even with an un-accepted divergence (nudge,
// never a gate), -strict opts into 1, and rosters that agree return 0.
func TestRunExitCodeDiscipline(t *testing.T) {
	// tui-ux is a dev-team-only divergence that is NOT accepted.
	root := fixtureRepo(t,
		[]string{"coder", "lead"},
		[]string{"coder", "tui-ux"},
		"product-only\tlead\tby design\n",
	)

	t.Run("default path exits 0 despite a divergence", func(t *testing.T) {
		res := run([]string{"-repo-root", root})
		if res.code != 0 {
			t.Fatalf("default run returned %d, want 0 (a nudge must never gate); stderr=%q", res.code, res.stderr)
		}
		if !strings.Contains(res.stdout, "tui-ux") {
			t.Errorf("expected the tui-ux divergence in stdout, got %q", res.stdout)
		}
	})

	t.Run("strict exits 1 on an un-accepted divergence", func(t *testing.T) {
		if res := run([]string{"-repo-root", root, "-strict"}); res.code != 1 {
			t.Fatalf("strict run returned %d, want 1", res.code)
		}
	})
}

func TestRunAgreeingRostersExitsZero(t *testing.T) {
	root := fixtureRepo(t,
		[]string{"coder", "lead"},
		[]string{"coder", "release"},
		"product-only\tlead\tby design\ndevteam-only\trelease\tdev-only\n",
	)
	res := run([]string{"-repo-root", root, "-strict"})
	if res.code != 0 {
		t.Fatalf("all-accepted run returned %d, want 0; stderr=%q", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "Nothing to nudge") {
		t.Errorf("expected the agree message, got %q", res.stdout)
	}
}

func TestRunOperationalErrorsExitTwo(t *testing.T) {
	t.Run("missing roster directory", func(t *testing.T) {
		if res := run([]string{"-repo-root", filepath.Join(t.TempDir(), "nope")}); res.code != 2 {
			t.Fatalf("missing repo returned %d, want 2", res.code)
		}
	})

	t.Run("unknown side in accepted TSV", func(t *testing.T) {
		root := fixtureRepo(t, []string{"coder"}, []string{"coder"}, "sideways\tcoder\tbad\n")
		if res := run([]string{"-repo-root", root}); res.code != 2 {
			t.Fatalf("bad-side TSV returned %d, want 2", res.code)
		}
	})
}

// TestRunTolerantTSVComments proves blank lines and comment lines (including
// indented ones) are ignored rather than parsed as data.
func TestRunTolerantTSVComments(t *testing.T) {
	accepted := "# header\n\n   # indented comment\nproduct-only\tlead\tby design\n"
	root := fixtureRepo(t, []string{"coder", "lead"}, []string{"coder"}, accepted)
	res := run([]string{"-repo-root", root, "-strict"})
	if res.code != 0 {
		t.Fatalf("run with indented TSV comment returned %d, want 0; stderr=%q", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "Nothing to nudge") {
		t.Errorf("lead should be suppressed, leaving no divergence; got %q", res.stdout)
	}
}
