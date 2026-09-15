package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1247 M4: `uzi run set-token` — the parked/queued-state per-run credential switch.
// These pin the CLI's positional-label + mode-flag mapping through the FakeClient: a label
// resolves CLIENT-SIDE to {pinned, secret_id}; --auto/--default/--inherit ride as a bare
// mode; an unknown label is refused before any request; and the exactly-one / mutual-
// exclusion guard (D12) is enforced client-side.

// TestRunSetTokenLabelResolvesToPinned: a token LABEL resolves client-side (ListSecrets +
// findSecretByLabel) to {pinned, secret_id:<id>}, so the server receives an id, not a label.
func TestRunSetTokenLabelResolvesToPinned(t *testing.T) {
	fc := &uzicli.FakeClient{
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets: []apitypes.SecretDTO{
			{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"},
			{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"},
		},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "prod-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenRunID != "run-9" {
		t.Errorf("run id = %q, want run-9", fc.LastSetTokenRunID)
	}
	if fc.LastSetTokenOverride == nil {
		t.Fatal("set-token <label> sent no override")
	}
	if fc.LastSetTokenOverride.Mode != "pinned" {
		t.Errorf("mode = %q, want pinned", fc.LastSetTokenOverride.Mode)
	}
	if fc.LastSetTokenOverride.SecretID != "sec-b" {
		t.Errorf("secret_id = %q, want the resolved id sec-b (not the label)", fc.LastSetTokenOverride.SecretID)
	}
}

// TestRunSetTokenKeywords: --auto/--default/--inherit ride as a bare mode with NO secret and
// resolve WITHOUT a ListSecrets round-trip (a keyword never touches the server for a label).
func TestRunSetTokenKeywords(t *testing.T) {
	for _, mode := range []string{"auto", "default", "inherit"} {
		fc := &uzicli.FakeClient{SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		_, _, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "--"+mode)
		if code != uzicli.ExitOK {
			t.Fatalf("--%s: exit = %d, want 0", mode, code)
		}
		if fc.LastSetTokenOverride == nil {
			t.Fatalf("--%s sent no override", mode)
		}
		if fc.LastSetTokenOverride.Mode != mode {
			t.Errorf("--%s: mode = %q, want %q", mode, fc.LastSetTokenOverride.Mode, mode)
		}
		if fc.LastSetTokenOverride.SecretID != "" {
			t.Errorf("--%s: secret_id = %q, want empty (a keyword carries no secret)", mode, fc.LastSetTokenOverride.SecretID)
		}
	}
}

// TestRunSetTokenUnknownLabelRefusedClientSide: an unknown label is a CLIENT-SIDE usage
// error naming the read command, and NO request is sent (SetRunCredential never runs, so
// LastSetTokenRunID stays empty).
func TestRunSetTokenUnknownLabelRefusedClientSide(t *testing.T) {
	fc := &uzicli.FakeClient{
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets:     []apitypes.SecretDTO{{ID: "sec-a", Kind: "anthropic_token", Label: "default-key"}},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "no-such-token")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastSetTokenRunID != "" {
		t.Errorf("an unknown label must NOT send a request, but SetRunCredential ran (run=%q)", fc.LastSetTokenRunID)
	}
	if !containsAll(errb, "no Anthropic token", "uzi token list") {
		t.Errorf("refusal should name the read command; got: %q", errb)
	}
}

// TestRunSetTokenWrongKindLabelNamesKind: a label carried only by a NON-anthropic secret is
// refused naming that secret's kind, still client-side with no request sent.
func TestRunSetTokenWrongKindLabelNamesKind(t *testing.T) {
	fc := &uzicli.FakeClient{
		SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		Secrets:     []apitypes.SecretDTO{{ID: "sec-c", Kind: "openai_api_key", Label: "shared-label"}},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "shared-label")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastSetTokenRunID != "" {
		t.Errorf("a wrong-kind label must NOT send a request, but SetRunCredential ran (run=%q)", fc.LastSetTokenRunID)
	}
	if !containsAll(errb, "only Anthropic tokens") {
		t.Errorf("refusal should say the override is Anthropic-only; got: %q", errb)
	}
}

// TestRunSetTokenNoChoiceIsUsageError: neither a label nor a mode flag is a client-side
// usage error (exactly one is required, D12), and no request is sent.
func TestRunSetTokenNoChoiceIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastSetTokenRunID != "" {
		t.Errorf("a no-choice set-token must NOT send a request, but SetRunCredential ran (run=%q)", fc.LastSetTokenRunID)
	}
	if !containsAll(errb, "exactly one") {
		t.Errorf("refusal should say exactly one is required; got: %q", errb)
	}
}

// TestRunSetTokenMutuallyExclusive: a label AND a mode flag together (or two mode flags) is
// a client-side usage error (mutually exclusive, D12), and no request is sent.
func TestRunSetTokenMutuallyExclusive(t *testing.T) {
	cases := [][]string{
		{"run", "set-token", "run-9", "prod-key", "--auto"},
		{"run", "set-token", "run-9", "--auto", "--default"},
		{"run", "set-token", "run-9", "--default", "--inherit"},
	}
	for _, args := range cases {
		fc := &uzicli.FakeClient{
			SetTokenRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
			Secrets:     []apitypes.SecretDTO{{ID: "sec-b", Kind: "anthropic_token", Label: "prod-key"}},
		}
		_, errb, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (usage)", args, code, uzicli.ExitUsage)
		}
		if fc.LastSetTokenRunID != "" {
			t.Errorf("%v: mutually-exclusive args must NOT send a request, but SetRunCredential ran", args)
		}
		if !containsAll(errb, "mutually exclusive") {
			t.Errorf("%v: refusal should say mutually exclusive; got: %q", args, errb)
		}
	}
}

// TestRunSetTokenPrintsWarning: a D6 warning on the 200 is printed to the user (above the
// run detail) without the command failing.
func TestRunSetTokenPrintsWarning(t *testing.T) {
	// Assigned via a local so gosec G101 does not read a string literal on a Token-named
	// field as a hardcoded credential (it is a D6 warning message, not a secret).
	warn := "auto has no pooled token with headroom right now; the run will hold in pool_wait until one is available"
	fc := &uzicli.FakeClient{
		SetTokenRun:     apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"},
		SetTokenWarning: warn,
	}
	outb, _, code := runCLI(t, fakeEnv(fc), "run", "set-token", "run-9", "--auto")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !containsAll(outb, "warning", "pool_wait") {
		t.Errorf("a D6 warning should be printed; got stdout: %q", outb)
	}
}
