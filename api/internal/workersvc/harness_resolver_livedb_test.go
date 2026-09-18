package workersvc

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
)

// harness_resolver_livedb_test.go proves the store-backed availability + Codex
// credential-selection layer (PRD #1332 D4) against a REAL Postgres, reusing the
// codexTestEnv harness from codexcred_livedb_test.go / codexauthz_livedb_test.go. It
// covers the credential matrix the pure resolver takes as input: no credentials,
// Claude-only, Codex-only (subscription and api-key), both, a set-but-unusable default,
// a missing named credential, the "failed subscription never spends an API key" rule,
// and canonical duplicate imports (D6: no run-long lock). Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.
// Every test name ends in LiveDB so the store-IT sweep selects it.

// seedAnthropicToken inserts one anthropic_token secret so UserHasAnthropicToken (which
// checks existence by kind, not default) reports the user holds a Claude credential. The
// ciphertext is never opened by the resolver, so a placeholder byte string suffices.
func seedAnthropicToken(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), []byte("ct"))
}

// makeCodexDefault promotes a codex alias into the user's single shared codex default
// slot. There is NO production writer for the default in M5A, so tests set it directly.
func makeCodexDefault(t *testing.T, env codexTestEnv, secretID uuid.UUID) {
	t.Helper()
	env.exec(`UPDATE user_secrets SET is_default = true WHERE id = $1`, secretID)
}

// setDefaultHarness writes users.default_harness directly — the only writer in M5A, per
// D4 (no public preference writer exists).
func setDefaultHarness(t *testing.T, env codexTestEnv, userID uuid.UUID, harness string) {
	t.Helper()
	env.exec(`UPDATE users SET default_harness = $2 WHERE id = $1`, userID, harness)
}

// harnessSvc builds the Service over the live queries; s.q is a *store.Queries, which
// satisfies the resolver's narrow read surface.
func harnessSvc(env codexTestEnv) *Service { return &Service{q: env.q, box: env.box} }

// TestResolveRunHarnessNoCredentialsLiveDB: a user with neither an Anthropic token nor a
// Codex credential resolves to the neither-usable refusal.
func TestResolveRunHarnessNoCredentialsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if !errors.Is(err, errNoUsableCredential) {
		t.Fatalf("err = %v (res=%+v), want errNoUsableCredential", err, res)
	}
	// Existing refusal behavior is preserved via the wrap.
	if !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("err = %v, want it to also satisfy errCredentialUnavailable", err)
	}
}

// TestResolveRunHarnessClaudeOnlyLiveDB: an Anthropic token and no Codex credential
// resolves to Claude, carrying no Codex selection.
func TestResolveRunHarnessClaudeOnlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessClaude {
		t.Fatalf("harness = %q, want claude", res.Harness)
	}
	if res.Codex != nil {
		t.Fatalf("a Claude run must carry no Codex selection, got %+v", res.Codex)
	}
}

// TestResolveRunHarnessCodexOnlySubscriptionLiveDB: a linked subscription default and no
// Anthropic token resolves to Codex with the subscription auth mode and that alias.
func TestResolveRunHarnessCodexOnlySubscriptionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessCodex {
		t.Fatalf("harness = %q, want codex", res.Harness)
	}
	if res.Codex == nil {
		t.Fatal("a Codex run must carry a Codex selection")
	}
	if res.Codex.AuthMode != codexAuthModeSubscription {
		t.Fatalf("auth mode = %q, want subscription", res.Codex.AuthMode)
	}
	if res.Codex.SecretID != alias {
		t.Fatalf("selected secret = %s, want the default subscription alias %s", res.Codex.SecretID, alias)
	}
}

// TestResolveRunHarnessCodexOnlyAPIKeyLiveDB: a static api-key default and no Anthropic
// token resolves to Codex with the api_key auth mode.
func TestResolveRunHarnessCodexOnlyAPIKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	alias := env.seedStaticAPIKey(t, userID, "codex-key", codexToken("sk"))
	makeCodexDefault(t, env, alias)

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessCodex {
		t.Fatalf("harness = %q, want codex", res.Harness)
	}
	if res.Codex == nil || res.Codex.AuthMode != codexAuthModeAPIKey {
		t.Fatalf("codex selection = %+v, want api_key auth mode", res.Codex)
	}
	if res.Codex.SecretID != alias {
		t.Fatalf("selected secret = %s, want the default api-key alias %s", res.Codex.SecretID, alias)
	}
}

