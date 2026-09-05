package uzicli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// HookManager installs/removes an opt-in lifecycle hook that runs `uzi skill
// install --target <harness>` when a harness session starts (PRD #86, #1143).
//
// One manager serves BOTH harnesses; the target-specific pieces (file path,
// matcher, canonical command, backup perm, and the optional Codex config.toml
// conflict path) live on the struct, resolved once by a constructor:
//
//   - Claude: home/.claude/settings.json, matcher "startup", command
//     "uzi skill install --target claude", also recognizing the M1 legacy bare
//     "uzi skill install" so a first re-install MIGRATES it in place.
//   - Codex:  <codexHome>/hooks.json, matcher "startup|resume", command
//     "uzi skill install --target codex", no legacy forms.
//
// The hook file is a STRICT-JSON, user-scope file SHARED with other tools' hooks.
// Every mutation therefore round-trips the whole document through map[string]any so
// unrelated top-level keys and foreign hooks survive untouched, backs the file up
// byte-for-byte to <path>.bak before the first write, and REFUSES to write when the
// existing file is not valid JSON (clobbering a hand-maintained file is worse than
// doing nothing). The path is resolved ONCE (os.UserHomeDir / $CODEX_HOME for the
// real CLI, an explicit dir for tests) and never from user input, mirroring
// SkillInstaller.
type HookManager struct {
	path           string      // absolute hook file path
	matcher        string      // "startup" (claude) | "startup|resume" (codex)
	command        string      // canonical command this manager writes
	legacyCommands []string    // extra commands recognized as ours (claude: the M1 bare form)
	backupPerm     os.FileMode // file+backup perm: 0o600 (claude settings.json may hold secrets) | 0o644 (codex hooks.json)
	configTOMLPath string      // codex only: <codexHome>/config.toml, read-only [hooks]-table conflict check; "" for claude
}

const (
	settingsFileName = "settings.json"

	// hookEvent is the hooks key we manage in both harnesses' JSON.
	hookEvent = "SessionStart"
	// hookTimeout is the per-hook timeout, in seconds.
	hookTimeout = 15
)

// errSettingsMalformed is returned by the mutating methods when the hook file
// exists but is not valid JSON. We abort rather than overwrite it.
var errSettingsMalformed = errors.New("hook file is not valid JSON; aborting to avoid clobbering it")

// NewHookManager builds a Claude manager rooted at the user's home directory.
func NewHookManager() (*HookManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewHookManagerAt(home), nil
}

// NewHookManagerAt builds a Claude manager rooted at an explicit home directory.
// TEST/INJECTION SEAM ONLY: the real CLI always goes through NewHookManager
// (os.UserHomeDir); this exists so tests point at a temp dir and never touch a
// developer's real ~/.claude/settings.json. It is not wired to any flag/env.
func NewHookManagerAt(home string) *HookManager {
	return &HookManager{
		path:           filepath.Join(home, ".claude", settingsFileName),
		matcher:        "startup",
		command:        "uzi skill install --target claude",
		legacyCommands: []string{"uzi skill install"},
		backupPerm:     0o600,
	}
}

// NewCodexHookManager builds a Codex manager for an absolute hooks.json path (the
// path resolveSkillTargets already validated). The config.toml conflict check reads
// the sibling file in the same config home; Codex has no legacy command forms.
func NewCodexHookManager(hookPath string) *HookManager {
	return &HookManager{
		path:           hookPath,
		matcher:        "startup|resume",
		command:        "uzi skill install --target codex",
		legacyCommands: nil,
		backupPerm:     0o644,
		configTOMLPath: filepath.Join(filepath.Dir(hookPath), "config.toml"),
	}
}

func (hm *HookManager) dir() string          { return filepath.Dir(hm.path) }
func (hm *HookManager) settingsPath() string { return hm.path }
func (hm *HookManager) backupPath() string   { return hm.path + ".bak" }

