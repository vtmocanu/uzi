package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func skillEnv(fc uzicli.Client, home string) Env {
	env := fakeEnv(fc)
	env.SkillHome = home
	env.AutoUpgradeSkill = true
	return env
}

func installedSkillPath(home string) string {
	return filepath.Join(home, ".claude", "skills", "uzi-cli", "SKILL.md")
}

func settingsFilePath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// hookCommandCount reads settings.json and counts SessionStart hook commands
// equal to the canonical `uzi skill install`.
func hookCommandCount(t *testing.T, home string) int {
	t.Helper()
	b, err := os.ReadFile(settingsFilePath(home))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings.json not valid JSON: %v", err)
	}
	hooks, _ := m["hooks"].(map[string]any)
	arr, _ := hooks["SessionStart"].([]any)
	n := 0
	for _, mo := range arr {
		mm, ok := mo.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := mm["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); cmd == "uzi skill install" {
				n++
			}
		}
	}
	return n
}

// `uzi skill install-hook` writes the SessionStart hook and exits 0.
func TestSkillInstallHookCommand(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install-hook")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := hookCommandCount(t, home); got != 1 {
		t.Fatalf("want 1 hook command, got %d", got)
	}
	if !strings.Contains(out, "hook") {
		t.Errorf("install-hook output = %q, want mention of the hook", out)
	}
}

// `uzi skill install-hook` twice is idempotent — still exactly one entry.
func TestSkillInstallHookIdempotent(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	if _, _, code := runCLI(t, env, "skill", "install-hook"); code != uzicli.ExitOK {
		t.Fatal("first install-hook failed")
	}
	out, _, code := runCLI(t, env, "skill", "install-hook")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := hookCommandCount(t, home); got != 1 {
		t.Fatalf("re-run created a duplicate: %d hook commands", got)
	}
	if !strings.Contains(out, "already present") {
		t.Errorf("second install-hook = %q, want 'already present'", out)
	}
}

// `uzi skill status --json` carries the hook fields, before and after install.
func TestSkillStatusJSONIncludesHook(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home

	out, _, code := runCLI(t, env, "skill", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("status exit = %d, want 0", code)
	}
	var before struct {
		Skill map[string]any `json:"skill"`
		Hook  struct {
			Installed bool `json:"installed"`
		} `json:"hook"`
	}
	if err := json.Unmarshal([]byte(out), &before); err != nil {
		t.Fatalf("status --json not decodable: %v\n%s", err, out)
	}
	if before.Skill == nil {
		t.Errorf("status --json missing the skill object:\n%s", out)
	}
	if before.Hook.Installed {
		t.Errorf("hook should not be installed yet:\n%s", out)
	}

	if _, _, code := runCLI(t, env, "skill", "install-hook"); code != uzicli.ExitOK {
		t.Fatal("install-hook failed")
	}
	out, _, code = runCLI(t, env, "skill", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("status exit = %d, want 0", code)
	}
	var after struct {
		Hook struct {
			Installed bool `json:"installed"`
			Current   bool `json:"current"`
		} `json:"hook"`
	}
	if err := json.Unmarshal([]byte(out), &after); err != nil {
		t.Fatalf("status --json not decodable: %v\n%s", err, out)
	}
	if !after.Hook.Installed || !after.Hook.Current {
		t.Errorf("hook status after install-hook = %+v, want installed+current\n%s", after.Hook, out)
	}
}

// `uzi skill uninstall-hook` removes the hook and exits 0.
func TestSkillUninstallHookCommand(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	if _, _, code := runCLI(t, env, "skill", "install-hook"); code != uzicli.ExitOK {
		t.Fatal("install-hook failed")
	}
	out, _, code := runCLI(t, env, "skill", "uninstall-hook")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := hookCommandCount(t, home); got != 0 {
		t.Fatalf("uninstall-hook left %d hook commands", got)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("uninstall-hook output = %q, want 'removed'", out)
	}
}

