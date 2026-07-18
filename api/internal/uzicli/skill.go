package uzicli

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// embeddedSkill is the bundled Claude Code skill, compiled into the binary via
// go:embed. It is the single source of truth for the installed skill: `uzi`
// writes it to ~/.claude/skills/uzi-cli/SKILL.md so agents always know how to
// drive THIS binary's command surface (skew is structurally impossible — the
// skill ships inside the CLI). The skill<->cobra-tree drift test asserts every
// command/flag this file documents exists in the real command tree.
//
//go:embed skill/SKILL.md
var embeddedSkill string

// EmbeddedSkill returns the SKILL.md content compiled into this binary. The drift
// test and `uzi skill status` read it from here so both see the shipped bytes.
func EmbeddedSkill() string { return embeddedSkill }

// skillDirParts is the FIXED install location under $HOME, as a compile-time
// constant with NO user-supplied component. Because nothing here is derived from
// a flag/env/config, no path traversal is expressible and the writer can never
// reach ~/.claude/commands/ (a disjoint tree). Do not thread user input into it.
var skillDirParts = []string{".claude", "skills", "uzi-cli"}

const (
	skillFileName   = "SKILL.md"
	skillBackupName = "SKILL.md.bak"
	skillStateName  = ".uzi-cli-state.json"
)

// SkillInstaller writes the bundled skill under a home directory and tracks
// staleness by content hash. The home is resolved ONCE (os.UserHomeDir for the
// real CLI; an explicit dir for tests) and never from user input.
type SkillInstaller struct {
	home    string // base dir; the skill lives at home/.claude/skills/uzi-cli/
	version string // CLI version, recorded in the sidecar (informational only)
}

// NewSkillInstaller builds an installer rooted at the user's home directory.
// A missing home dir is an error the caller treats as "nothing to install".
func NewSkillInstaller(version string) (*SkillInstaller, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &SkillInstaller{home: home, version: version}, nil
}

// NewSkillInstallerAt builds an installer rooted at an explicit home directory.
// TEST/INJECTION SEAM ONLY: the real CLI always goes through NewSkillInstaller
// (os.UserHomeDir); this exists so tests can point the install at a temp dir and
// never touch the developer's real ~/.claude. It is not wired to any flag/env.
func NewSkillInstallerAt(home, version string) *SkillInstaller {
	return &SkillInstaller{home: home, version: version}
}

func (si *SkillInstaller) dir() string {
	return filepath.Join(append([]string{si.home}, skillDirParts...)...)
}
func (si *SkillInstaller) skillPath() string  { return filepath.Join(si.dir(), skillFileName) }
func (si *SkillInstaller) backupPath() string { return filepath.Join(si.dir(), skillBackupName) }
func (si *SkillInstaller) statePath() string  { return filepath.Join(si.dir(), skillStateName) }

// SkillPath is where the skill is (or would be) installed.
func (si *SkillInstaller) SkillPath() string { return si.skillPath() }

// skillState is the sidecar recording what we last wrote. Staleness keys on the
// hash (skill_sha256), NOT on cli_version — a version bump with an unchanged
// skill must not rewrite. cli_version is retained for human forensics.
type skillState struct {
	CLIVersion  string `json:"cli_version"`
	SkillSHA256 string `json:"skill_sha256"`
}

// SkillStatusResult is what `uzi skill status` reports.
type SkillStatusResult struct {
	Path        string `json:"path"`
	Installed   bool   `json:"installed"`
	UpToDate    bool   `json:"up_to_date"`
	UserEdited  bool   `json:"user_edited"`
	EmbeddedSHA string `json:"embedded_sha256"`
	OnDiskSHA   string `json:"on_disk_sha256,omitempty"`
}