// HookInstallResult reports what InstallHook did.
type HookInstallResult struct {
	Path           string `json:"path"`
	Changed        bool   `json:"changed"`         // the file was written this call
	AlreadyPresent bool   `json:"already_present"` // our canonical hook was already there
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
	// NonTerminalRemoval is set when a foreign hook entry followed ours in the
	// SessionStart order, so removing ours shifts the successors' Codex trust
	// indices. json:"-" keeps the mutating DTO's JSON byte-identical to today; the
	// CLI reads it to print a stderr warning.
	NonTerminalRemoval bool `json:"-"`
}

// HookStatusResult is what `uzi skill status` reports for the hook.
type HookStatusResult struct {
	Path       string `json:"path"`
	Installed  bool   `json:"installed"`  // at least one SessionStart command is one we manage
	Current    bool   `json:"current"`    // an entry's command is already in canonical form (exact or canonical+flags)
	Command    string `json:"command"`    // the canonical command we manage
	Duplicates int    `json:"duplicates"` // matching entries beyond the first (R5 orphans)
	Malformed  bool   `json:"malformed"`  // file exists but is not valid JSON
	// HookConfigConflict (Codex only) is set when config.toml declares a [hooks]
	// table and hooks.json does not already hold our hook — the mixed
	// representation Codex warns about. Additive to a READ DTO (no envelope change).
	HookConfigConflict bool `json:"hook_config_conflict,omitempty"`
}

// readRaw reads the hook file. A missing file is not an error (existed=false).
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

// decodeSettings parses the hook file into a top-level object. It uses UseNumber()
// so large-integer siblings survive verbatim (json.Unmarshal would coerce them to
// float64 and lose precision on re-marshal). A decode error or a non-object
// top-level (array, string, number, bool) is errSettingsMalformed — we never
// overwrite a shape we do not understand. A literal JSON `null` decodes to a nil
// map, which callers treat as an empty object.
//
// Unlike a bare json.Decoder.Decode (which stops after the first value and
// silently ignores trailing bytes), this REQUIRES the input to be a single JSON
// document: any trailing content after the first value — e.g. `{}{}` or
// `{"a":1}\ngarbage` — is malformed, matching json.Unmarshal's strictness so the
// "refuse to write over invalid JSON" guarantee holds. Trailing whitespace is fine.
func decodeSettings(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errSettingsMalformed
	}
	// Reject trailing data: a second decode must hit EOF (whitespace is skipped).
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errSettingsMalformed
	}
	if v == nil {
		return nil, nil // JSON null
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errSettingsMalformed
	}
	return root, nil
}

// encodeSettings marshals a settings document with HTML escaping OFF (so `&&`,
// `>`, `<` in foreign commands are preserved verbatim, not turned into &
// etc.) and 2-space indent. The encoder appends a trailing newline.
func encodeSettings(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// canonicalMatcherObject builds the SessionStart matcher-object this manager writes.
func (hm *HookManager) canonicalMatcherObject() map[string]any {
	return map[string]any{
		"matcher": hm.matcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hm.command,
				"timeout": hookTimeout,
			},
		},
	}
}

// isOurCommand reports whether a SessionStart command string is one we manage: the
// canonical command, or ANY legacy form, either exactly or followed by a space (so
// appended flags like `... --force` still count, PRD R5). The match is
// WORD-BOUNDARY aware — a longer word like the real sibling subcommand `uzi skill
// install-hook` is NOT ours, which a bare prefix match would destroy.
func (hm *HookManager) isOurCommand(cmd string) bool {
	if commandMatches(cmd, hm.command) {
		return true
	}
	for _, legacy := range hm.legacyCommands {
		if commandMatches(cmd, legacy) {
			return true
		}
	}
	return false
}

// commandMatches applies the word-boundary rule: exact, or base followed by a space.
func commandMatches(cmd, base string) bool {
	return cmd == base || strings.HasPrefix(cmd, base+" ")
}

// isCanonicalCommand reports whether a command is ALREADY in this manager's
// canonical, target-specific form: exactly the canonical command, or the canonical
// command followed by extra user flags (e.g. `... --force`). Such an entry is
// preserved verbatim on re-install — never rewritten — so a user's appended flags
// are not silently dropped. Only a legacy/non-canonical isOurCommand form migrates.
func (hm *HookManager) isCanonicalCommand(cmd string) bool {
	return commandMatches(cmd, hm.command)
}

