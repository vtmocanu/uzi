package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// decodeTopLevel decodes a --json envelope into its top-level key set, so a test
// can assert on exact key presence/absence rather than a substring match.
func decodeTopLevel(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("envelope not decodable: %v\n%s", err, out)
	}
	return m
}

// envelopeTarget is one decoded targets[] element with an opaque result payload.
type envelopeTarget struct {
	Target   string          `json:"target"`
	Result   json.RawMessage `json:"result"`
	Error    string          `json:"error"`
	ExitCode int             `json:"exit_code"`
}

// decodeTargets pulls the targets[] array out of an envelope.
func decodeTargets(t *testing.T, top map[string]json.RawMessage) []envelopeTarget {
	t.Helper()
	raw, ok := top["targets"]
	if !ok {
		t.Fatalf("envelope has no targets[] key: %v", top)
	}
	var targets []envelopeTarget
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatalf("targets[] not decodable: %v", err)
	}
	return targets
}

// Legacy status: no --target, Codex not detected — the pre-M3 top-level {skill,hook}
// object is byte-preserved AND an additive single-element targets[] is present.
func TestSkillStatusLegacyEnvelope(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	top := decodeTopLevel(t, out)
	if _, ok := top["skill"]; !ok {
		t.Errorf("legacy status --json missing top-level skill:\n%s", out)
	}
	if _, ok := top["hook"]; !ok {
		t.Errorf("legacy status --json missing top-level hook:\n%s", out)
	}
	targets := decodeTargets(t, top)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].Target != "claude" || targets[0].ExitCode != 0 || targets[0].Error != "" {
		t.Errorf("target[0] = %+v, want claude/exit0/no-error", targets[0])
	}
	// The per-target result is itself a {skill,hook} object.
	var r map[string]json.RawMessage
	if err := json.Unmarshal(targets[0].Result, &r); err != nil {
		t.Fatalf("result not decodable: %v", err)
	}
	if _, ok := r["skill"]; !ok {
		t.Errorf("target result missing skill: %s", targets[0].Result)
	}
	if _, ok := r["hook"]; !ok {
		t.Errorf("target result missing hook: %s", targets[0].Result)
	}
}

// Legacy install: the pre-M3 top-level SkillInstallResult fields are preserved AND
// an additive targets[] is present.
func TestSkillInstallLegacyEnvelope(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	top := decodeTopLevel(t, out)
	for _, k := range []string{"path", "wrote", "already_current", "backed_up"} {
		if _, ok := top[k]; !ok {
			t.Errorf("legacy install --json missing top-level %q:\n%s", k, out)
		}
	}
	targets := decodeTargets(t, top)
	if len(targets) != 1 || targets[0].Target != "claude" {
		t.Fatalf("want 1 claude target, got %+v", targets)
	}
}

// Codex-detected (a ~/.codex home under SkillHome): the no-target envelope lists
// [claude, codex] in that order while the top level still promotes claude's fields.
func TestSkillStatusCodexDetectedEnvelope(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	top := decodeTopLevel(t, out)
	if _, ok := top["skill"]; !ok {
		t.Errorf("top level still promotes claude's skill:\n%s", out)
	}
	targets := decodeTargets(t, top)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d: %+v", len(targets), targets)
	}
	if targets[0].Target != "claude" || targets[1].Target != "codex" {
		t.Errorf("targets order = [%s,%s], want [claude,codex]", targets[0].Target, targets[1].Target)
	}
}

// New syntax (--target set): the envelope is {targets:[...]} ONLY — NO promoted
// legacy top-level key survives.
func TestSkillStatusTargetEnvelopeOnly(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "status", "--target", "all", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	top := decodeTopLevel(t, out)
	for _, k := range []string{"skill", "hook", "path"} {
		if _, ok := top[k]; ok {
			t.Errorf("--target envelope must NOT carry top-level %q:\n%s", k, out)
		}
	}
	targets := decodeTargets(t, top)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	// Exactly the one key.
	if len(top) != 1 {
		t.Errorf("--target envelope has extra top-level keys: %v", top)
	}
}

