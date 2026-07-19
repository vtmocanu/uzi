package uzicli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// HookManager installs/removes an opt-in Claude Code `SessionStart` hook that
// runs `uzi skill install` when a Claude Code session starts (PRD #86 M2/M3).
//
// The hook lives in home/.claude/settings.json — a STRICT-JSON, user-scope file
// that is SHARED with other tools' hooks. Every mutation therefore round-trips
// the whole document through map[string]any so unrelated top-level keys and
// foreign hooks survive untouched, backs the file up to settings.json.bak
// before the first write, and REFUSES to write when the existing file is not
// valid JSON (clobbering a hand-maintained settings.json is worse than doing
// nothing). The home is resolved ONCE (os.UserHomeDir for the real CLI; an
// explicit dir for tests) and never from user input, mirroring SkillInstaller.
type HookManager struct {
	home string // base dir; settings.json lives at home/.claude/settings.json
}

// settingsDirParts is the FIXED location under $HOME, a compile-time constant
// with NO user-supplied component (mirrors skill.go's skillDirParts). Nothing
// here is derived from a flag/env/config, so no path traversal is expressible.
var settingsDirParts = []string{".claude"}

const (
	settingsFileName   = "settings.json"
	settingsBackupName = "settings.json.bak"

	// hookEvent is the settings.json hooks key we manage.
	hookEvent = "SessionStart"
	// hookMatcher scopes the hook to session startup.
	hookMatcher = "startup"
	// hookCommand is the canonical command we write.
	hookCommand = "uzi skill install"
	// hookCommandPrefix is the tolerant match: any SessionStart command that
	// starts with this prefix is treated as ours, so a user who appended flags
	// or tweaked the string is still recognised (PRD R5) — no orphan duplicate.
	hookCommandPrefix = "uzi skill install"
	// hookTimeout is the per-hook timeout, in seconds.
	hookTimeout = 15
)

// errSettingsMalformed is returned by the mutating methods when settings.json
// exists but is not valid JSON. We abort rather than overwrite it.
var errSettingsMalformed = errors.New("settings.json is not valid JSON; aborting to avoid clobbering it")

// NewHookManager builds a manager rooted at the user's home directory.
func NewHookManager() (*HookManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &HookManager{home: home}, nil
}

// NewHookManagerAt builds a manager rooted at an explicit home directory.
// TEST/INJECTION SEAM ONLY: the real CLI always goes through NewHookManager
// (os.UserHomeDir); this exists so tests point at a temp dir and never touch a
// developer's real ~/.claude/settings.json. It is not wired to any flag/env.
func NewHookManagerAt(home string) *HookManager {
	return &HookManager{home: home}
}

func (hm *HookManager) dir() string {
	return filepath.Join(append([]string{hm.home}, settingsDirParts...)...)
}
func (hm *HookManager) settingsPath() string { return filepath.Join(hm.dir(), settingsFileName) }
func (hm *HookManager) backupPath() string   { return filepath.Join(hm.dir(), settingsBackupName) }

// SettingsPath is where the managed settings.json is (or would be).
func (hm *HookManager) SettingsPath() string { return hm.settingsPath() }

// HookInstallResult reports what InstallHook did.
type HookInstallResult struct {
	Path           string `json:"path"`
	Changed        bool   `json:"changed"`         // the file was written this call
	AlreadyPresent bool   `json:"already_present"` // our hook was already there
	BackedUp       bool   `json:"backed_up"`       // the prior file was copied to .bak
	BackupPath     string `json:"backup_path,omitempty"`
}

// HookUninstallResult reports what UninstallHook did.
type HookUninstallResult struct {
	Path       string `json:"path"`
	Changed    bool   `json:"changed"`
	Removed    int    `json:"removed"` // count of hook entries removed
	BackedUp   bool   `json:"backed_up"`
	BackupPath string `json:"backup_path,omitempty"`
}

// HookStatusResult is what `uzi skill status` reports for the hook.
type HookStatusResult struct {
	Path       string `json:"path"`
	Installed  bool   `json:"installed"`  // at least one SessionStart command has our prefix
	Current    bool   `json:"current"`    // an entry's command == the canonical string
	Command    string `json:"command"`    // the canonical command we manage
	Duplicates int    `json:"duplicates"` // matching entries beyond the first (R5 orphans)
	Malformed  bool   `json:"malformed"`  // file exists but is not valid JSON
}

// readRaw reads settings.json. A missing file is not an error (existed=false).
func (hm *HookManager) readRaw() (data []byte, existed bool, err error) {
	b, err := os.ReadFile(hm.settingsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

// canonicalMatcherObject builds the SessionStart matcher-object we manage.
func canonicalMatcherObject() map[string]any {
	return map[string]any{
		"matcher": hookMatcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCommand,
				"timeout": hookTimeout,
			},
		},
	}
}

// commandHasOurPrefix reports whether an untrusted hook element is a
// command-object whose "command" starts with our prefix. It type-asserts every
// hop with the ,ok form so a malformed shape is skipped, never a panic.
func commandHasOurPrefix(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := m["command"].(string)
	if !ok {
		return false
	}
	return strings.HasPrefix(cmd, hookCommandPrefix)
}