// commandString extracts the "command" string from an untrusted hook element,
// type-asserting every hop with the ,ok form so a malformed shape is skipped.
func commandString(h any) (string, bool) {
	m, ok := h.(map[string]any)
	if !ok {
		return "", false
	}
	cmd, ok := m["command"].(string)
	return cmd, ok
}

// commandIsOurs reports whether an untrusted hook element is a command-object whose
// "command" is one we manage.
func (hm *HookManager) commandIsOurs(h any) bool {
	cmd, ok := commandString(h)
	return ok && hm.isOurCommand(cmd)
}

// sessionStartArray returns the (possibly nil) SessionStart array from a parsed
// settings document, defensively type-asserting each hop.
func sessionStartArray(root map[string]any) []any {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	arr, _ := hooks[hookEvent].([]any)
	return arr
}

// matcherObjectHasOurHook reports whether a SessionStart matcher-object holds a
// hook command we manage.
func (hm *HookManager) matcherObjectHasOurHook(mo any) bool {
	m, ok := mo.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		if hm.commandIsOurs(h) {
			return true
		}
	}
	return false
}

// hasOurHook reports whether any SessionStart entry is one we manage.
func (hm *HookManager) hasOurHook(root map[string]any) bool {
	for _, mo := range sessionStartArray(root) {
		if hm.matcherObjectHasOurHook(mo) {
			return true
		}
	}
	return false
}

// codexHookEventKeys is the set of Codex hook EVENT names. A config.toml [hooks]
// table declares an inline hook only via one of these keys, whether written inline
// (`SessionStart = "..."`) or as a standard header (`[hooks.SessionStart]`) — both
// decode to the same key under m["hooks"]. Codex's own trust-persistence subtable
// `[hooks.state]` (and any other non-event metadata under [hooks]) is NOT a hook
// event and must not count as an inline hook. Keys are PascalCase, matching Codex.
var codexHookEventKeys = map[string]struct{}{
	"PreToolUse":        {},
	"PermissionRequest": {},
	"PostToolUse":       {},
	"PreCompact":        {},
	"PostCompact":       {},
	"SessionStart":      {},
	"SessionEnd":        {},
	"UserPromptSubmit":  {},
	"SubagentStart":     {},
	"SubagentStop":      {},
	"Stop":              {},
}

// configHasInlineHooks (Codex only) reports whether config.toml declares at least one
// inline hook EVENT under its [hooks] table. It tests each [hooks] child key against
// codexHookEventKeys, which covers both the inline form (`SessionStart = "..."`) and
// the standard-header form (`[hooks.SessionStart]`), while EXCLUDING Codex's
// trust-persistence subtable `[hooks.state]` and any future non-event metadata under
// [hooks] — otherwise a config that keeps all its hooks in hooks.json but has ever
// trusted one would falsely conflict. Read-only: we NEVER write config.toml. A missing
// file mirrors LoadConfig's ErrNotExist handling (no conflict). A parse error is
// treated as "cannot detect → proceed": we do not block an install on someone else's
// malformed TOML, and since we never write it a wrong guess only risks Codex's own
// dedup warning, never data loss.
func (hm *HookManager) configHasInlineHooks() bool {
	b, err := os.ReadFile(hm.configTOMLPath)
	if err != nil {
		return false // missing/unreadable ⇒ cannot detect ⇒ proceed
	}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		return false // malformed TOML ⇒ cannot detect ⇒ proceed
	}
	hooks, ok := m["hooks"].(map[string]any)
	if !ok {
		return false
	}
	for key := range hooks {
		if _, isEvent := codexHookEventKeys[key]; isEvent {
			return true // a real inline hook event ⇒ mixed representation
		}
	}
	return false // only [hooks.state]/metadata ⇒ no inline hook event
}

// inlineHooksConflict reports the Codex mixed-representation conflict: a [hooks]
// table in config.toml AND our hook not already in hooks.json. Claude
// (configTOMLPath == "") never conflicts. hasOurHook is checked before the
// (file-reading) configHasInlineHooks so an idempotent re-install stays cheap.
func (hm *HookManager) inlineHooksConflict(root map[string]any) bool {
	return hm.configTOMLPath != "" && !hm.hasOurHook(root) && hm.configHasInlineHooks()
}

