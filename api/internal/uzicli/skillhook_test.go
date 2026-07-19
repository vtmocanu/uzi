package uzicli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsPathIn(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

func backupPathIn(home string) string {
	return filepath.Join(home, ".claude", "settings.json.bak")
}

// writeSettings writes home/.claude/settings.json with the given raw bytes,
// creating the dir. It is the fixture seam for the tests below.
func writeSettings(t *testing.T, home, raw string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPathIn(home), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parseSettings reads + parses settings.json for assertions.
func parseSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(settingsPathIn(home))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings.json is not valid JSON after write: %v\n%s", err, b)
	}
	return m
}

// sessionStartCommands walks the SessionStart array and returns every hook
// command string it finds (across all matcher-objects).
func sessionStartCommands(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	hooks, _ := m["hooks"].(map[string]any)
	arr, _ := hooks["SessionStart"].([]any)
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
			if cmd, ok := hm["command"].(string); ok {
				out = append(out, cmd)
			}
		}
	}
	return out
}

func countOurCommands(cmds []string) int {
	n := 0
	for _, c := range cmds {
		if c == hookCommand {
			n++
		}
	}
	return n
}

// InstallHook creates a fresh settings.json when none exists, and takes no .bak.
func TestInstallHookMissingFile(t *testing.T) {
	home := t.TempDir()
	hm := NewHookManagerAt(home)

	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if !res.Changed || res.AlreadyPresent || res.BackedUp {
		t.Fatalf("missing-file install: got %+v, want Changed only", res)
	}
	if _, err := os.Stat(backupPathIn(home)); !os.IsNotExist(err) {
		t.Fatalf("no .bak expected when the file was created fresh (err=%v)", err)
	}
	m := parseSettings(t, home)
	if got := countOurCommands(sessionStartCommands(t, m)); got != 1 {
		t.Fatalf("want exactly 1 of our commands, got %d", got)
	}
}

// InstallHook on an empty object adds the entry and backs the prior file up.
func TestInstallHookEmptyObjectBacksUp(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, "{}\n")
	hm := NewHookManagerAt(home)

	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if !res.Changed || !res.BackedUp {
		t.Fatalf("empty-object install: got %+v, want Changed+BackedUp", res)
	}
	// The backup must be the ORIGINAL bytes, written before the mutation.
	if got, err := os.ReadFile(backupPathIn(home)); err != nil || string(got) != "{}\n" {
		t.Fatalf(".bak content = %q err %v, want original %q", got, err, "{}\n")
	}
	if got := countOurCommands(sessionStartCommands(t, home2map(t, home))); got != 1 {
		t.Fatalf("want 1 of our commands, got %d", got)
	}
}

func home2map(t *testing.T, home string) map[string]any { return parseSettings(t, home) }

// InstallHook must preserve an unrelated top-level key AND a foreign
// SessionStart hook already present.
func TestInstallHookPreservesForeignHookAndSiblings(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{
	  "model": "opus",
	  "hooks": {
	    "SessionStart": [
	      {"matcher": "startup", "hooks": [{"type": "command", "command": "dot-ai refresh", "timeout": 5}]}
	    ],
	    "PreToolUse": [
	      {"matcher": "*", "hooks": [{"type": "command", "command": "echo hi"}]}
	    ]
	  }
	}`)
	hm := NewHookManagerAt(home)

	if _, err := hm.InstallHook(); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	m := parseSettings(t, home)

	if m["model"] != "opus" {
		t.Errorf("unrelated top-level key `model` lost: %+v", m["model"])
	}
	cmds := sessionStartCommands(t, m)
	var sawForeign, sawOurs bool
	for _, c := range cmds {
		if c == "dot-ai refresh" {
			sawForeign = true
		}
		if c == hookCommand {
			sawOurs = true
		}
	}
	if !sawForeign {
		t.Errorf("foreign SessionStart hook `dot-ai refresh` lost; commands=%v", cmds)
	}
	if !sawOurs {
		t.Errorf("our hook was not added; commands=%v", cmds)
	}
	// Sibling event PreToolUse must survive.
	hooks, _ := m["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Errorf("sibling event PreToolUse lost: %+v", hooks)
	}
}

// Re-running InstallHook is idempotent: AlreadyPresent, no second write.
func TestInstallHookIdempotent(t *testing.T) {
	home := t.TempDir()
	hm := NewHookManagerAt(home)

	if _, err := hm.InstallHook(); err != nil {
		t.Fatalf("first InstallHook: %v", err)
	}
	// Remove any .bak so we can detect a spurious second write cleanly.
	_ = os.Remove(backupPathIn(home))

	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("second InstallHook: %v", err)
	}
	if !res.AlreadyPresent || res.Changed {
		t.Fatalf("re-install: got %+v, want AlreadyPresent and no Changed", res)
	}
	if _, err := os.Stat(backupPathIn(home)); !os.IsNotExist(err) {
		t.Fatalf("second install must not write a .bak (err=%v)", err)
	}
	if got := countOurCommands(sessionStartCommands(t, parseSettings(t, home))); got != 1 {
		t.Fatalf("re-install created a duplicate: %d of our commands", got)
	}
}

// A malformed settings.json aborts: error, file unchanged, no .bak.
func TestInstallHookMalformedAborts(t *testing.T) {
	home := t.TempDir()
	const bad = "{not json"
	writeSettings(t, home, bad)
	hm := NewHookManagerAt(home)

	if _, err := hm.InstallHook(); err == nil {
		t.Fatal("InstallHook must error on malformed settings.json")
	}
	if got, err := os.ReadFile(settingsPathIn(home)); err != nil || string(got) != bad {
		t.Fatalf("settings.json was modified: got %q err %v", got, err)
	}
	if _, err := os.Stat(backupPathIn(home)); !os.IsNotExist(err) {
		t.Fatalf("no .bak may be created on the abort path (err=%v)", err)
	}
}

// UninstallHook removes only our entry, leaves siblings + foreign hook, prunes
// empty containers, and counts what it removed.
func TestUninstallHookRemovesOnlyOurs(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{
	  "model": "opus",
	  "hooks": {
	    "SessionStart": [
	      {"matcher": "startup", "hooks": [{"type": "command", "command": "dot-ai refresh"}]}
	    ]
	  }
	}`)
	hm := NewHookManagerAt(home)
	if _, err := hm.InstallHook(); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	res, err := hm.UninstallHook()
	if err != nil {
		t.Fatalf("UninstallHook: %v", err)
	}
	if !res.Changed || res.Removed != 1 {
		t.Fatalf("uninstall: got %+v, want Changed and Removed=1", res)
	}
	m := parseSettings(t, home)
	if m["model"] != "opus" {
		t.Errorf("unrelated key lost on uninstall")
	}
	cmds := sessionStartCommands(t, m)
	if countOurCommands(cmds) != 0 {
		t.Errorf("our hook not removed; commands=%v", cmds)
	}
	var sawForeign bool
	for _, c := range cmds {
		if c == "dot-ai refresh" {
			sawForeign = true
		}
	}
	if !sawForeign {
		t.Errorf("foreign hook lost on uninstall; commands=%v", cmds)
	}
}

