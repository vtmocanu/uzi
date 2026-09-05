package uzicli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// noEnv is a deterministic env-lookup that always returns "" — the shape a test
// gets when it never sets CODEX_HOME. It NEVER consults the real process
// environment, which is the isolation the PRD requires: a developer's exported
// CODEX_HOME must not change what these tests resolve.
func noEnv(string) string { return "" }

// findTarget returns the resolved target with the given name.
func findTarget(t *testing.T, rts []ResolvedTarget, name string) ResolvedTarget {
	t.Helper()
	for _, rt := range rts {
		if rt.Target.Name == name {
			return rt
		}
	}
	t.Fatalf("no %q target in resolution", name)
	return ResolvedTarget{}
}

// Claude is always detected with fixed skill/hook paths; Codex, with no CODEX_HOME
// and no ~/.codex dir, is NOT detected but carries no selection error.
func TestResolveSkillTargetsNoCodexHome(t *testing.T) {
	home := t.TempDir()
	rts := ResolveSkillTargets(home, noEnv)

	claude := findTarget(t, rts, "claude")
	if !claude.Detected || claude.SelectErr != nil {
		t.Errorf("claude = %+v, want detected and no selectErr", claude)
	}
	if want := filepath.Join(home, ".claude", "skills", "uzi-cli"); claude.Target.SkillDir != want {
		t.Errorf("claude skill dir = %q, want %q", claude.Target.SkillDir, want)
	}
	if want := filepath.Join(home, ".claude", "settings.json"); claude.Target.HookPath != want {
		t.Errorf("claude hook path = %q, want %q", claude.Target.HookPath, want)
	}

	codex := findTarget(t, rts, "codex")
	if codex.Detected {
		t.Errorf("codex must not be detected with no CODEX_HOME and no ~/.codex: %+v", codex)
	}
	if codex.SelectErr != nil {
		t.Errorf("codex selectErr = %v, want nil (absent is not an error)", codex.SelectErr)
	}
	// The Codex skill tail is fixed under $HOME, independent of CODEX_HOME.
	if want := filepath.Join(home, ".agents", "skills", "uzi-cli"); codex.Target.SkillDir != want {
		t.Errorf("codex skill dir = %q, want %q", codex.Target.SkillDir, want)
	}
	if want := filepath.Join(home, ".codex", "hooks.json"); codex.Target.HookPath != want {
		t.Errorf("codex hook path = %q, want the default ~/.codex/hooks.json %q", codex.Target.HookPath, want)
	}
}

// An existing default ~/.codex dir (empty CODEX_HOME) makes Codex auto-detected.
func TestResolveSkillTargetsDefaultCodexHomeExists(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	codex := findTarget(t, ResolveSkillTargets(home, noEnv), "codex")
	if !codex.Detected {
		t.Errorf("codex must be detected when ~/.codex exists: %+v", codex)
	}
}

// An absolute CODEX_HOME pointing at an existing dir detects Codex; the SKILL still
// resolves under $HOME/.agents/skills/uzi-cli (NOT under CODEX_HOME) and only the
// hook path follows CODEX_HOME. The env is supplied through the getenv seam, never a
// real exported variable.
func TestResolveSkillTargetsAbsoluteCodexHome(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(t.TempDir(), "custom-codex")
	if err := os.MkdirAll(codexHome, 0o750); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}
		return ""
	}
	codex := findTarget(t, ResolveSkillTargets(home, getenv), "codex")
	if !codex.Detected || codex.SelectErr != nil {
		t.Errorf("codex = %+v, want detected and no selectErr", codex)
	}
	if want := filepath.Join(home, ".agents", "skills", "uzi-cli"); codex.Target.SkillDir != want {
		t.Errorf("codex skill dir = %q, want it under $HOME (%q), never under CODEX_HOME", codex.Target.SkillDir, want)
	}
	if want := filepath.Join(codexHome, "hooks.json"); codex.Target.HookPath != want {
		t.Errorf("codex hook path = %q, want it under CODEX_HOME %q", codex.Target.HookPath, want)
	}
}

// An absolute CODEX_HOME that does NOT exist: valid, not an error, but not detected.
func TestResolveSkillTargetsAbsoluteCodexHomeMissing(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(t.TempDir(), "does-not-exist")
	getenv := func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}
		return ""
	}
	codex := findTarget(t, ResolveSkillTargets(home, getenv), "codex")
	if codex.Detected {
		t.Errorf("a non-existent absolute CODEX_HOME must not be detected: %+v", codex)
	}
	if codex.SelectErr != nil {
		t.Errorf("a non-existent absolute CODEX_HOME is not a selection error: %v", codex.SelectErr)
	}
	if want := filepath.Join(codexHome, "hooks.json"); codex.Target.HookPath != want {
		t.Errorf("codex hook path = %q, want %q", codex.Target.HookPath, want)
	}
}

// A relative CODEX_HOME is a usage error on explicit selection and is not detected.
func TestResolveSkillTargetsRelativeCodexHome(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "CODEX_HOME" {
			return "relcodex"
		}
		return ""
	}
	codex := findTarget(t, ResolveSkillTargets(home, getenv), "codex")
	if codex.Detected {
		t.Errorf("a relative CODEX_HOME must never be detected: %+v", codex)
	}
	if codex.SelectErr == nil {
		t.Fatalf("a relative CODEX_HOME must yield a selection error")
	}
	var ee *ExitError
	if !errors.As(codex.SelectErr, &ee) || ee.Code != ExitUsage {
		t.Errorf("selectErr = %v, want an *ExitError with ExitUsage", codex.SelectErr)
	}
}

// Independent state per target: a user-edited Claude copy and a user-edited Codex
// copy each get their own .bak on reinstall, and neither install writes into the
// other's directory.
func TestPerTargetInstallIsolation(t *testing.T) {
	home := t.TempDir()
	rts := ResolveSkillTargets(home, noEnv)
	claude := NewSkillInstallerForTarget(findTarget(t, rts, "claude").Target, "v1")
	codex := NewSkillInstallerForTarget(findTarget(t, rts, "codex").Target, "v1")

	if _, err := claude.Install(false); err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Install(false); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(home, ".claude", "skills", "uzi-cli")
	codexDir := filepath.Join(home, ".agents", "skills", "uzi-cli")
	if read(t, filepath.Join(claudeDir, "SKILL.md")) != EmbeddedSkill() {
		t.Errorf("claude SKILL.md not installed")
	}
	if read(t, filepath.Join(codexDir, "SKILL.md")) != EmbeddedSkill() {
		t.Errorf("codex SKILL.md not installed")
	}

	// Edit each copy distinctly, reinstall, and assert each .bak holds ITS OWN edit.
	const claudeEdit = "# claude local edit\n"
	const codexEdit = "# codex local edit\n"
	if err := os.WriteFile(filepath.Join(claudeDir, "SKILL.md"), []byte(claudeEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "SKILL.md"), []byte(codexEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	cres, err := claude.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	xres, err := codex.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	if !cres.BackedUp || !xres.BackedUp {
		t.Fatalf("both edits should back up: claude=%+v codex=%+v", cres, xres)
	}
	if got := read(t, filepath.Join(claudeDir, "SKILL.md.bak")); got != claudeEdit {
		t.Errorf("claude .bak = %q, want the claude edit %q", got, claudeEdit)
	}
	if got := read(t, filepath.Join(codexDir, "SKILL.md.bak")); got != codexEdit {
		t.Errorf("codex .bak = %q, want the codex edit %q (cross-contamination)", got, codexEdit)
	}
}
