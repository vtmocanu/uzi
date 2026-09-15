package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1247 M2: `uzi run create --token` — the create-time per-run Anthropic credential
// choice. These pin the CLI's three-way + label-resolution wire mapping through the
// FakeClient: a label resolves CLIENT-SIDE to {pinned, secret_id}, the keywords ride as a
// bare mode, an unknown label is refused before any request, and an omitted flag sends
// nothing (inherit).

// TestRunCreateTokenLabelResolvesToPinned: a token LABEL resolves client-side (ListSecrets +
// findSecretByLabel) to {pinned, secret_id:<id>}, so the server receives an id, not a label.
func TestRunCreateTokenLabelResolvesToPinned(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets: []apitypes.SecretDTO{
			{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"},
			{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"},
		},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--token", "prod-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateCredOverride == nil {
		t.Fatal("--token <label> sent no credential override")
	}
	if fc.LastCreateCredOverride.Mode != "pinned" {
		t.Errorf("mode = %q, want pinned", fc.LastCreateCredOverride.Mode)
	}
	if fc.LastCreateCredOverride.SecretID != "sec-b" {
		t.Errorf("secret_id = %q, want the resolved id sec-b (not the label)", fc.LastCreateCredOverride.SecretID)
	}
}

// TestRunCreateTokenLabelCaseInsensitive: findSecretByLabel matches case-insensitively (the
// server's unique index is on lower(label)), so a differently-cased label still resolves.
func TestRunCreateTokenLabelCaseInsensitive(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets:    []apitypes.SecretDTO{{ID: "sec-b", Kind: "anthropic_token", Label: "Prod-Key"}},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--token", "prod-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateCredOverride == nil || fc.LastCreateCredOverride.SecretID != "sec-b" {
		t.Fatalf("case-insensitive label did not resolve: %+v", fc.LastCreateCredOverride)
	}
}

// TestRunCreateTokenKeywords: auto/default/inherit ride as a bare mode with NO secret, and
// resolve without a ListSecrets round-trip (the keywords never touch the server for a label).
func TestRunCreateTokenKeywords(t *testing.T) {
	for _, mode := range []string{"auto", "default", "inherit"} {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--token", mode)
		if code != uzicli.ExitOK {
			t.Fatalf("--token %s: exit = %d, want 0", mode, code)
		}
		if fc.LastCreateCredOverride == nil {
			t.Fatalf("--token %s sent no credential override", mode)
		}
		if fc.LastCreateCredOverride.Mode != mode {
			t.Errorf("--token %s: mode = %q, want %q", mode, fc.LastCreateCredOverride.Mode, mode)
		}
		if fc.LastCreateCredOverride.SecretID != "" {
			t.Errorf("--token %s: secret_id = %q, want empty (a keyword carries no secret)", mode, fc.LastCreateCredOverride.SecretID)
		}
	}
}

// TestRunCreateTokenUnknownLabelRefusedClientSide: an unknown label is a CLIENT-SIDE usage
// error naming the read commands, and NO create request is sent (CreateRun never runs, so
// LastCreateRepoID stays empty).
func TestRunCreateTokenUnknownLabelRefusedClientSide(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets:    []apitypes.SecretDTO{{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"}},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--token", "no-such-token")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastCreateRepoID != "" {
		t.Errorf("an unknown label must NOT send a create request, but CreateRun ran (repo=%q)", fc.LastCreateRepoID)
	}
	if !containsAll(errb, "no Anthropic token", "uzi token list") {
		t.Errorf("refusal should name the read command; got: %q", errb)
	}
}

// TestRunCreateTokenWrongKindLabelNamesKind: a label carried only by a NON-anthropic secret is
// refused naming that secret's kind (a token override applies only to Anthropic tokens), still
// client-side with no request sent.
func TestRunCreateTokenWrongKindLabelNamesKind(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets:    []apitypes.SecretDTO{{ID: "sec-c", Kind: "openai_api_key", Label: "shared-label"}},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--token", "shared-label")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastCreateRepoID != "" {
		t.Errorf("a wrong-kind label must NOT send a create request, but CreateRun ran (repo=%q)", fc.LastCreateRepoID)
	}
	if !containsAll(errb, "only Anthropic tokens") {
		t.Errorf("refusal should say the override is Anthropic-only; got: %q", errb)
	}
}

// TestRunCreateTokenOmittedSendsNothing: no --token means no credential override is sent (the
// run inherits the worker binding, byte-identical to a pre-#1247 create).
func TestRunCreateTokenOmittedSendsNothing(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateCredOverride != nil {
		t.Errorf("an omitted --token must send no credential override, got %+v", fc.LastCreateCredOverride)
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
