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

// The token command tree carries ONLY `list`: add/rename/set-default/rm are
// cookie-only web actions (D8) and must not exist as CLI commands, the same way
// `uzi worker` has no `create`.
func TestTokenHasOnlyList(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	tok := findCmd(root, "token")
	if tok == nil {
		t.Fatal("missing `uzi token` command")
	}
	subs := map[string]bool{}
	for _, c := range tok.Commands() {
		subs[c.Name()] = true
	}
	if !subs["list"] {
		t.Error("`uzi token list` is missing")
	}
	for _, forbidden := range []string{"add", "rename", "set-default", "rm", "create", "delete"} {
		if subs[forbidden] {
			t.Errorf("`uzi token %s` exists, but token writes are web-only (D8)", forbidden)
		}
	}
}
