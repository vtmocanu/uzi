package workersvc

import (
	"crypto/subtle"
	"encoding/json"
	"strings"
	"testing"
)

// TestMintCodexCapabilityRoundTrip proves a minted capability's stored hash matches
// hashCodexCapability of its plaintext (so the authority check can constant-time
// compare), that two mints differ (high entropy), and that a wrong plaintext does not
// match.
func TestMintCodexCapabilityRoundTrip(t *testing.T) {
	plaintext, hash := mintCodexCapability()
	if plaintext == "" {
		t.Fatal("mint returned empty plaintext")
	}
	if len(hash) != 32 {
		t.Fatalf("hash length = %d, want 32 (sha256)", len(hash))
	}
	if subtle.ConstantTimeCompare(hash, hashCodexCapability(plaintext)) != 1 {
		t.Fatal("stored hash does not match hashCodexCapability(plaintext)")
	}
	// A different plaintext must not match — the whole point of storing only the hash.
	if subtle.ConstantTimeCompare(hash, hashCodexCapability(plaintext+"x")) == 1 {
		t.Fatal("hash matched a different plaintext")
	}
	// Two mints are distinct with overwhelming probability.
	other, _ := mintCodexCapability()
	if other == plaintext {
		t.Fatal("two mints produced the same plaintext")
	}
}

// TestCodexCapabilityWireFormat proves the epoch-tagged wire form round-trips through
// parseCodexCapability, and that malformed inputs are rejected.
func TestCodexCapabilityWireFormat(t *testing.T) {
	plaintext, _ := mintCodexCapability()
	wire := formatCodexCapability(7, plaintext)
	epoch, secret, ok := parseCodexCapability(wire)
	if !ok {
		t.Fatalf("parse of %q failed", wire)
	}
	if epoch != 7 {
		t.Fatalf("epoch = %d, want 7", epoch)
	}
	if secret != plaintext {
		t.Fatalf("secret = %q, want %q", secret, plaintext)
	}
	// hashing the parsed secret matches hashing the original plaintext.
	if subtle.ConstantTimeCompare(hashCodexCapability(secret), hashCodexCapability(plaintext)) != 1 {
		t.Fatal("parsed secret hashes differently from the original plaintext")
	}
	for _, bad := range []string{"", "noseparator", "notanumber.secret", "7."} {
		if _, _, ok := parseCodexCapability(bad); ok {
			t.Fatalf("parse of malformed %q unexpectedly succeeded", bad)
		}
	}
}

// TestCodexScopeIndependence proves the three scopes are distinct and are checked
// independently against the auth mode — holding one does not grant another. Concretely:
// an api_key run's credential can be RELEASED but cannot START-REFRESH or PERSIST-
// RECOVERY (a static key has no rotating login), so release authorization never implies
// the refresh/recovery scopes.
func TestCodexScopeIndependence(t *testing.T) {
	// Distinct enum values.
	scopes := []CodexOpScope{ScopePersistRecovery, ScopeReleaseAccessToken, ScopeStartRefresh}
	seen := map[CodexOpScope]bool{}
	for _, s := range scopes {
		if !s.valid() {
			t.Fatalf("scope %v reported invalid", s)
		}
		if seen[s] {
			t.Fatalf("duplicate scope value %v", s)
		}
		seen[s] = true
	}
	// The zero value is invalid so an unset scope authorizes nothing.
	if (CodexOpScope(0)).valid() {
		t.Fatal("zero-value scope must be invalid")
	}

	// api_key: release applies, refresh/recovery do not.
	if !ScopeReleaseAccessToken.appliesTo(codexAuthModeAPIKey) {
		t.Fatal("release must apply to api_key")
	}
	if ScopeStartRefresh.appliesTo(codexAuthModeAPIKey) {
		t.Fatal("start-refresh must NOT apply to api_key (release does not grant refresh)")
	}
	if ScopePersistRecovery.appliesTo(codexAuthModeAPIKey) {
		t.Fatal("persist-recovery must NOT apply to api_key")
	}

	// subscription: all three apply (a subscription has a rotating login), but they are
	// still separate scopes each caller must request explicitly.
	for _, s := range scopes {
		if !s.appliesTo(codexAuthModeSubscription) {
			t.Fatalf("scope %v must apply to subscription", s)
		}
	}
}

// TestClaimSecretsClaudeByteIdentical proves an ordinary Claude claim (Codex nil)
// marshals with NO `codex` key, so the wire stays byte-identical to before m2.
func TestClaimSecretsClaudeByteIdentical(t *testing.T) {
	anthropicToken := "ANTHROPIC-OAUTH-PLACEHOLDER" //nolint:gosec // G101: placeholder fixture string, not a real credential
	cs := ClaimSecrets{
		ForgeUsername:       "bot",
		ForgePAT:            "PAT-PLACEHOLDER",
		AnthropicOAuthToken: anthropicToken,
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "codex") {
		t.Fatalf("Claude claim secrets must not contain a codex key: %s", got)
	}
	want := `{"forge_username":"bot","forge_pat":"PAT-PLACEHOLDER","anthropic_oauth_token":"ANTHROPIC-OAUTH-PLACEHOLDER"}`
	if got != want {
		t.Fatalf("wire drift:\n got %s\nwant %s", got, want)
	}
}

// TestClaimCodexSecretsShape proves a Codex-bound claim carries ONLY the access token
// and the per-claim capability — never a refresh/login blob or an id.
func TestClaimCodexSecretsShape(t *testing.T) {
	// The one placeholder whose name trips gosec G101 is extracted so its suppression
	// sits on the exact triggering assignment, not on the composite-literal brace (which
	// would blanket-silence gosec for every field in the literal, hiding a future one).
	forgePAT := "PAT-PLACEHOLDER"
	anthropicToken := "ANTHROPIC-OAUTH-PLACEHOLDER" //nolint:gosec // G101: placeholder fixture string, not a real credential
	codexAccess := "CODEX-ACCESS-PLACEHOLDER"
	codexCap := formatCodexCapability(3, "cap-secret-placeholder")
	cs := ClaimSecrets{
		ForgeUsername:       "bot",
		ForgePAT:            forgePAT,
		AnthropicOAuthToken: anthropicToken,
		Codex: &ClaimCodexSecrets{
			AccessToken: codexAccess,
			Capability:  codexCap,
		},
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"codex":{`) {
		t.Fatalf("codex block missing: %s", got)
	}
	if !strings.Contains(got, `"access_token":"CODEX-ACCESS-PLACEHOLDER"`) {
		t.Fatalf("access_token missing: %s", got)
	}
	if !strings.Contains(got, `"capability":"3.cap-secret-placeholder"`) {
		t.Fatalf("capability missing: %s", got)
	}
	// The refresh/login blob and any secret id must NEVER appear.
	for _, forbidden := range []string{"refresh_token", "refresh", "login", "secret_id", "account_id"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("forbidden field %q leaked into codex claim: %s", forbidden, got)
		}
	}
}
