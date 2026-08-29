package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentSourceValidateEnabled pins the strict bool gate on agent_source_enabled
// (PRD #602 M2): exactly "true"/"false", mirroring selfimprove_enabled. Without its
// arm the default branch (ValidateLabel) would accept "yes" and the kill-switch
// would read as false, silently keeping the feature off after an admin turned it on.
func TestAgentSourceValidateEnabled(t *testing.T) {
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeyAgentSourceEnabled, ok); err != nil {
			t.Errorf("Validate(agent_source_enabled, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "yes", "TRUE", "1", "0", "banana"} {
		if err := Validate(KeyAgentSourceEnabled, bad); err == nil {
			t.Errorf("Validate(agent_source_enabled, %q) = nil, want a non-bool rejection", bad)
		}
	}
}

// TestAgentSourceValidateInterval pins the duration gate with its 1m floor (PRD
// #602 M2): a valid Go duration >= 1m, rejecting sub-minute and non-duration values.
func TestAgentSourceValidateInterval(t *testing.T) {
	for _, ok := range []string{"1m", "1h", "15m", "24h"} {
		if err := Validate(KeyAgentSourceInterval, ok); err != nil {
			t.Errorf("Validate(agent_source_interval, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "0", "0s", "30s", "59s", "banana", "-5m"} {
		if err := Validate(KeyAgentSourceInterval, bad); err == nil {
			t.Errorf("Validate(agent_source_interval, %q) = nil, want a floor/format rejection", bad)
		}
	}
}

// TestAgentSourceValidateRepoURL pins the URL gate (PRD #602 M2): empty is allowed
// (the unconfigured value), a non-empty value must be an absolute https URL with a
// host. The allowlist check is NOT here (that is the handler's job) — this only
// enforces the format.
func TestAgentSourceValidateRepoURL(t *testing.T) {
	for _, ok := range []string{"", "https://git.example.com/roster.git", "https://host"} {
		if err := Validate(KeyAgentSourceRepoURL, ok); err != nil {
			t.Errorf("Validate(agent_source_repo_url, %q) = %v, want nil", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"http (not https)":    "http://git.example.com/roster.git",
		"ftp scheme":          "ftp://git.example.com",
		"no scheme":           "git.example.com/roster",
		"no host":             "https://",
		"userinfo token":      "https://tok3n@git.example.com/roster.git",
		"userinfo user:pass":  "https://user:pass@git.example.com/x.git",
		"leading whitespace":  " https://git.example.com/x.git",
		"trailing whitespace": "https://git.example.com/x.git ",
	} {
		if err := Validate(KeyAgentSourceRepoURL, bad); err == nil {
			t.Errorf("%s: Validate(agent_source_repo_url, %q) = nil, want a rejection", name, bad)
		}
	}
}

// TestAgentSourceValidateCredential pins the sealed-credential gate (PRD #602 M2):
// a real GitHub fine-grained PAT (~93 chars, over the 64-char label cap) MUST be
// accepted, so the credential does not inherit the ValidateLabel default branch;
// empty/whitespace-only and any embedded whitespace/control char are rejected. The
// error never echoes the value.
func TestAgentSourceValidateCredential(t *testing.T) {
	oks := []string{
		"ghp_" + strings.Repeat("a", 36),                 // classic PAT
		"github_pat_" + strings.Repeat("A1b2", 20) + "c", // fine-grained PAT, ~92 chars
		"glpat-" + strings.Repeat("z", 20),               // GitLab PAT
		"tok,en,with,commas",                             // commas allowed (unlike label)
	}
	for _, ok := range oks {
		if err := Validate(KeyAgentSourceCredential, ok); err != nil {
			t.Errorf("Validate(agent_source_credential, len=%d) = %v, want nil", len(ok), err)
		}
	}
	rejects := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"embedded space":   "tok en",
		"embedded tab":     "tok\ten",
		"embedded newline": "tok\nen",
		"control char":     "tok\x00en",
		"too long":         strings.Repeat("x", maxAgentSourceCredentialLen+1),
	}
	for name, bad := range rejects {
		err := Validate(KeyAgentSourceCredential, bad)
		if err == nil {
			t.Errorf("%s: Validate(agent_source_credential) = nil, want a rejection", name)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: error echoes the credential value: %v", name, err)
		}
	}
}

// TestAgentSourceValidateRef pins the ref gate (PRD #602 M2): empty allowed; a
// non-empty value is a single token with no whitespace/control chars.
func TestAgentSourceValidateRef(t *testing.T) {
	for _, ok := range []string{"", "v1.2.3", "main", "abc123def456", "release/2026"} {
		if err := Validate(KeyAgentSourceRef, ok); err != nil {
			t.Errorf("Validate(agent_source_ref, %q) = %v, want nil", ok, err)
		}
	}
	rejects := map[string]string{
		"whitespace only":  "   ",
		"internal space":   "v1 2",
		"trailing newline": "v1\n",
		"tab":              "v1\tx",
		"too long":         strings.Repeat("x", maxAgentSourceRefLen+1),
	}
	for name, bad := range rejects {
		if err := Validate(KeyAgentSourceRef, bad); err == nil {
			t.Errorf("%s: Validate(agent_source_ref, %q) = nil, want a rejection", name, bad)
		}
	}
}

// TestAgentSourceValidateFolder pins the folder gate (PRD #702 M1, Decision 2):
// empty allowed (resolves to the default at read time); a clean repo-relative
// subpath passes (including a trailing-slash form, which the reader normalizes);
// a leading "/", a ".." segment, a URL/UNC/scheme shape, a control char, and an
// over-cap value are all rejected.
func TestAgentSourceValidateFolder(t *testing.T) {
	oks := []string{
		"",
		".claude/agents",
		"product-agents",
		"product-agents/", // trailing slash accepted; reader normalizes it away
		"a/b/c",
		"team-42_roles",
		"dir with spaces/x", // spaces are legal in a path segment (unlike a ref)
	}
	for _, ok := range oks {
		if err := Validate(KeyAgentSourceFolder, ok); err != nil {
			t.Errorf("Validate(agent_source_folder, %q) = %v, want nil", ok, err)
		}
	}
	rejects := map[string]string{
		"whitespace only": "   ",
		"leading slash":   "/etc/passwd",
		"parent segment":  "../secrets",
		"nested parent":   "a/../../b",
		"url scheme":      "https://evil.com/x",
		"unc path":        `\\server\share`,
		"scheme colon":    "file:foo",
		"control char":    "prod\x00agents",
		"too long":        strings.Repeat("x", maxAgentSourceRefLen+1),
	}
	for name, bad := range rejects {
		if err := Validate(KeyAgentSourceFolder, bad); err == nil {
			t.Errorf("%s: Validate(agent_source_folder, %q) = nil, want a rejection", name, bad)
		}
	}
}

// TestAgentSourceFolderReader pins the Cache reader (PRD #702 M1): an empty/unset
// (or whitespace-only) value resolves to DefaultAgentSourceFolder, and a configured
// value is returned with any single trailing slash trimmed.
func TestAgentSourceFolderReader(t *testing.T) {
	cases := map[string]string{
		"":                DefaultAgentSourceFolder,
		"   ":             DefaultAgentSourceFolder,
		"product-agents":  "product-agents",
		"product-agents/": "product-agents",
		"a/b/c":           "a/b/c",
	}
	for stored, want := range cases {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeyAgentSourceFolder, stored)}}, time.Minute)
		got, err := c.AgentSourceFolder(context.Background())
		if err != nil {
			t.Errorf("AgentSourceFolder(stored=%q) err = %v", stored, err)
			continue
		}
		if got != want {
			t.Errorf("AgentSourceFolder(stored=%q) = %q, want %q", stored, got, want)
		}
	}
	// Absent row (empty table) also resolves to the default.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.AgentSourceFolder(context.Background()); err != nil || got != DefaultAgentSourceFolder {
		t.Errorf("AgentSourceFolder(absent) = %q, %v; want %q", got, err, DefaultAgentSourceFolder)
	}
}

