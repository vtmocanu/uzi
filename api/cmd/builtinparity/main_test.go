package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRun(t *testing.T) {
	up := writeRoles(t, map[string]string{
		"coder.md":    "coder body\n",
		"reviewer.md": "reviewer body\n",
		"tui-ux.md":   "not a builtin\n",
		"README.md":   "ignored\n",
	})

	cases := []struct {
		name     string
		builtins map[string]string
		wantCode int
		want     []string
		notWant  []string
	}{
		{
			name:     "in sync, lead exempt, extra upstream roles ignored",
			builtins: map[string]string{"coder.md": "coder body\n", "reviewer.md": "reviewer body\n", "lead.md": "uzi only\n"},
			want:     []string{"builtins match upstream (2 roles"},
			notWant:  []string{"DIFFERS", "MISSING", " lead:"},
		},
		{
			name:     "a body drift is reported, still exit 0",
			builtins: map[string]string{"coder.md": "coder body\nlocal edit\n", "reviewer.md": "reviewer body\n"},
			want:     []string{"DIFFERS  coder: first difference at line 2", "1 of 2 builtins drift"},
		},
		{
			name:     "a builtin with no upstream file is reported",
			builtins: map[string]string{"coder.md": "coder body\n", "auditor.md": "a\n"},
			want:     []string{"MISSING  auditor"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run([]string{"-upstream", up, "-builtins", writeRoles(t, tc.builtins)})
			if res.code != tc.wantCode {
				t.Fatalf("code = %d, want %d (stderr %q)", res.code, tc.wantCode, res.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.stdout, w) {
					t.Errorf("stdout lacks %q:\n%s", w, res.stdout)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(res.stdout, w) {
					t.Errorf("stdout unexpectedly has %q:\n%s", w, res.stdout)
				}
			}
		})
	}
}

// A broken instrument must not read as parity: exit 2, never 0.
func TestRunInstrumentFailures(t *testing.T) {
	good := writeRoles(t, map[string]string{"coder.md": "x\n"})
	for name, args := range map[string][]string{
		"no upstream flag":        {"-builtins", good},
		"upstream dir missing":    {"-upstream", filepath.Join(good, "nope"), "-builtins", good},
		"upstream has no roles":   {"-upstream", t.TempDir(), "-builtins", good},
		"builtins dir unreadable": {"-upstream", good, "-builtins", filepath.Join(good, "nope")},
	} {
		t.Run(name, func(t *testing.T) {
			if res := run(args); res.code != 2 {
				t.Errorf("code = %d, want 2 (stdout %q)", res.code, res.stdout)
			}
		})
	}
}