// Partial failure: `install --target all` where the Codex tree is unwritable — the
// envelope still lists BOTH targets, the failed one carries a non-empty error and a
// non-zero exit_code, the process exit equals the first failing target's code, and
// the succeeding (claude) target's install still happened.
func TestSkillInstallPartialFailureEnvelope(t *testing.T) {
	home := t.TempDir()
	// Make ~/.agents a regular file so the Codex skill dir MkdirAll fails while the
	// Claude tree (~/.claude) installs cleanly.
	if err := os.WriteFile(filepath.Join(home, ".agents"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install", "--target", "all", "--json")
	if code == uzicli.ExitOK {
		t.Fatalf("exit = %d, want non-zero on a failed target", code)
	}
	top := decodeTopLevel(t, out)
	targets := decodeTargets(t, top)
	if len(targets) != 2 {
		t.Fatalf("want both targets listed, got %d: %+v", len(targets), targets)
	}
	claude, codex := targets[0], targets[1]
	if claude.Target != "claude" || claude.Error != "" || claude.ExitCode != 0 {
		t.Errorf("claude target = %+v, want success", claude)
	}
	if codex.Target != "codex" || codex.Error == "" || codex.ExitCode == 0 {
		t.Errorf("codex target = %+v, want a failure with a non-zero exit_code", codex)
	}
	if code != codex.ExitCode {
		t.Errorf("process exit = %d, want the first failing target's code %d", code, codex.ExitCode)
	}
	// The succeeding target's work still happened.
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("claude install must still have written SKILL.md despite the codex failure")
	}
}

// codexSkillPath is the fixed Codex user-skill copy under a home dir.
func codexSkillPath(home string) string {
	return filepath.Join(home, ".agents", "skills", "uzi-cli", "SKILL.md")
}

// pathExists reports whether a path is present on disk.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// `skill install --target codex` with no Codex home selects Codex and CREATES the
// fixed `.agents/skills/uzi-cli` copy, without touching the Claude tree.
func TestSkillInstallTargetCodexCreatesCopy(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "install", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := os.ReadFile(codexSkillPath(home))
	if err != nil || string(got) != uzicli.EmbeddedSkill() {
		t.Fatalf("--target codex did not write the Codex SKILL.md (err=%v)", err)
	}
	if pathExists(installedSkillPath(home)) {
		t.Errorf("--target codex must not write the Claude copy")
	}
	if !strings.Contains(out, "codex") || !strings.Contains(out, "installed") {
		t.Errorf("output = %q, want a codex install line", out)
	}
}

// `skill status --target codex` and `skill uninstall-hook --target codex` create
// NOTHING when no Codex home exists: status is read-only, and uninstall-hook over a
// missing hooks.json is a clean no-op (never writes, never MkdirAll).
func TestSkillCodexReadOnlyVerbsCreateNothing(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home

	if _, _, code := runCLI(t, env, "skill", "status", "--target", "codex"); code != uzicli.ExitOK {
		t.Fatalf("status --target codex exit = %d, want 0", code)
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("status must not create the Codex skill copy")
	}

	_, _, code := runCLI(t, env, "skill", "uninstall-hook", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("uninstall-hook --target codex exit = %d, want 0 (no-op over a missing hooks.json)", code)
	}
	if pathExists(filepath.Join(home, ".codex")) || pathExists(filepath.Join(home, ".agents")) {
		t.Errorf("a read-only/no-op Codex verb must create no directories")
	}
}

// codexHooksFilePath is the Codex hooks.json under the default ~/.codex config home.
func codexHooksFilePath(home string) string {
	return filepath.Join(home, ".codex", "hooks.json")
}

// `skill install-hook --target codex` writes ~/.codex/hooks.json with the codex
// command and matcher, and prints the /hooks stderr guidance.
func TestSkillInstallHookTargetCodex(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home

	out, errb, code := runCLI(t, env, "skill", "install-hook", "--target", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	b, err := os.ReadFile(codexHooksFilePath(home))
	if err != nil {
		t.Fatalf("hooks.json not written: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "uzi skill install --target codex") {
		t.Errorf("hooks.json missing the codex command:\n%s", s)
	}
	if !strings.Contains(s, "startup|resume") {
		t.Errorf("hooks.json missing the codex matcher:\n%s", s)
	}
	if !strings.Contains(out, "codex") {
		t.Errorf("stdout = %q, want a codex line", out)
	}
	if !strings.Contains(errb, "/hooks") {
		t.Errorf("stderr = %q, want the /hooks trust guidance", errb)
	}
	// uzi never writes Codex config or trust state.
	if pathExists(filepath.Join(home, ".codex", "config.toml")) {
		t.Errorf("install-hook must never create config.toml")
	}
}

// `skill status --target all` shows hook rows for BOTH targets.
func TestSkillStatusTargetAllShowsHookRows(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	out, _, code := runCLI(t, env, "skill", "status", "--target", "all")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Count(out, "HOOK_INSTALLED") != 2 {
		t.Errorf("status --target all should show HOOK_INSTALLED for both targets:\n%s", out)
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, "codex") {
		t.Errorf("status --target all should name both targets:\n%s", out)
	}
}

// `skill install --target all` installs BOTH targets even when Codex is not detected.
func TestSkillInstallTargetAllInstallsBoth(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "all"); code != uzicli.ExitOK {
		t.Fatalf("install --target all exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) || !pathExists(codexSkillPath(home)) {
		t.Errorf("--target all must install both the Claude and Codex copies")
	}
}

// A relative CODEX_HOME (injected through the Env.Getenv seam, never a real exported
// var) is a usage error when Codex is explicitly selected, and silently skipped on
// the automatic path.
func TestSkillTargetCodexRelativeHomeIsUsageError(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	env.Getenv = func(k string) string {
		if k == "CODEX_HOME" {
			return "relcodex"
		}
		return ""
	}
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "codex"); code != uzicli.ExitUsage {
		t.Fatalf("install --target codex with relative CODEX_HOME exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if _, _, code := runCLI(t, env, "skill", "install", "--target", "all"); code != uzicli.ExitUsage {
		t.Fatalf("install --target all with relative CODEX_HOME exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("a rejected relative CODEX_HOME must not write the Codex copy")
	}

	// Auto path: skip Codex silently, still install Claude.
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	autoEnv := skillEnv(fc, home)
	autoEnv.Getenv = env.Getenv
	if _, _, code := runCLI(t, autoEnv, "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("auto path with relative CODEX_HOME exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must still install the Claude copy")
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must skip a relative-CODEX_HOME Codex target")
	}
}

// An invalid --target value is a usage error on every verb.
func TestSkillInvalidTarget(t *testing.T) {
	home := t.TempDir()
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = home
	for _, verb := range [][]string{
		{"skill", "status"}, {"skill", "install"},
		{"skill", "install-hook"}, {"skill", "uninstall-hook"},
	} {
		args := append(append([]string{}, verb...), "--target", "bogus")
		if _, _, code := runCLI(t, env, args...); code != uzicli.ExitUsage {
			t.Errorf("%v --target bogus exit = %d, want %d", verb, code, uzicli.ExitUsage)
		}
	}
}

// The automatic path installs the Codex copy too when a Codex config home already
// exists (here the default ~/.codex under the temp home), while keeping the seam
// deterministic (no real CODEX_HOME consulted).
func TestAutoUpgradeInstallsCodexWhenDetected(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o750); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must install the Claude copy")
	}
	if !pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must install the Codex copy when ~/.codex exists")
	}
}

// The automatic path does NOT install Codex when no Codex home exists (it must not
// litter a machine that does not run Codex).
func TestAutoUpgradeSkipsUndetectedCodex(t *testing.T) {
	t.Setenv("UZI_SKILL_AUTO_UPGRADE", "")
	home := t.TempDir()
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@b.c"}}
	if _, _, code := runCLI(t, skillEnv(fc, home), "whoami", "--json"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !pathExists(installedSkillPath(home)) {
		t.Errorf("auto path must install the Claude copy")
	}
	if pathExists(codexSkillPath(home)) {
		t.Errorf("auto path must not create a Codex copy with no Codex home")
	}
}