// TestResolveRunHarnessBothUsableNoDefaultLiveDB: both Claude and a linked-subscription
// Codex are usable and no default_harness is set, so the tiebreak chooses Claude.
func TestResolveRunHarnessBothUsableNoDefaultLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessClaude {
		t.Fatalf("harness = %q, want claude (both usable, no default -> claude)", res.Harness)
	}
	if res.Codex != nil {
		t.Fatalf("a Claude run must carry no Codex selection, got %+v", res.Codex)
	}
}

// TestResolveRunHarnessDefaultCodexWinsLiveDB: both usable AND default_harness=codex, so
// the usable default wins the tiebreak and Codex is chosen.
func TestResolveRunHarnessDefaultCodexWinsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)
	alias := env.seedLinkedSubscription(t, userID, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)
	setDefaultHarness(t, env, userID, string(HarnessCodex))

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessCodex {
		t.Fatalf("harness = %q, want codex (usable default wins the tiebreak)", res.Harness)
	}
	if res.Codex == nil || res.Codex.SecretID != alias {
		t.Fatalf("codex selection = %+v, want the default subscription alias %s", res.Codex, alias)
	}
}

// TestResolveRunHarnessUnusableDefaultFallsThroughLiveDB: default_harness=codex but the
// codex credential is an UNLINKED (staging) subscription, so Codex is not usable and the
// set-but-unusable default falls through to the sole usable harness, Claude.
func TestResolveRunHarnessUnusableDefaultFallsThroughLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)
	seedAnthropicToken(t, env, userID)
	// A staging (never reconciled) subscription is present-but-unusable.
	alias := env.seedStagingAlias(t, userID, "codex-staging", codexLoginBlob{AccessToken: codexToken("a"), RefreshToken: codexToken("r")})
	makeCodexDefault(t, env, alias)
	setDefaultHarness(t, env, userID, string(HarnessCodex))

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if err != nil {
		t.Fatalf("resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessClaude {
		t.Fatalf("harness = %q, want claude (set-but-unusable codex default falls through)", res.Harness)
	}
	if res.Codex != nil {
		t.Fatalf("a Claude run must carry no Codex selection, got %+v", res.Codex)
	}
}

// TestResolveRunHarnessMissingNamedCredentialLiveDB: default_harness=codex but the user
// holds NO codex credential at all. Codex is not usable, so the default falls through to
// Claude; with no Claude either it is the neither-usable refusal.
func TestResolveRunHarnessMissingNamedCredentialLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	// (a) missing codex credential but Claude present -> falls through to Claude.
	userA := env.seedUser(t)
	seedAnthropicToken(t, env, userA)
	setDefaultHarness(t, env, userA, string(HarnessCodex))
	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userA, nil)
	if err != nil {
		t.Fatalf("(a) resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessClaude || res.Codex != nil {
		t.Fatalf("(a) result = %+v, want claude with no codex selection", res)
	}

	// (b) missing codex credential and no Claude -> neither-usable refusal.
	userB := env.seedUser(t)
	setDefaultHarness(t, env, userB, string(HarnessCodex))
	_, err = harnessSvc(env).resolveRunHarness(env.ctx, userB, nil)
	if !errors.Is(err, errNoUsableCredential) {
		t.Fatalf("(b) err = %v, want errNoUsableCredential", err)
	}
}

// TestResolveRunHarnessFailedSubscriptionNeverSpendsAPIKeyLiveDB pins D4's sharpest rule:
// when the DEFAULT codex credential is an unusable (staging/failed) subscription, the
// resolver must NOT fall through to a separately-saved, non-default, usable API key. With
// no Anthropic token either, Codex is unusable and the whole resolve refuses — proving the
// API key is never silently spent behind a failed subscription.
func TestResolveRunHarnessFailedSubscriptionNeverSpendsAPIKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	// The DEFAULT is an unlinked subscription (unusable).
	sub := env.seedStagingAlias(t, userID, "codex-sub", codexLoginBlob{AccessToken: codexToken("a"), RefreshToken: codexToken("r")})
	makeCodexDefault(t, env, sub)
	// A perfectly usable api-key exists, but it is NOT the default.
	apiKey := env.seedStaticAPIKey(t, userID, "codex-key", codexToken("sk"))

	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userID, nil)
	if !errors.Is(err, errNoUsableCredential) {
		t.Fatalf("err = %v (res=%+v), want errNoUsableCredential — a failed subscription default must NOT spend the non-default API key %s", err, res, apiKey)
	}
}