// TestAgentSourceValidateMergedEnableRequiresURLAndRef pins the cross-key rule (PRD
// #602 M2): enabling the source with an empty URL or ref is rejected; a fully
// specified enable-on state passes; and a disabled state with empty URL/ref is fine.
func TestAgentSourceValidateMergedEnableRequiresURLAndRef(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			KeyUziLabel:            "uzi",
			KeyAutopilotLabel:      "autopilot",
			KeyAgentSourceEnabled:  "false",
			KeyAgentSourceRepoURL:  "",
			KeyAgentSourceRef:      "",
			KeyAgentSourceInterval: "1h",
		}
	}

	// Disabled + empty URL/ref: fine (the feature is off).
	if err := ValidateMerged(base()); err != nil {
		t.Errorf("disabled source rejected: %v", err)
	}

	// Enabled but no URL: rejected.
	m := base()
	m[KeyAgentSourceEnabled] = "true"
	if err := ValidateMerged(m); err == nil {
		t.Error("enabled with empty URL accepted, want rejection")
	}

	// Enabled with URL but no ref: rejected.
	m = base()
	m[KeyAgentSourceEnabled] = "true"
	m[KeyAgentSourceRepoURL] = "https://git.example.com/roster.git"
	if err := ValidateMerged(m); err == nil {
		t.Error("enabled with empty ref accepted, want rejection")
	}

	// Enabled with both URL and ref: accepted.
	m = base()
	m[KeyAgentSourceEnabled] = "true"
	m[KeyAgentSourceRepoURL] = "https://git.example.com/roster.git"
	m[KeyAgentSourceRef] = "v1.0.0"
	if err := ValidateMerged(m); err != nil {
		t.Errorf("fully specified enable-on state rejected: %v", err)
	}

	// Whitespace-only URL does not satisfy "set" (TrimSpace guard).
	m = base()
	m[KeyAgentSourceEnabled] = "true"
	m[KeyAgentSourceRepoURL] = "   "
	m[KeyAgentSourceRef] = "v1.0.0"
	if err := ValidateMerged(m); err == nil {
		t.Error("enabled with whitespace-only URL accepted, want rejection")
	}
}

