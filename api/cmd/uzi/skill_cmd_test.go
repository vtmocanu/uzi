package main

import (
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