// sessionStartArray returns the (possibly nil) SessionStart array from a parsed
// settings document, defensively type-asserting each hop.
func sessionStartArray(root map[string]any) []any {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	arr, _ := hooks["SessionStart"].([]any)
	return arr
}

// matcherObjectHasOurHook reports whether a SessionStart matcher-object holds a
// hook command with our prefix.
func matcherObjectHasOurHook(mo any) bool {
	m, ok := mo.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		if commandHasOurPrefix(h) {
			return true
		}
	}
	return false
}

// InstallHook ensures our SessionStart hook is present in settings.json. It
// preserves every unrelated key and foreign hook, is a no-op when our hook is
// already present, and refuses to touch a malformed file.
func (hm *HookManager) InstallHook() (HookInstallResult, error) {
	res := HookInstallResult{Path: hm.settingsPath(), BackupPath: hm.backupPath()}

	raw, existed, err := hm.readRaw()
	if err != nil {
		return res, err
	}

	root := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &root); err != nil {
			return res, errSettingsMalformed
		}
		if root == nil { // literal JSON null
			root = map[string]any{}
		}
	}

	// Already present anywhere in SessionStart ⇒ nothing to do, no write.
	for _, mo := range sessionStartArray(root) {
		if matcherObjectHasOurHook(mo) {
			res.AlreadyPresent = true
			return res, nil
		}
	}

	// Navigate/create hooks (object) → SessionStart (array), preserving siblings.
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	sessionStart = append(sessionStart, canonicalMatcherObject())
	hooks["SessionStart"] = sessionStart
	root["hooks"] = hooks

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return res, err
	}

	if err := os.MkdirAll(hm.dir(), 0o755); err != nil {
		return res, err
	}
	// Back up the prior file before the first mutating write.
	if existed {
		if err := writeFileAtomic(hm.backupPath(), raw, 0o644); err != nil {
			return res, err
		}
		res.BackedUp = true
	}
	if err := writeFileAtomic(hm.settingsPath(), out, 0o644); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// UninstallHook removes every SessionStart hook command with our prefix, prunes
// any container it empties, and leaves all siblings intact. Missing file or no
// SessionStart hooks ⇒ no-op; a malformed file ⇒ error, no write.
func (hm *HookManager) UninstallHook() (HookUninstallResult, error) {
	res := HookUninstallResult{Path: hm.settingsPath(), BackupPath: hm.backupPath()}

	raw, existed, err := hm.readRaw()
	if err != nil {
		return res, err
	}
	if !existed {
		return res, nil
	}

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return res, errSettingsMalformed
	}
	if root == nil {
		return res, nil
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return res, nil
	}
	sessionStart, ok := hooks["SessionStart"].([]any)
	if !ok {
		return res, nil
	}

	removed := 0
	kept := make([]any, 0, len(sessionStart))
	for _, mo := range sessionStart {
		m, ok := mo.(map[string]any)
		if !ok {
			kept = append(kept, mo) // unknown shape — leave it alone
			continue
		}
		inner, ok := m["hooks"].([]any)
		if !ok {
			kept = append(kept, mo)
			continue
		}
		keptInner := make([]any, 0, len(inner))
		for _, h := range inner {
			if commandHasOurPrefix(h) {
				removed++
				continue
			}
			keptInner = append(keptInner, h)
		}
		if len(keptInner) == 0 {
			continue // drop the now-empty matcher-object
		}
		m["hooks"] = keptInner
		kept = append(kept, m)
	}

	if removed == 0 {
		return res, nil // nothing of ours was present
	}

	// Prune empty containers, preserving all siblings.
	if len(kept) == 0 {
		delete(hooks, "SessionStart")
	} else {
		hooks["SessionStart"] = kept
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(hm.dir(), 0o755); err != nil {
		return res, err
	}
	if err := writeFileAtomic(hm.backupPath(), raw, 0o644); err != nil {
		return res, err
	}
	res.BackedUp = true
	if err := writeFileAtomic(hm.settingsPath(), out, 0o644); err != nil {
		return res, err
	}
	res.Changed = true
	res.Removed = removed
	return res, nil
}

// HookStatus reports whether our hook is installed, without writing. A missing
// file reads as not installed; an unparseable file sets Malformed (so status
// never lies "not installed" over a file we simply could not read).
func (hm *HookManager) HookStatus() HookStatusResult {
	res := HookStatusResult{Path: hm.settingsPath(), Command: hookCommand}

	raw, existed, err := hm.readRaw()
	if err != nil || !existed {
		return res
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		res.Malformed = true
		return res
	}

	matches := 0
	for _, mo := range sessionStartArray(root) {
		m, ok := mo.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := m["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			hmMap, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, ok := hmMap["command"].(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(cmd, hookCommandPrefix) {
				matches++
				if cmd == hookCommand {
					res.Current = true
				}
			}
		}
	}
	res.Installed = matches > 0
	if matches > 1 {
		res.Duplicates = matches - 1
	}
	return res
}