// SkillInstallResult reports what an install call did. All false ⇒ already
// current, nothing written.
type SkillInstallResult struct {
	Path           string `json:"path"`
	Wrote          bool   `json:"wrote"`            // the skill was (re)written this call
	AlreadyCurrent bool   `json:"already_current"`  // on-disk already matched the embedded skill
	BackedUp       bool   `json:"backed_up"`        // a user-edited SKILL.md was preserved
	BackupPath     string `json:"backup_path,omitempty"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (si *SkillInstaller) embeddedSHA() string { return sha256Hex([]byte(embeddedSkill)) }

// readOnDisk returns the installed SKILL.md bytes and whether it exists.
func (si *SkillInstaller) readOnDisk() ([]byte, bool) {
	b, err := os.ReadFile(si.skillPath())
	if err != nil {
		return nil, false
	}
	return b, true
}

// recordedSHA reads the last-written hash from the sidecar, or "" when the
// sidecar is absent or unreadable (treated as "never installed by us").
func (si *SkillInstaller) recordedSHA() string {
	b, err := os.ReadFile(si.statePath())
	if err != nil {
		return ""
	}
	var st skillState
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	return st.SkillSHA256
}

// Status reports whether the installed skill is current, without writing.
func (si *SkillInstaller) Status() SkillStatusResult {
	e := si.embeddedSHA()
	res := SkillStatusResult{Path: si.skillPath(), EmbeddedSHA: e}
	onDisk, ok := si.readOnDisk()
	if !ok {
		return res // not installed
	}
	d := sha256Hex(onDisk)
	res.Installed = true
	res.OnDiskSHA = d
	res.UpToDate = d == e
	// A user edit is an on-disk file that differs both from what we'd ship and
	// from what we last wrote (the sidecar). The second guard keeps a merely
	// stale sidecar (file untouched) from reading as an edit.
	res.UserEdited = d != e && d != si.recordedSHA()
	return res
}

// Install ensures the on-disk SKILL.md equals the embedded skill and the sidecar
// records it. It NEVER deletes the user's work: if the on-disk file was edited
// since we last wrote it, the edit is copied to SKILL.md.bak before the rewrite.
// Writes are atomic (temp file + rename) so racing `uzi` processes and crashes
// never leave a torn file.
//
//   - force=false: rewrite only when the on-disk content differs from embedded
//     (covers a stale skill and a missing file), refreshing the sidecar in either
//     case. An up-to-date install is a no-op (AlreadyCurrent).
//   - force=true: always rewrite (still backing up a detected edit first).
func (si *SkillInstaller) Install(force bool) (SkillInstallResult, error) {
	e := si.embeddedSHA()
	res := SkillInstallResult{Path: si.skillPath(), BackupPath: si.backupPath()}

	onDisk, exists := si.readOnDisk()
	d := ""
	if exists {
		d = sha256Hex(onDisk)
	}
	recorded := si.recordedSHA()

	// Fast path: file present, matches embedded, sidecar agrees. Nothing to do.
	if !force && exists && d == e && recorded == e {
		res.AlreadyCurrent = true
		return res, nil
	}

	if err := os.MkdirAll(si.dir(), 0o755); err != nil {
		return res, err
	}

	// Preserve a genuine user edit: present, differs from what we'll write, and
	// differs from what we last wrote. (A file already equal to embedded, or one
	// only the sidecar disagrees with, is not an edit worth a .bak.)
	if exists && d != e && d != recorded {
		if err := writeFileAtomic(si.backupPath(), onDisk, 0o644); err != nil {
			return res, err
		}
		res.BackedUp = true
	}

	// Write the skill unless the file already matches embedded (then only the
	// sidecar was stale and we just refresh it below).
	if force || !exists || d != e {
		if err := writeFileAtomic(si.skillPath(), []byte(embeddedSkill), 0o644); err != nil {
			return res, err
		}
		res.Wrote = true
	}

	if err := si.writeState(skillState{CLIVersion: si.version, SkillSHA256: e}); err != nil {
		return res, err
	}
	return res, nil
}

func (si *SkillInstaller) writeState(st skillState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(si.statePath(), b, 0o644)
}
