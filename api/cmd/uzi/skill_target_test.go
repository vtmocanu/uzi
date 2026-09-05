package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// codexSkillPath is the fixed Codex user-skill copy under a home dir.
func codexSkillPath(home string) string {
	return filepath.Join(home, ".agents", "skills", "uzi-cli", "SKILL.md")
}

// pathExists reports whether a path is present on disk.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// `skill install --target codex` with no Codex home selects Codex and CREATES the
// fixed `.agents/skills/uzi-cli` copy, without touching the Claude tree.
func TestSkillInstallTargetCodexCreatesCopy(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := os.ReadFile(codexSkillPath(home))
	if err != nil || string(got) != uzicli.EmbeddedSkill() {
		t.Fatalf("--target codex did not write the Codex SKILL.md (err=%v)", err)
	}
	if pathExists(installedSkillPath(home)) {
		t.Errorf("--target codex must not write the Claude copy")
	}
	if !strings.Contains(out, "codex") || !strings.Contains(out, "installed") {
		t.Errorf("output = %q, want a codex install line", out)
	}
}

// `skill status --target codex` and `skill uninstall-hook --target codex` create
// NOTHING when no Codex home exists: status is read-only, and uninstall-hook over a
// missing hooks.json is a clean no-op (never writes, never MkdirAll).
func TestSkillCodexReadOnlyVerbsCreateNothing(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home

	if _, _, code := runCLI(t, env, "skill", "status", "--target", "codex"); code != uzicli.ExitOK {
		t.Fatalf("status --target codex exit = %d, want 0", code)
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("status must not create the Codex skill copy")
	}

	_, _, code := runCLI(t, env, "skill", "uninstall-hook", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("uninstall-hook --target codex exit = %d, want 0 (no-op over a missing hooks.json)", code)
	}
	if pathExists(filepath.Join(home, ".codex")) || pathExists(filepath.Join(home, ".agents")) {
		t.Errorf("a read-only/no-op Codex verb must create no directories")
	}
}

// codexHooksFilePath is the Codex hooks.json under the default ~/.codex config home.
func codexHooksFilePath(home string) string {
	return filepath.Join(home, ".codex", "hooks.json")
}

// `skill install-hook --target codex` writes ~/.codex/hooks.json with the codex
// command and matcher, and prints the /hooks stderr guidance.
func TestSkillInstallHookTargetCodex(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home

	out, errb, code := runCLI(t, env, "skill", "install-hook", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	b, err := os.ReadFile(codexHooksFilePath(home))
	if err != nil {
		t.Fatalf("hooks.json not written: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "uzi skill install --target codex") {
		t.Errorf("hooks.json missing the codex command:\n%s", s)
	}
	if !strings.Contains(s, "startup|resume") {
		t.Errorf("hooks.json missing the codex matcher:\n%s", s)
	}
	if !strings.Contains(out, "codex") {
		t.Errorf("stdout = %q, want a codex line", out)
	}
	if !strings.Contains(errb, "/hooks") {
		t.Errorf("stderr = %q, want the /hooks trust guidance", errb)
	}
	// uzi never writes Codex config or trust state.
	if pathExists(filepath.Join(home, ".codex", "config.toml")) {
		t.Errorf("install-hook must never create config.toml")
	}
}

// `skill status --target all` shows hook rows for BOTH targets.
func TestSkillStatusTargetAllShowsHookRows(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "status", "--target", "all")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Count(out, "HOOK_INSTALLED") != 2 {
		t.Errorf("status --target all should show HOOK_INSTALLED for both targets:\n%s", out)
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, "codex") {
		t.Errorf("status --target all should name both targets:\n%s", out)
	}
}

// `skill install --target all` installs BOTH targets even when Codex is not detected.
func TestSkillInstallTargetAllInstallsBoth(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "all"); code != uzicli.ExitOK {
		t.Fatalf("install --target all exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) || !pathExists(codexSkillPath(home)) {
		t.Errorf("--target all must install both the Claude and Codex copies")
	}
}

// A relative CODEX_HOME (injected through the Env.Getenv seam, never a real exported
// var) is a usage error when Codex is explicitly selected, and silently skipped on
// the automatic path.
func TestSkillTargetCodexRelativeHomeIsUsageError(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	env.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return "relcodex"
		}
		return ""
	}
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "codex"); code != uzicli.ExitUsage {
		t.Fatalf("install --target codex with relative CODEX_HOME exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "all"); code != uzicli.ExitUsage {
		t.Fatalf("install --target all with relative CODEX_HOME exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("a rejected relative CODEX_HOME must not write the Codex copy")
	}

	// Auto path: skip Codex silently, still install Claude.
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	autoEnv := skillEnv(fc, home)
	autoEnv.Getenv = env.Getenv
	if _, _, code := runCLI(t, autoEnv, "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("auto path with relative CODEX_HOME exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must still install the Claude copy")
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must skip a relative-CODEX_HOME Codex target")
	}
}

// An invalid --target value is a usage error on every verb.
func TestSkillInvalidTarget(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	for _, verb := range [][]string{
		{"skill", "status"}, {"skill", "install"},
		{"skill", "install-hook"}, {"skill", "uninstall-hook"},
	} {
		args := append(append([]string{}, verb...), "--target", "bogus")
		if _, _, code := runCLI(t, env, args...); code != uzicli.ExitUsage {
			t.Errorf("%v --target bogus exit = %d, want %d", verb, code, uzicli.ExitUsage)
		}
	}
}

// The automatic path installs the Codex copy too when a Codex config home already
// exists (here the default ~/.codex under the temp home), while keeping the seam
// deterministic (no real CODEX_HOME consulted).
func TestAutoUpgradeInstallsCodexWhenDetected(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must install the Claude copy")
	}
	if !pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must install the Codex copy when ~/.codex exists")
	}
}

// The automatic path does NOT install Codex when no Codex home exists (it must not
// litter a machine that does not run Codex).
func TestAutoUpgradeSkipsUndetectedCodex(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must install the Claude copy")
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must not create a Codex copy with no Codex home")
	}
}