// TestAgentSourceCredentialIsSecret pins the credential as a SecretKeys member (PRD
// #602 M2): Known+IsSecret true, absent from Defaults, masked in every value read,
// and sealed on write via ValueForStorage.
func TestAgentSourceCredentialIsSecret(t *testing.T) {
	if !IsSecret(KeyAgentSourceCredential) {
		t.Error("IsSecret(agent_source_credential) = false, want true")
	}
	if !Known(KeyAgentSourceCredential) {
		t.Error("Known(agent_source_credential) = false, want true (secret keys are writable)")
	}
	if _, inDefaults := Defaults[KeyAgentSourceCredential]; inDefaults {
		t.Error("agent_source_credential must NOT be in Defaults (a secret must never leak through a value read)")
	}

	box := testBox(t)
	const plain = "ghp_agentsource_clone_token_9f8e7d"
	sealed, err := ValueForStorage(box, KeyAgentSourceCredential, plain)
	if err != nil {
		t.Fatalf("ValueForStorage(credential): %v", err)
	}
	if sealed == plain || strings.Contains(sealed, plain) {
		t.Fatal("credential stored in the clear")
	}
	if got, err := DecodeSecret(box, sealed); err != nil || got != plain {
		t.Fatalf("round-trip = %q, %v; want %q", got, err, plain)
	}

	// Structurally excluded from All() and AdminView.Values, reported configured.
	fs := &fakeStore{rows: []store.AppSetting{row(KeyAgentSourceCredential, sealed)}}
	c := New(fs, time.Minute)
	c.ConfigureSecrets(box, nil)
	all, err := c.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, present := all[KeyAgentSourceCredential]; present {
		t.Fatal("All() leaked the credential key into the value map")
	}
	view, err := c.AdminView(context.Background())
	if err != nil {
		t.Fatalf("AdminView: %v", err)
	}
	if _, present := view.Values[KeyAgentSourceCredential]; present {
		t.Fatal("AdminView.Values leaked the credential key")
	}
	if !view.Secrets[KeyAgentSourceCredential] {
		t.Error("configured credential should report Secrets[agent_source_credential]=true")
	}
	// Byte-level: neither the plaintext nor the sealed bytes appear in the read.
	viewJSON, _ := json.Marshal(view)
	allJSON, _ := json.Marshal(all)
	for _, blob := range []string{string(viewJSON), string(allJSON)} {
		if strings.Contains(blob, plain) || strings.Contains(blob, sealed) {
			t.Error("credential bytes appeared in a serialized settings read")
		}
	}
}

// TestAgentSourceKeysSurfaceInDefaults pins that the four non-secret keys are in
// Defaults with the documented fresh-install values (off, empty URL/ref, 1h).
func TestAgentSourceKeysSurfaceInDefaults(t *testing.T) {
	want := map[string]string{
		KeyAgentSourceRepoURL:  "",
		KeyAgentSourceRef:      "",
		KeyAgentSourceEnabled:  "false",
		KeyAgentSourceInterval: "1h",
	}
	for k, w := range want {
		got, ok := Defaults[k]
		if !ok {
			t.Errorf("Defaults missing %s", k)
			continue
		}
		if got != w {
			t.Errorf("Defaults[%s] = %q, want %q", k, got, w)
		}
	}
}