// UninstallHook prunes SessionStart (and hooks) when our entry was the only one.
func TestUninstallHookPrunesEmptyContainers(t *testing.T) {
	home := t.TempDir()
	hm := NewHookManagerAt(home)
	if _, err := hm.InstallHook(); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if _, err := hm.UninstallHook(); err != nil {
		t.Fatalf("UninstallHook: %v", err)
	}
	m := parseSettings(t, home)
	if _, ok := m["hooks"]; ok {
		t.Errorf("empty `hooks` container should be pruned: %+v", m)
	}
}

// UninstallHook when nothing of ours is present is a no-op, no error.
func TestUninstallHookNoop(t *testing.T) {
	home := t.TempDir()
	hm := NewHookManagerAt(home)

	// Missing file.
	if res, err := hm.UninstallHook(); err != nil || res.Changed {
		t.Fatalf("missing-file uninstall: res=%+v err=%v", res, err)
	}
	// Present but only a foreign hook.
	writeSettings(t, home, `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"dot-ai refresh"}]}]}}`)
	if res, err := hm.UninstallHook(); err != nil || res.Changed || res.Removed != 0 {
		t.Fatalf("foreign-only uninstall: res=%+v err=%v", res, err)
	}
}

func TestHookStatus(t *testing.T) {
	t.Run("not installed (missing file)", func(t *testing.T) {
		home := t.TempDir()
		st := NewHookManagerAt(home).HookStatus()
		if st.Installed || st.Current || st.Malformed {
			t.Fatalf("missing file: %+v, want all false", st)
		}
		if st.Command != hookCommand {
			t.Errorf("Command = %q, want canonical %q", st.Command, hookCommand)
		}
	})

	t.Run("installed and current", func(t *testing.T) {
		home := t.TempDir()
		if _, err := NewHookManagerAt(home).InstallHook(); err != nil {
			t.Fatal(err)
		}
		st := NewHookManagerAt(home).HookStatus()
		if !st.Installed || !st.Current || st.Duplicates != 0 {
			t.Fatalf("installed: %+v, want Installed+Current, no dups", st)
		}
	})

	t.Run("duplicates", func(t *testing.T) {
		home := t.TempDir()
		writeSettings(t, home, `{"hooks":{"SessionStart":[
		  {"matcher":"startup","hooks":[{"type":"command","command":"uzi skill install"}]},
		  {"matcher":"startup","hooks":[{"type":"command","command":"uzi skill install --force"}]}
		]}}`)
		st := NewHookManagerAt(home).HookStatus()
		if !st.Installed || st.Duplicates < 1 {
			t.Fatalf("duplicates: %+v, want Installed and Duplicates>=1", st)
		}
		if !st.Current { // the exact canonical one is present
			t.Errorf("want Current=true when the exact command is one of the dups")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		home := t.TempDir()
		writeSettings(t, home, "{not json")
		st := NewHookManagerAt(home).HookStatus()
		if !st.Malformed || st.Installed {
			t.Fatalf("malformed: %+v, want Malformed and not Installed", st)
		}
	})

	t.Run("prefix-tolerant install detection", func(t *testing.T) {
		home := t.TempDir()
		// A user-tweaked command that still starts with our prefix counts as
		// installed, but not Current (not the exact canonical string).
		writeSettings(t, home, `{"hooks":{"SessionStart":[
		  {"matcher":"startup","hooks":[{"type":"command","command":"uzi skill install --json"}]}
		]}}`)
		st := NewHookManagerAt(home).HookStatus()
		if !st.Installed || st.Current {
			t.Fatalf("prefix match: %+v, want Installed and not Current", st)
		}
	})
}
