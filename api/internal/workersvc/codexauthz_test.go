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

// TestClaimCodexSecretsShape proves a Codex-bound claim carries ONLY the auth mode, the
// access token, the per-claim capability and (subscription only) the committed generation
// — never a refresh/login blob or an id. It also proves the two discriminated arms match
// the TS wire union EXACTLY (agent/src/protocol.ts): subscription carries a REAL
// "generation" (0 is a wire-visible zero), api_key omits the key entirely.
func TestClaimCodexSecretsShape(t *testing.T) {
	// The one placeholder whose name trips gosec G101 is extracted so its suppression
	// sits on the exact triggering assignment, not on the composite-literal brace (which
	// would blanket-silence gosec for every field in the literal, hiding a future one).
	forgePAT := "PAT-PLACEHOLDER"
	anthropicToken := "ANTHROPIC-OAUTH-PLACEHOLDER" //nolint:gosec // G101: placeholder fixture string, not a real credential
	codexAccess := "CODEX-ACCESS-PLACEHOLDER"
	codexCap := formatCodexCapability(3, "cap-secret-placeholder")

	// The forbidden set: neither the refresh/login blob nor any id may ride the wire, in
	// either arm. `login` matches nothing in `auth_mode` (no "login" substring), so it stays
	// a clean canary for a leaked login blob.
	forbidden := []string{"refresh_token", "refresh", "login", "secret_id", "account_id"}
	assertNoForbidden := func(t *testing.T, got string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(got, f) {
				t.Fatalf("forbidden field %q leaked into codex claim: %s", f, got)
			}
		}
	}

	t.Run("subscription carries a real generation (0 is visible)", func(t *testing.T) {
		gen := int64(0) // a real zero — must serialize as "generation":0, never omitted
		cs := ClaimSecrets{
			ForgeUsername:       "bot",
			ForgePAT:            forgePAT,
			AnthropicOAuthToken: anthropicToken,
			Codex: &ClaimCodexSecrets{
				AuthMode:    codexAuthModeSubscription,
				AccessToken: codexAccess,
				Capability:  codexCap,
				Generation:  &gen,
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
		if !strings.Contains(got, `"auth_mode":"subscription"`) {
			t.Fatalf("auth_mode missing/wrong: %s", got)
		}
		if !strings.Contains(got, `"access_token":"CODEX-ACCESS-PLACEHOLDER"`) {
			t.Fatalf("access_token missing: %s", got)
		}
		if !strings.Contains(got, `"capability":"3.cap-secret-placeholder"`) {
			t.Fatalf("capability missing: %s", got)
		}
		if !strings.Contains(got, `"generation":0`) {
			t.Fatalf("subscription generation 0 must be a REAL wire-visible zero: %s", got)
		}
		assertNoForbidden(t, got)
	})

	t.Run("subscription non-zero generation", func(t *testing.T) {
		gen := int64(7)
		cs := ClaimSecrets{
			ForgePAT: forgePAT,
			Codex: &ClaimCodexSecrets{
				AuthMode:    codexAuthModeSubscription,
				AccessToken: codexAccess,
				Capability:  codexCap,
				Generation:  &gen,
			},
		}
		b, err := json.Marshal(cs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := string(b); !strings.Contains(got, `"generation":7`) {
			t.Fatalf("generation 7 missing: %s", got)
		}
	})

	t.Run("api_key omits generation entirely", func(t *testing.T) {
		cs := ClaimSecrets{
			ForgePAT: forgePAT,
			Codex: &ClaimCodexSecrets{
				AuthMode:    codexAuthModeAPIKey,
				AccessToken: codexAccess,
				Capability:  codexCap,
				// Generation left nil: omitempty must drop the key for the api_key arm, which
				// can never refresh. Its PRESENCE (even as 0) would violate the TS union.
			},
		}
		b, err := json.Marshal(cs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := string(b)
		if !strings.Contains(got, `"auth_mode":"api_key"`) {
			t.Fatalf("auth_mode missing/wrong: %s", got)
		}
		if strings.Contains(got, "generation") {
			t.Fatalf("api_key claim must carry NO generation key: %s", got)
		}
		assertNoForbidden(t, got)
	})
}