// migrateLegacyCommand rewrites the FIRST legacy/non-canonical SessionStart command
// we manage in place to the canonical command, PRESERVING any user suffix the legacy
// entry carried (e.g. `--force`), and reports whether it did. An entry ALREADY in
// canonical form (exact or canonical+flags) is skipped so a user's appended flags
// survive — those are handled by the caller's idempotent short-circuit. Migrating only
// the first match avoids appending a duplicate.
func (hm *HookManager) migrateLegacyCommand(root map[string]any) bool {
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
			hmap, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, ok := hmap["command"].(string)
			if !ok {
				continue
			}
			if hm.isCanonicalCommand(cmd) {
				continue // already canonical (exact or canonical+flags) — leave verbatim
			}
			for _, legacy := range hm.legacyCommands {
				if commandMatches(cmd, legacy) {
					// Rewrite to the canonical command, keeping any suffix the legacy
					// entry carried (TrimPrefix yields "" for the bare legacy form) so a
					// user's appended flags are never silently dropped.
					hmap["command"] = hm.command + strings.TrimPrefix(cmd, legacy)
					return true
				}
			}
		}
	}
	return false
}

// InstallHook ensures our SessionStart hook is present in the hook file. It
// preserves every unrelated key and foreign hook and their order, refuses to touch
// a malformed file, and classifies the existing entries three ways: an entry already
// in canonical form (exact OR canonical+flags) is an idempotent no-op preserved
// verbatim; a legacy/non-canonical form is MIGRATED in place to the canonical
// command, preserving any user suffix it carried (no duplicate); otherwise the
// canonical matcher-object is appended. For
// Codex it first refuses (read-only) a mixed [hooks]-table/hooks.json representation.
func (hm *HookManager) InstallHook() (HookInstallResult, error) {
	res := HookInstallResult{Path: hm.settingsPath(), BackupPath: hm.backupPath()}

	raw, existed, err := hm.readRaw()
	if err != nil {
		return res, err
	}

	root := map[string]any{}
	if existed {
		decoded, err := decodeSettings(raw)
		if err != nil {
			return res, err
		}
		if decoded != nil { // nil ⇒ literal JSON null ⇒ treat as empty object
			root = decoded
		}
	}

	// Codex only: refuse a mixed hook representation before mutating anything. This
	// is READ-ONLY detection — we never write config.toml, [hooks.state], or
	// trusted_hash (PRD #1143 M2).
	if hm.inlineHooksConflict(root) {
		return res, Exitf(ExitUsage,
			"Codex already declares a [hooks] table in %s; consolidate your Codex hooks on %s "+
				"(Codex warns when both a [hooks] table in config.toml and a hooks.json are present) and re-run",
			hm.configTOMLPath, hm.settingsPath())
	}

	// An entry already in canonical form (exact OR canonical+flags) is present
	// anywhere ⇒ idempotent, no write. Such an entry is target-specific already, so
	// we preserve it verbatim rather than normalizing away a user's appended flags.
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
			if cmd, ok := commandString(h); ok && hm.isCanonicalCommand(cmd) {
				res.AlreadyPresent = true
				return res, nil
			}
		}
	}

	// A legacy form present ⇒ migrate it in place. Otherwise navigate/create hooks
	// (object) → SessionStart (array) and append, preserving siblings. A
	// present-but-wrong-typed `hooks` or `SessionStart` aborts like a malformed file.
	if !hm.migrateLegacyCommand(root) {
		hooks := map[string]any{}
		if v, present := root["hooks"]; present {
			m, ok := v.(map[string]any)
			if !ok {
				return res, errSettingsMalformed
			}
			hooks = m
		}
		var sessionStart []any
		if v, present := hooks[hookEvent]; present {
			arr, ok := v.([]any)
			if !ok {
				return res, errSettingsMalformed
			}
			sessionStart = arr
		}
		sessionStart = append(sessionStart, hm.canonicalMatcherObject())
		hooks[hookEvent] = sessionStart
		root["hooks"] = hooks
	}

	out, err := encodeSettings(root)
	if err != nil {
		return res, err
	}

	if err := os.MkdirAll(hm.dir(), 0o750); err != nil {
		return res, err
	}
	// Back up the prior file BYTE-FOR-BYTE before the first mutating write. The perm
	// matches the target: Claude settings.json can hold secrets (0o600), Codex
	// hooks.json does not (0o644).
	if existed {
		if err := writeFileAtomic(hm.backupPath(), raw, hm.backupPerm); err != nil {
			return res, err
		}
		res.BackedUp = true
	}
	if err := writeFileAtomic(hm.settingsPath(), out, hm.backupPerm); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// UninstallHook removes every SessionStart hook command we manage, prunes any
