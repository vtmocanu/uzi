package uzicli

import (
	"os"
	"path/filepath"
)

// codexSkillDirParts is the FIXED Codex user-skill tail under $HOME — the
// documented `.agents/skills` root (Codex CLI 0.153.4). Like skillDirParts it is a
// compile-time constant with NO user-supplied component, and it is deliberately
// INDEPENDENT of $CODEX_HOME: only the Codex hook/config home follows that env var
// (see ResolveSkillTargets), so no environment-derived string ever reaches the
// skill path component (PRD #1143 D1/D8).
var codexSkillDirParts = []string{".agents", "skills", "uzi-cli"}

// codexHookFileName is the documented Codex user-layer hook file, read/written
// under the Codex config home ($CODEX_HOME, or ~/.codex when unset) by
// NewCodexHookManager (skillhook.go).
const codexHookFileName = "hooks.json"

// SkillTarget describes one harness the bundled skill can install into: the
// absolute directory the SKILL.md copy lives in and the absolute path to that
// harness's lifecycle-hook file. Both are fully resolved by ResolveSkillTargets.
type SkillTarget struct {
	Name     string // "claude" | "codex"
	SkillDir string // absolute dir where SKILL.md lives
	HookPath string // absolute path to the hook file
}

// ResolvedTarget pairs a SkillTarget with the metadata target selection needs: is
// the automatic (presence-based) path allowed to touch it, and — for a target a
// user selects explicitly — is that selection itself a usage error.
type ResolvedTarget struct {
	Target SkillTarget
	// Detected reports whether the automatic install path applies (Claude is always
	// detected; Codex only when its config home already exists as a directory).
	Detected bool
	// SelectErr is non-nil when explicitly selecting this target is a usage error
	// (today: a relative $CODEX_HOME). It never fires on the automatic path.
	SelectErr error
}

// ResolveSkillTargets builds both install targets from a home dir and an env-lookup
// seam. The seam (never a package-global os.Getenv) is what keeps a test's temp
// home from being polluted by a developer's exported $CODEX_HOME.
//
// The Claude skill and hook paths are fully fixed under $HOME. The Codex skill tail
// is likewise fixed under $HOME (.agents/skills/uzi-cli); ONLY the Codex hook/config
// home is derived from $CODEX_HOME, and that value is validated absolute and cleaned
// with filepath.Clean before it becomes a path component — so no untrusted input can
// reach any path we write (PRD #1143 D8).
func ResolveSkillTargets(home string, getenv func(string) string) []ResolvedTarget {
	claude := ResolvedTarget{
		Target: SkillTarget{
			Name:     "claude",
			SkillDir: filepath.Join(append([]string{home}, skillDirParts...)...),
			HookPath: filepath.Join(home, ".claude", settingsFileName),
		},
		Detected: true,
	}

	codex := ResolvedTarget{
		Target: SkillTarget{
			Name:     "codex",
			SkillDir: filepath.Join(append([]string{home}, codexSkillDirParts...)...),
		},
	}
	switch raw := getenv("CODEX_HOME"); {
	case raw == "":
		codexHome := filepath.Join(home, ".codex")
		codex.Target.HookPath = filepath.Join(codexHome, codexHookFileName)
		codex.Detected = isDir(codexHome)
	case filepath.IsAbs(raw):
		codexHome := filepath.Clean(raw)
		codex.Target.HookPath = filepath.Join(codexHome, codexHookFileName)
		codex.Detected = isDir(codexHome)
	default:
		// A relative, non-empty $CODEX_HOME is never honored: skipped on the automatic
		// path (Detected stays false) and a usage error when Codex is selected
		// explicitly. HookPath is left empty on purpose — selection errors out before
		// anything could use it.
		codex.SelectErr = Exitf(ExitUsage, "CODEX_HOME must be an absolute path, got %q", raw)
	}

	return []ResolvedTarget{claude, codex}
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