// TestResolveRunHarnessCanonicalDuplicateImportsLiveDB (D6): two separately-saved imports
// of ONE canonical subscription account converge to a single provider account, make Codex
// usable, and are selectable CONCURRENTLY with no run-long lock — resolution issues only
// SELECTs, so two runs resolving at once both succeed. (The heavy overlapping-claim D6
// proof is C5's; this pins the resolver layer holds no lock.)
func TestResolveRunHarnessCanonicalDuplicateImportsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	// Two staging aliases whose discovery yields the SAME identity tuple → one account.
	tok1, tok2 := codexToken("access-1"), codexToken("access-2")
	alias1 := env.seedStagingAlias(t, userID, "codex-a", codexLoginBlob{AccessToken: tok1, RefreshToken: codexToken("r")})
	alias2 := env.seedStagingAlias(t, userID, "codex-b", codexLoginBlob{AccessToken: tok2, RefreshToken: codexToken("r")})
	fake := newFakeCodexIdentity()
	sameID := codexauth.Identity{ProviderUserID: "user-" + uuid.NewString(), WorkspaceAccountID: "acct-" + uuid.NewString()}
	fake.idByToken[tok1] = sameID
	fake.idByToken[tok2] = sameID
	r := NewCodexReconciler(env.q, nil, env.box, fake, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias1); err != nil {
		t.Fatalf("reconcile alias1: %v", err)
	}
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias2); err != nil {
		t.Fatalf("reconcile alias2: %v", err)
	}
	if n := env.countProviderAccounts(t, userID); n != 1 {
		t.Fatalf("provider accounts = %d, want 1 (duplicate imports converge)", n)
	}
	// One of the two imports is the default; both are linked and usable.
	makeCodexDefault(t, env, alias1)

	svc := harnessSvc(env)

	// Two concurrent resolutions (two runs) both succeed with Codex — no lock serializes
	// or blocks them.
	const n = 2
	var wg sync.WaitGroup
	results := make([]resolvedHarness, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.resolveRunHarness(env.ctx, userID, nil)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent resolve %d: %v", i, errs[i])
		}
		if results[i].Harness != HarnessCodex {
			t.Fatalf("concurrent resolve %d harness = %q, want codex", i, results[i].Harness)
		}
		if results[i].Codex == nil || results[i].Codex.SecretID != alias1 {
			t.Fatalf("concurrent resolve %d selection = %+v, want the default converged alias %s", i, results[i].Codex, alias1)
		}
	}
}

// TestResolveRunHarnessExplicitSelectionLiveDB pins the explicit path through the store
// layer: an explicit Codex with a usable subscription is honored, and an explicit Codex
// with only Claude usable REFUSES with no_credential_for_harness — no fallback to Claude.
func TestResolveRunHarnessExplicitSelectionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	// (a) explicit codex, usable -> codex.
	userA := env.seedUser(t)
	seedAnthropicToken(t, env, userA)
	alias := env.seedLinkedSubscription(t, userA, "codex-sub", codexToken("access"), codexToken("refresh"))
	makeCodexDefault(t, env, alias)
	res, err := harnessSvc(env).resolveRunHarness(env.ctx, userA, h(HarnessCodex))
	if err != nil {
		t.Fatalf("(a) resolveRunHarness: %v", err)
	}
	if res.Harness != HarnessCodex || res.Codex == nil {
		t.Fatalf("(a) result = %+v, want explicit codex honored", res)
	}

	// (b) explicit codex, Codex unusable but Claude usable -> refuse, NO fallback.
	userB := env.seedUser(t)
	seedAnthropicToken(t, env, userB)
	_, err = harnessSvc(env).resolveRunHarness(env.ctx, userB, h(HarnessCodex))
	if !errors.Is(err, errNoCredentialForHarness) {
		t.Fatalf("(b) err = %v, want errNoCredentialForHarness (explicit codex never falls back to claude)", err)
	}
}
