package main

import (
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTokenList(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, CreatedAt: time.Unix(1784000000, 0)},
		{ID: "s2", Kind: "anthropic_token", Label: "console-key", IsDefault: false, CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "default") || !strings.Contains(out, "console-key") {
		t.Errorf("token list output missing labels: %q", out)
	}
	// The table marks the default.
	if !strings.Contains(out, "true") {
		t.Errorf("token list should show the default flag: %q", out)
	}
}

func TestTokenListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"label": "default"`) || !strings.Contains(out, `"is_default": true`) {
		t.Errorf("token list --json missing fields: %q", out)
	}
	// The value must never appear in any CLI output — there is no value field at all.
	if strings.Contains(out, "ciphertext") || strings.Contains(out, "sealed") {
		t.Fatalf("token list leaked a value-ish field: %q", out)
	}
}

// The token command tree carries `list` and `pool`, and NOTHING that mints or
// replaces a credential: add/rename/set-default/rm are cookie-only web actions
// (PRD #104 D8) and must not exist as CLI commands, the same way `uzi worker` has
// no `create`.
//
// This test used to be named TestTokenHasOnlyList and its comment said the tree
// carried "ONLY list". That was true and is not any more — `pool` (PRD #111 M2,
// D13) is a write, and it is legitimate here for a reason the old wording could not
// express: the rule was never "no writes", it was "nothing that MINTS or REPLACES a
// credential". Toggling pool membership re-points spend among tokens the caller
// already holds; a stolen uzc_ gains nothing from it that it did not already have.
// The forbidden list below is the durable half and is unchanged.
func TestTokenSubcommands(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	tok := findCmd(root, "token")
	if tok == nil {
		t.Fatal("missing `uzi token` command")
	}
	subs := map[string]bool{}
	for _, c := range tok.Commands() {
		subs[c.Name()] = true
	}
	for _, want := range []string{"list", "pool"} {
		if !subs[want] {
			t.Errorf("`uzi token %s` is missing", want)
		}
	}
	for _, forbidden := range []string{"add", "rename", "set-default", "rm", "create", "delete"} {
		if subs[forbidden] {
			t.Errorf("`uzi token %s` exists, but minting or replacing a credential is web-only (D8)", forbidden)
		}
	}
}

// --- PRD #111 M2: the auto-selection pool toggle ---------------------------

func poolFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, AutoEligible: false, CreatedAt: time.Unix(1784000000, 0)},
			{ID: "s2", Kind: "anthropic_token", Label: "console-key", IsDefault: false, AutoEligible: true, CreatedAt: time.Unix(1784000000, 0)},
		},
		PoolSecret: apitypes.SecretDTO{ID: "s2", Kind: "anthropic_token", Label: "console-key", AutoEligible: true},
	}
}

// TestTokenPoolResolvesLabelToID pins that the LABEL is resolved client-side, to an
// id, before anything is sent. That is the whole shape of the command: the server
// route is keyed on the secret id, and a label posted to it would be a second,
// server-side resolution nobody wrote.
func TestTokenPoolResolvesLabelToID(t *testing.T) {
	fc := poolFake()
	_, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "console-key", "--on")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s2" {
		t.Errorf("sent secret id %q, want s2 (the label must be resolved to an id client-side)", fc.LastPoolSecretID)
	}
	if !fc.LastPoolValue {
		t.Error("--on must send auto_eligible=true")
	}
}

// Case-insensitively, matching the unique index 00077 put on
// (user_id, kind, lower(label)) — `Console-Key` and `console-key` are the same
// token everywhere else, so they must be here too.
func TestTokenPoolLabelIsCaseInsensitive(t *testing.T) {
	fc := poolFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "CONSOLE-KEY", "--off"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s2" {
		t.Errorf("sent %q, want s2 — label matching must be case-insensitive", fc.LastPoolSecretID)
	}
	if fc.LastPoolValue {
		t.Error("--off must send auto_eligible=false")
	}
}

// TestTokenPoolRequiresExactlyOneDirection: neither flag and both flags are usage
// errors, and NOTHING is sent in either case. Defaulting one way would make a bare
// `uzi token pool console-key` a spend decision the user never expressed.
func TestTokenPoolRequiresExactlyOneDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"neither", []string{"token", "pool", "console-key"}},
		{"both", []string{"token", "pool", "console-key", "--on", "--off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := poolFake()
			_, _, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
			}
			if fc.LastPoolSecretID != "" {
				t.Errorf("a usage error still sent a write for %q", fc.LastPoolSecretID)
			}
		})
	}
}

// TestTokenPoolUnknownLabelIsUsageError also EXECUTES the `uzi token list` remedy
// this command prints — the instruction knownInstructions attributes to this test.
// The assertion is the outcome pair: exit 3 (usage, not a 404 — the label never
// reached the server) and no write attempted.
func TestTokenPoolUnknownLabelIsUsageError(t *testing.T) {
	fc := poolFake()
	_, errOut, code := runCLI(t, fakeEnv(fc), "token", "pool", "no-such-token", "--on")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "uzi token list") {
		t.Errorf("the error should name the read that lists the valid labels, got: %q", errOut)
	}
	if fc.LastPoolSecretID != "" {
		t.Errorf("an unresolvable label still sent a write for %q", fc.LastPoolSecretID)
	}
}

// The POOL column is the OPT-IN, and `list` must show both values — a column that
// only ever printed one would be indistinguishable from a broken one.
func TestTokenListShowsPoolColumn(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(poolFake()), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "POOL") {
		t.Errorf("token list is missing the POOL column: %q", out)
	}
	if !strings.Contains(out, "true") || !strings.Contains(out, "false") {
		t.Errorf("token list should render both pool values: %q", out)
	}
}

// The label reaches a terminal through cellText, like every other user-authored
// cell: validateSecretLabel permits unicode.Cf (bidi overrides) and
// uzicli.Printer.Table does not sanitize what it is handed.
func TestTokenListSanitizesLabel(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "safe‮dnetsop\x1b[31m", CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, bad := range []string{"‮", "\x1b"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile label reached the terminal carrying %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") {
		t.Errorf("sanitizing dropped the printable text too: %q", out)
	}
}