// `uzi skill install-hook` against a malformed settings.json fails non-zero and
// leaves the file unchanged.
func TestSkillInstallHookMalformedFails(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const bad = "{not json"
	if err := os.WriteFile(settingsFilePath(home), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	_, _, code := runCLI(t, env, "skill", "install-hook")
	if code == uzicli.ExitOK {
		t.Fatalf("exit = %d, want non-zero on malformed settings.json", code)
	}
	if got, _ := os.ReadFile(settingsFilePath(home)); string(got) != bad {
		t.Fatalf("settings.json changed on the abort path: %q", got)
	}
}

// Explicit `uzi skill install` writes the bundled skill and exits 0.
func TestSkillInstallCommand(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got, err := os.ReadFile(installedSkillPath(home)); err != nil || string(got) != uzicli.EmbeddedSkill() {
		t.Fatalf("skill install did not write the bundled SKILL.md (err=%v)", err)
	}
	if !strings.Contains(out, "installed") {
		t.Errorf("skill install output = %q, want 'installed'", out)
	}
}

// `uzi skill install --json` emits the structured result; --force rewrites.
func TestSkillInstallForceJSON(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	if _, _, code := runCLI(t, env, "skill", "install"); code != uzicli.ExitOK {
		t.Fatal("first install failed")
	}
	out, _, code := runCLI(t, env, "skill", "install", "--force", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"wrote": true`) {
		t.Errorf("--force --json = %q, want wrote:true", out)
	}
}

// The auto-upgrade hook installs the skill on any normal (non-skill) command.
func TestAutoUpgradeInstallsOnCommand(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(installedSkillPath(home)); err != nil {
		t.Fatalf("auto-upgrade did not install the skill: %v", err)
	}
}

// Success Criterion 5: editing an installed SKILL.md, then running any uzi
// command, preserves the edit at .bak, reinstalls the bundled skill, warns on
// stderr, and the command still exits 0.
func TestAutoUpgradePreservesEditExit0(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	env := skillEnv(fc, home)

	// First run installs the skill and records its state.
	if _, _, code := runCLI(t, env, "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("first run exit = %d", code)
	}
	// User edits the installed skill.
	const edit = "# hand edits I want to keep\n"
	if err := os.WriteFile(installedSkillPath(home), []byte(edit), 0o644); err != nil {
		t.Fatal(err)
	}
	// Any subsequent command triggers the rescue; it must still exit 0.
	out, errb, code := runCLI(t, env, "whoami", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (never fatal)", code)
	}
	if !strings.Contains(out, "u1") {
		t.Errorf("the real command did not run:\n%s", out)
	}
	bak := installedSkillPath(home) + ".bak"
	if got, err := os.ReadFile(bak); err != nil || string(got) != edit {
		t.Fatalf("edit not preserved at %s: got %q err %v", bak, got, err)
	}
	if got, _ := os.ReadFile(installedSkillPath(home)); string(got) != uzicli.EmbeddedSkill() {
		t.Errorf("bundled skill not reinstalled over the edit")
	}
	if !strings.Contains(errb, "preserved") {
		t.Errorf("expected a stderr warning about the preserved edit, got:\n%s", errb)
	}
}

// UZI_SKILL_AUTO_UPGRADE=0 disables the auto path entirely.
func TestAutoUpgradeDisabledByEnv(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "0")
	home := t.TempDir()
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(installedSkillPath(home)); !os.IsNotExist(err) {
		t.Errorf("UZI_SKILL_AUTO_UPGRADE=0 must skip install, but SKILL.md exists (err=%v)", err)
	}
}

// A skill-install error (here: a home path that is a regular file, so MkdirAll
// fails) must NOT break the command — it warns on stderr and still exits 0. This
// is the read-only-$HOME-in-CI guarantee.
func TestAutoUpgradeNeverFatalOnError(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	notADir := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	out, errb, code := runCLI(t, skillEnv(fc, notADir), "whoami", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("a skill-install error must not break the command: exit = %d", code)
	}
	if !strings.Contains(out, "u1") {
		t.Errorf("the real command did not run:\n%s", out)
	}
	if !strings.Contains(errb, "auto-upgrade skipped") {
		t.Errorf("expected a stderr warning about the skipped upgrade:\n%s", errb)
	}
}
