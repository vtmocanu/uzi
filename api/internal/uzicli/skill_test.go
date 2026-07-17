package uzicli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func embeddedHash() string {
	sum := sha256.Sum256([]byte(EmbeddedSkill()))
	return hex.EncodeToString(sum[:])
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// A fresh install writes SKILL.md (== embedded) plus the state sidecar, under
// home/.claude/skills/uzi-cli/, and reports no backup.
func TestSkillInstallFresh(t *testing.T) {
	home := t.TempDir()
	si := NewSkillInstallerAt(home, "v9.9.9")

	res, err := si.Install(false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Wrote || res.BackedUp || res.AlreadyCurrent {
		t.Fatalf("fresh install result = %+v, want Wrote only", res)
	}
	want := filepath.Join(home, ".claude", "skills", "uzi-cli", "SKILL.md")
	if res.Path != want {
		t.Errorf("install path = %q, want %q", res.Path, want)
	}
	if got := read(t, want); got != EmbeddedSkill() {
		t.Errorf("installed SKILL.md != embedded")
	}
	// State sidecar records the embedded hash.
	state := read(t, filepath.Join(home, ".claude", "skills", "uzi-cli", ".uzi-cli-state.json"))
	if !strings.Contains(state, embeddedHash()) {
		t.Errorf("state sidecar %q missing embedded sha %s", state, embeddedHash())
	}
}

// A second install with nothing changed is a no-op (AlreadyCurrent), no rewrite.
func TestSkillInstallIdempotent(t *testing.T) {
	home := t.TempDir()
	si := NewSkillInstallerAt(home, "v1")
	if _, err := si.Install(false); err != nil {
		t.Fatal(err)
	}
	res, err := si.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyCurrent || res.Wrote || res.BackedUp {
		t.Fatalf("second install = %+v, want AlreadyCurrent only", res)
	}
}

// Editing the installed SKILL.md then re-installing preserves the edit at
// SKILL.md.bak, rewrites the bundled skill, and signals BackedUp (the "warn once"
// trigger). This is the core of Success Criterion 5.
func TestSkillInstallPreservesUserEdit(t *testing.T) {
	home := t.TempDir()
	si := NewSkillInstallerAt(home, "v1")
	if _, err := si.Install(false); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(home, ".claude", "skills", "uzi-cli", "SKILL.md")
	bakPath := skillPath + ".bak"

	const edit = "# my local edits\nkeep this\n"
	if err := os.WriteFile(skillPath, []byte(edit), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := si.Install(false)
	if err != nil {
		t.Fatalf("install after edit: %v", err)
	}
	if !res.BackedUp || !res.Wrote {
		t.Fatalf("edit install = %+v, want BackedUp && Wrote", res)
	}
	if got := read(t, bakPath); got != edit {
		t.Errorf(".bak = %q, want the user's edit %q", got, edit)
	}
	if got := read(t, skillPath); got != EmbeddedSkill() {
		t.Errorf("SKILL.md was not restored to the bundled skill")
	}
	// A subsequent install is clean again (the warn-once property): file now matches.
	res2, err := si.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	if res2.BackedUp || !res2.AlreadyCurrent {
		t.Fatalf("post-restore install = %+v, want AlreadyCurrent, no backup", res2)
	}
}

// A stale on-disk skill (old content, with a sidecar recording that same old
// content) is rewritten to the embedded skill WITHOUT a .bak. The sidecar hash
// matches the on-disk hash, so the file is recognised as our own prior write —
// stale, not a user edit — and is refreshed cleanly with no backup.
func TestSkillInstallRewritesStaleContent(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "uzi-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale skill from an older binary, with a matching stale sidecar
	// (so it is not seen as a user edit — just stale content to refresh).
	stale := "old skill body\n"
	staleHash := sha256Hex([]byte(stale))
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".uzi-cli-state.json"),
		[]byte(`{"cli_version":"v0","skill_sha256":"`+staleHash+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	si := NewSkillInstallerAt(home, "v1")
	res, err := si.Install(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote || res.BackedUp {
		t.Fatalf("stale rewrite = %+v, want Wrote and NO backup (sidecar matched)", res)
	}
	if got := read(t, filepath.Join(dir, "SKILL.md")); got != EmbeddedSkill() {
		t.Errorf("stale skill not refreshed to embedded")
	}
}

// --force rewrites even an up-to-date install.
func TestSkillInstallForce(t *testing.T) {
	home := t.TempDir()
	si := NewSkillInstallerAt(home, "v1")
	if _, err := si.Install(false); err != nil {
		t.Fatal(err)
	}
	res, err := si.Install(true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Wrote {
		t.Fatalf("force install = %+v, want Wrote", res)
	}
}

// Status reflects installed / up-to-date / user-edited without writing.
func TestSkillStatus(t *testing.T) {
	home := t.TempDir()
	si := NewSkillInstallerAt(home, "v1")

	st := si.Status()
	if st.Installed {
		t.Fatalf("status before install = %+v, want not installed", st)
	}
	if _, err := si.Install(false); err != nil {
		t.Fatal(err)
	}
	st = si.Status()
	if !st.Installed || !st.UpToDate || st.UserEdited {
		t.Fatalf("status after install = %+v, want installed && up-to-date && !edited", st)
	}
	// Status must not have written anything (idempotent read).
	if err := os.WriteFile(si.SkillPath(), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = si.Status()
	if st.UpToDate || !st.UserEdited {
		t.Fatalf("status after edit = %+v, want !up-to-date && edited", st)
	}
}

// The install target is confined to ~/.claude/skills/uzi-cli/ and never touches
// ~/.claude/commands/ — the write path is a compile-time constant with no
// user-supplied component (PRD #64).
func TestSkillInstallDoesNotTouchCommands(t *testing.T) {
	home := t.TempDir()
	commands := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(commands, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(commands, "sentinel.md")
	if err := os.WriteFile(marker, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	si := NewSkillInstallerAt(home, "v1")
	if _, err := si.Install(true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, marker); got != "do not touch" {
		t.Errorf("install disturbed ~/.claude/commands/: %q", got)
	}
	entries, err := os.ReadDir(commands)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("commands/ has %d entries, want 1 (install must not add files there)", len(entries))
	}
}
