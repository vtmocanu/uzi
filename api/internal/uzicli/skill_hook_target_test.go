package uzicli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// codexHooksPathIn is the Codex hooks.json under a config home dir.
func codexHooksPathIn(codexHome string) string {
	return filepath.Join(codexHome, "hooks.json")
}

// writeFileFixture writes raw bytes to path, creating the parent dir.
func writeFileFixture(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// decodeFile parses a JSON file for assertions.
func decodeFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test reads a test-controlled temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", path, err, b)
	}
	return m
}

// commandsFrom walks a parsed hook document and returns every SessionStart command.
func commandsFrom(t *testing.T, m map[string]any) []string {
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

// matchersFrom returns the matcher string of every SessionStart matcher-object.
func matchersFrom(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	hooks, _ := m["hooks"].(map[string]any)
	arr, _ := hooks["SessionStart"].([]any)
	for _, mo := range arr {
		mm, ok := mo.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := mm["matcher"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestInstallHookLegacyMigration: a settings.json holding the bare M1 legacy command
// is REWRITTEN in place to the canonical --target claude form (no duplicate), backs
// up the original byte-for-byte, and a second install is an idempotent no-op.
func TestInstallHookLegacyMigration(t *testing.T) {
	home := t.TempDir()
	const orig = `{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "uzi skill install", "timeout": 15}]}
    ]
  }
}
`
	writeSettings(t, home, orig)
	hm := NewHookManagerAt(home)

	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if !res.Changed || res.AlreadyPresent || !res.BackedUp {
		t.Fatalf("migration: got %+v, want Changed+BackedUp, not AlreadyPresent", res)
	}
	// The .bak is the ORIGINAL bytes, written before the mutation.
	bak, err := os.ReadFile(backupPathIn(home))
	if err != nil || string(bak) != orig {
		t.Fatalf(".bak = %q err %v, want byte-identical original", bak, err)
	}
	cmds := commandsFrom(t, parseSettings(t, home))
	if len(cmds) != 1 || cmds[0] != claudeCanonicalCommand {
		t.Fatalf("after migration commands=%v, want exactly [%q]", cmds, claudeCanonicalCommand)
	}

	// Second install is now idempotent.
	_ = os.Remove(backupPathIn(home))
	res2, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("second InstallHook: %v", err)
	}
	if !res2.AlreadyPresent || res2.Changed {
		t.Fatalf("post-migration re-install: got %+v, want AlreadyPresent no Changed", res2)
	}
	if _, err := os.Stat(backupPathIn(home)); !os.IsNotExist(err) {
		t.Fatalf("idempotent re-install must not write a .bak (err=%v)", err)
	}
}

// TestCodexInstallHookAppendsAndMatcher: a fresh Codex hooks.json gets our entry with
// the codex command and the "startup|resume" matcher; a second install is idempotent.
func TestCodexInstallHookAppendsAndMatcher(t *testing.T) {
	codexHome := t.TempDir()
	hookPath := codexHooksPathIn(codexHome)
	hm := NewCodexHookManager(hookPath)

	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if !res.Changed || res.BackedUp || res.AlreadyPresent {
		t.Fatalf("fresh codex install: got %+v, want Changed only", res)
	}
	m := decodeFile(t, hookPath)
	if cmds := commandsFrom(t, m); len(cmds) != 1 || cmds[0] != "uzi skill install --target codex" {
		t.Fatalf("codex commands=%v, want the codex canonical", cmds)
	}
	if ms := matchersFrom(t, m); len(ms) != 1 || ms[0] != "startup|resume" {
		t.Fatalf("codex matcher=%v, want [startup|resume]", ms)
	}

	res2, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("second InstallHook: %v", err)
	}
	if !res2.AlreadyPresent || res2.Changed {
		t.Fatalf("codex re-install: got %+v, want AlreadyPresent", res2)
	}
}

// TestClaudeMatcherIsStartup: the Claude entry uses the "startup" matcher (not codex's).
func TestClaudeMatcherIsStartup(t *testing.T) {
	home := t.TempDir()
	if _, err := NewHookManagerAt(home).InstallHook(); err != nil {
		t.Fatal(err)
	}
	if ms := matchersFrom(t, parseSettings(t, home)); len(ms) != 1 || ms[0] != "startup" {
		t.Fatalf("claude matcher=%v, want [startup]", ms)
	}
}

// TestBackupByteIdentical asserts the .bak equals the original file bytes exactly for
// BOTH a mutated Claude settings.json and a mutated Codex hooks.json.
func TestBackupByteIdentical(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		home := t.TempDir()
		const orig = "{\n  \"model\": \"opus\"\n}\n"
		writeSettings(t, home, orig)
		if _, err := NewHookManagerAt(home).InstallHook(); err != nil {
			t.Fatal(err)
		}
		bak, err := os.ReadFile(backupPathIn(home))
		if err != nil || !bytes.Equal(bak, []byte(orig)) {
			t.Fatalf("claude .bak not byte-identical: got %q err %v", bak, err)
		}
	})
	t.Run("codex", func(t *testing.T) {
		codexHome := t.TempDir()
		hookPath := codexHooksPathIn(codexHome)
		const orig = "{\n  \"description\": \"my codex hooks\"\n}\n"
		writeFileFixture(t, hookPath, orig)
		if _, err := NewCodexHookManager(hookPath).InstallHook(); err != nil {
			t.Fatal(err)
		}
		bak, err := os.ReadFile(hookPath + ".bak") //nolint:gosec // G304: test reads a test-controlled temp path
		if err != nil || !bytes.Equal(bak, []byte(orig)) {
			t.Fatalf("codex .bak not byte-identical: got %q err %v", bak, err)
		}
		// The top-level foreign "description" survives the re-encode.
		if decodeFile(t, hookPath)["description"] != "my codex hooks" {
			t.Errorf("foreign top-level description lost on codex install")
		}
	})
}