// container it empties, and leaves all siblings intact. Missing file or no hooks of
// ours ⇒ no-op; a malformed file ⇒ error, no write. It records NonTerminalRemoval
// when a foreign entry followed ours (its Codex trust index would shift).
func (hm *HookManager) UninstallHook() (HookUninstallResult, error) {
	res := HookUninstallResult{Path: hm.settingsPath(), BackupPath: hm.backupPath()}

	raw, existed, err := hm.readRaw()
	if err != nil {
		return res, err
	}
	if !existed {
		return res, nil
	}

	root, err := decodeSettings(raw)
	if err != nil {
		return res, err
	}
	if root == nil {
		return res, nil
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return res, nil
	}
	sessionStart, ok := hooks[hookEvent].([]any)
	if !ok {
		return res, nil
	}

	// Flatten the SessionStart commands (in order) to detect a non-terminal removal:
	// a foreign entry positioned AFTER one of ours whose trust index would shift.
	var flat []bool
	for _, mo := range sessionStart {
		m, ok := mo.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := m["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			flat = append(flat, hm.commandIsOurs(h))
		}
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
			if hm.commandIsOurs(h) {
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
	res.NonTerminalRemoval = hasSuccessorAfterOurs(flat)

	// Prune empty containers, preserving all siblings.
	if len(kept) == 0 {
		delete(hooks, hookEvent)
	} else {
		hooks[hookEvent] = kept
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}

	out, err := encodeSettings(root)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(hm.dir(), 0o750); err != nil {
		return res, err
	}
	if err := writeFileAtomic(hm.backupPath(), raw, hm.backupPerm); err != nil {
		return res, err
	}
	res.BackedUp = true
	if err := writeFileAtomic(hm.settingsPath(), out, hm.backupPerm); err != nil {
		return res, err
	}
	res.Changed = true
	res.Removed = removed
	return res, nil
}

// hasSuccessorAfterOurs reports whether a foreign (non-ours) hook appears anywhere
// after one of ours in the flattened SessionStart order.
func hasSuccessorAfterOurs(flat []bool) bool {
	seenOurs := false
	for _, ours := range flat {
		if ours {
			seenOurs = true
			continue
		}
		if seenOurs {
			return true
		}
	}
	return false
}

// HookStatus reports whether our hook is installed, without writing. A missing file
// reads as not installed; an unparseable file sets Malformed (so status never lies
// "not installed" over a file we simply could not read). For Codex it also reports a
// mixed [hooks]-table/hooks.json config conflict.
func (hm *HookManager) HookStatus() HookStatusResult {
	res := HookStatusResult{Path: hm.settingsPath(), Command: hm.command}

	raw, existed, err := hm.readRaw()
	if err != nil {
		return res
	}

	var root map[string]any
	if existed {
		root, err = decodeSettings(raw)
		if err != nil {
			res.Malformed = true
			return res
		}
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
			cmd, ok := commandString(h)
			if !ok {
				continue
			}
			if hm.isOurCommand(cmd) {
				matches++
				if hm.isCanonicalCommand(cmd) {
					res.Current = true
				}
			}
		}
	}
	res.Installed = matches > 0
	if matches > 1 {
		res.Duplicates = matches - 1
	}
	res.HookConfigConflict = hm.inlineHooksConflict(root)
	return res
}
