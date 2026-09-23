package workersvc

import (
	"testing"
)

// harness_resolver_settings_livedb_test.go proves ResolveSettingsHarness (PRD #1551 M1 / D3):
// the settings-surface wrapper over the D11 resolver that maps a credential-unavailable
// refusal to (Claude, nil) so settings GET/PUT never fail for want of a usable credential,
// while still honouring a usable stored default and a sole usable harness. It reuses the
// codexTestEnv harness (shared throwaway DB). Every test name ends LiveDB.

// Zero credentials must resolve to Claude, NOT the errNoUsableCredential the run-creation
// resolver returns — this is the whole reason the settings wrapper exists.
func TestResolveSettingsHarnessZeroCredentialsClaudeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	h, err := harnessSvc(env).ResolveSettingsHarness(env.ctx, userID)
	if err != nil {
		t.Fatalf("ResolveSettingsHarness: %v (want no error — settings must never fail on credentials)", err)
	}
	if h != HarnessClaude {
		t.Fatalf("harness = %q, want claude for a zero-credential user", h)
	}
}

// Claude-only resolves to Claude.
func TestResolveSettingsHarnessClaudeOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)

	h, err := harnessSvc(env).ResolveSettingsHarness(env.ctx, userID)
	if err != nil {
		t.Fatalf("ResolveSettingsHarness: %v", err)
	}
	if h != HarnessClaude {
		t.Fatalf("harness = %q, want claude", h)
	}
}

// Codex-only (a linked subscription default, no Anthropic token) resolves to Codex.
func TestResolveSettingsHarnessCodexOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	h, err := harnessSvc(env).ResolveSettingsHarness(env.ctx, userID)
	if err != nil {
		t.Fatalf("ResolveSettingsHarness: %v", err)
	}
	if h != HarnessCodex {
		t.Fatalf("harness = %q, want codex", h)
	}
}

// Both usable AND default_harness=codex: the usable stored default is honoured (settings
// projects the Codex lane), unlike a hand-coded "always claude".
func TestResolveSettingsHarnessUsableDefaultHonouredLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)
	setDefaultHarness(t, env, userID, string(HarnessCodex))

	h, err := harnessSvc(env).ResolveSettingsHarness(env.ctx, userID)
	if err != nil {
		t.Fatalf("ResolveSettingsHarness: %v", err)
	}
	if h != HarnessCodex {
		t.Fatalf("harness = %q, want codex (usable stored default honoured)", h)
	}
}