// TestForeignPreservationAndOrder: two foreign matcher-objects (one before, one after
// ours) survive semantically and in order across an install then uninstall, for both
// managers.
func TestForeignPreservationAndOrder(t *testing.T) {
	run := func(t *testing.T, hookPath string, hm *HookManager) {
		t.Helper()
		const raw = `{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "before-tool refresh"}]},
      {"matcher": "startup", "hooks": [{"type": "command", "command": "after-tool sync"}]}
    ]
  }
}`
		writeFileFixture(t, hookPath, raw)

		if _, err := hm.InstallHook(); err != nil {
			t.Fatalf("InstallHook: %v", err)
		}
		afterInstall := commandsFrom(t, decodeFile(t, hookPath))
		// Foreign entries keep their relative order; ours is appended at the end.
		if len(afterInstall) != 3 || afterInstall[0] != "before-tool refresh" || afterInstall[1] != "after-tool sync" {
			t.Fatalf("after install commands=%v, want the two foreign first in order then ours", afterInstall)
		}

		if _, err := hm.UninstallHook(); err != nil {
			t.Fatalf("UninstallHook: %v", err)
		}
		afterUninstall := commandsFrom(t, decodeFile(t, hookPath))
		if len(afterUninstall) != 2 || afterUninstall[0] != "before-tool refresh" || afterUninstall[1] != "after-tool sync" {
			t.Fatalf("after uninstall commands=%v, want the two foreign preserved in order", afterUninstall)
		}
	}

	t.Run("claude", func(t *testing.T) {
		home := t.TempDir()
		run(t, settingsPathIn(home), NewHookManagerAt(home))
	})
	t.Run("codex", func(t *testing.T) {
		codexHome := t.TempDir()
		hookPath := codexHooksPathIn(codexHome)
		run(t, hookPath, NewCodexHookManager(hookPath))
	})
}

// TestCodexMalformedAborts: a malformed hooks.json refuses (error, no write, no .bak).
func TestCodexMalformedAborts(t *testing.T) {
	codexHome := t.TempDir()
	hookPath := codexHooksPathIn(codexHome)
	const bad = "{not json"
	writeFileFixture(t, hookPath, bad)
	if _, err := NewCodexHookManager(hookPath).InstallHook(); err == nil {
		t.Fatal("InstallHook must error on malformed codex hooks.json")
	}
	if got, err := os.ReadFile(hookPath); err != nil || string(got) != bad { //nolint:gosec // G304: test reads a test-controlled temp path
		t.Fatalf("hooks.json changed on abort: got %q err %v", got, err)
	}
	if _, err := os.Stat(hookPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("no .bak may be written on the abort path (err=%v)", err)
	}
}

// TestCodexInlineHooksConflict: an inline [hooks] table in config.toml with no hooks.json
// entry of ours blocks install with ExitUsage and writes NOTHING; once hooks.json already
// holds our hook the same config.toml no longer blocks (idempotent).
func TestCodexInlineHooksConflict(t *testing.T) {
	codexHome := t.TempDir()
	hookPath := codexHooksPathIn(codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	const configTOML = "model = \"gpt-5\"\n\n[hooks]\nSessionStart = \"echo hi\"\n"
	writeFileFixture(t, configPath, configTOML)

	hm := NewCodexHookManager(hookPath)
	_, err := hm.InstallHook()
	if err == nil {
		t.Fatal("InstallHook must refuse when config.toml declares inline [hooks]")
	}
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitUsage {
		t.Fatalf("conflict error = %v, want ExitUsage", err)
	}
	// config.toml is unchanged and no hooks.json / trust state was written.
	if got, _ := os.ReadFile(configPath); string(got) != configTOML { //nolint:gosec // G304: test reads a test-controlled temp path
		t.Fatalf("config.toml was modified: %q", got)
	}
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Fatalf("hooks.json must not be written on the conflict path (err=%v)", err)
	}

	// With our hook already in hooks.json, the same config.toml does not block.
	writeFileFixture(t, hookPath, `{"hooks":{"SessionStart":[{"matcher":"startup|resume","hooks":[{"type":"command","command":"uzi skill install --target codex","timeout":15}]}]}}`)
	res, err := hm.InstallHook()
	if err != nil {
		t.Fatalf("idempotent install must not be blocked by config.toml: %v", err)
	}
	if !res.AlreadyPresent {
		t.Fatalf("install with our hook already present: got %+v, want AlreadyPresent", res)
	}
	// Still never wrote config.toml or any trust state.
	if got, _ := os.ReadFile(configPath); string(got) != configTOML { //nolint:gosec // G304: test reads a test-controlled temp path
		t.Fatalf("config.toml was modified on the idempotent path: %q", got)
	}
}

// TestCodexNeverWritesConfigOrTrust: after a normal Codex install, config.toml is
// byte-unchanged and no trust markers appear anywhere uzi wrote.
func TestCodexNeverWritesConfigOrTrust(t *testing.T) {
	codexHome := t.TempDir()
	hookPath := codexHooksPathIn(codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	const configTOML = "model = \"gpt-5\"\napproval_policy = \"on-request\"\n"
	writeFileFixture(t, configPath, configTOML)

	if _, err := NewCodexHookManager(hookPath).InstallHook(); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	if got, _ := os.ReadFile(configPath); string(got) != configTOML { //nolint:gosec // G304: test reads a test-controlled temp path
		t.Fatalf("config.toml must never be written: %q", got)
	}
	hooksRaw, err := os.ReadFile(hookPath) //nolint:gosec // G304: test reads a test-controlled temp path
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"trusted_hash", "hooks.state", "[hooks.state]"} {
		if bytes.Contains(hooksRaw, []byte(marker)) {
			t.Errorf("hooks.json unexpectedly contains trust marker %q:\n%s", marker, hooksRaw)
		}
	}
}

// TestNonTerminalRemovalWarning: a foreign entry AFTER ours makes UninstallHook record
// NonTerminalRemoval, and that field is json:"-" (absent from the DTO's JSON).
func TestNonTerminalRemovalWarning(t *testing.T) {
	codexHome := t.TempDir()
	hookPath := codexHooksPathIn(codexHome)
	hm := NewCodexHookManager(hookPath)
	if _, err := hm.InstallHook(); err != nil {
		t.Fatal(err)
	}
	// Append a foreign entry AFTER ours by editing the on-disk file.
	m := decodeFile(t, hookPath)
	hooks := m["hooks"].(map[string]any)
	arr := hooks["SessionStart"].([]any)
	arr = append(arr, map[string]any{
		"matcher": "startup|resume",
		"hooks":   []any{map[string]any{"type": "command", "command": "other-tool run"}},
	})
	hooks["SessionStart"] = arr
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(hookPath, out, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := hm.UninstallHook()
	if err != nil {
		t.Fatalf("UninstallHook: %v", err)
	}
	if !res.Changed || !res.NonTerminalRemoval {
		t.Fatalf("uninstall: got %+v, want Changed and NonTerminalRemoval", res)
	}
	// The DTO's JSON must NOT carry the flag (json:"-"): assert on the KEY set, since
	// a temp path value can incidentally contain the field name.
	j, _ := json.Marshal(res)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(j, &obj); err != nil {
		t.Fatal(err)
	}
	for k := range obj {
		if k == "non_terminal_removal" || k == "NonTerminalRemoval" {
			t.Errorf("HookUninstallResult JSON leaked NonTerminalRemoval key: %s", j)
		}
	}
}

// TestSiblingUnwritable: each target refreshes independently — a Codex install/uninstall
// succeeds while the Claude path is unwritable, and vice-versa.
func TestSiblingUnwritable(t *testing.T) {
	t.Run("codex works while claude unwritable", func(t *testing.T) {
		root := t.TempDir()
		// Claude path lives under a parent that is a regular file, so any write there fails.
		claudeHome := filepath.Join(root, "claude")
		if err := os.WriteFile(claudeHome, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		codexHome := filepath.Join(root, "codex")
		codexHook := codexHooksPathIn(codexHome)

		if _, err := NewCodexHookManager(codexHook).InstallHook(); err != nil {
			t.Fatalf("codex install must succeed regardless of the claude path: %v", err)
		}
		if !pathExistsHook(codexHook) {
			t.Fatalf("codex hooks.json not written")
		}
		// The Claude manager rooted at the file-parent fails, independently.
		if _, err := NewHookManagerAt(claudeHome).InstallHook(); err == nil {
			t.Fatalf("claude install under a non-dir home should fail")
		}
	})

	t.Run("claude works while codex unwritable", func(t *testing.T) {
		root := t.TempDir()
		claudeHome := filepath.Join(root, "claude")
		// Codex hooks.json under a parent that is a regular file.
		codexParent := filepath.Join(root, "codexfile")
		if err := os.WriteFile(codexParent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		codexHook := filepath.Join(codexParent, "hooks.json")

		if _, err := NewHookManagerAt(claudeHome).InstallHook(); err != nil {
			t.Fatalf("claude install must succeed regardless of the codex path: %v", err)
		}
		if !pathExistsHook(settingsPathIn(claudeHome)) {
			t.Fatalf("claude settings.json not written")
		}
		if _, err := NewCodexHookManager(codexHook).InstallHook(); err == nil {
			t.Fatalf("codex install under a non-dir parent should fail")
		}
	})
}

func pathExistsHook(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
