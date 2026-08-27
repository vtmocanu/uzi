package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestNamedContextsEndToEndSmoke is the pure-local proof of the whole PRD #427
// feature (M5): it drives the REAL `uzi` write commands to store two credentials
// under two named contexts, then confirms the active-context resolution
// (resolveSettings) returns the right token under each selection mechanism —
// config.Current, the --context/-c flag, and $UZI_CONTEXT — and that $UZI_TOKEN
// still overrides all of them. Resolution is entirely client-side, so this needs
// no server, no network, and no browser; it exercises the write→resolve pipeline
// the struct-seeded unit tests do not.
func TestNamedContextsEndToEndSmoke(t *testing.T) {
	env := storeEnv(t, "")

	// The endpoint the default context stores (a URL a URL-less context inherits
	// per D3). https so it survives any URL validation a real client would apply.
	const serverURL = "https://uzi.example.com"
	if err := env.Store.SaveConfig(&uzicli.Config{
		Contexts: map[string]uzicli.Context{"default": {URL: serverURL}},
	}); err != nil {
		t.Fatal(err)
	}

	// Store the uzc_ owner token under the implicit "default" context and the
	// uza_ admin-read token under an "admin" context, both via `uzi auth token`
	// (the token is read from stdin, never argv). A fresh stdin reader per call.
	writeToken := func(token string, args ...string) {
		t.Helper()
		env.Stdin = strings.NewReader(token + "\n")
		if _, _, code := runCLI(t, env, append([]string{"auth", "token"}, args...)...); code != uzicli.ExitOK {
			t.Fatalf("auth token %v: exit %d, want 0", args, code)
		}
	}
	writeToken("uzc_ownertoken")                       // active context: default
	writeToken("uza_admintoken", "--context", "admin") // creates "admin"

	// Both tokens are now stored, distinctly, under the two contexts.
	if got, _ := env.Store.Resolve("default"); got.Token != "uzc_ownertoken" {
		t.Fatalf("default token = %q, want uzc_ownertoken", got.Token)
	}
	if got, _ := env.Store.Resolve("admin"); got.Token != "uza_admintoken" {
		t.Fatalf("admin token = %q, want uza_admintoken", got.Token)
	}

	// resolveTok resolves the active context under the given flags/env and returns
	// the resolved token, failing on any error.
	resolveTok := func(gf *globalFlags) (string, string) {
		t.Helper()
		s, err := resolveSettings(env, gf)
		if err != nil {
			t.Fatalf("resolveSettings: %v", err)
		}
		return s.Token, s.URL
	}

	// Baseline: nothing set → the implicit default → uzc_ owner token, with the
	// stored server URL.
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_TOKEN", "")
	t.Setenv("UZI_URL", "")
	if tok, url := resolveTok(&globalFlags{}); tok != "uzc_ownertoken" || url != serverURL {
		t.Errorf("default resolution = (%q, %q), want (uzc_ownertoken, %s)", tok, url, serverURL)
	}

	// 1) The --context/-c flag selects admin → uza_ token, and admin's URL is
	// inherited from default (D3), since admin stored none of its own.
	if tok, url := resolveTok(&globalFlags{context: "admin"}); tok != "uza_admintoken" || url != serverURL {
		t.Errorf("--context admin = (%q, %q), want (uza_admintoken, %s)", tok, url, serverURL)
	}

	// 2) $UZI_CONTEXT selects admin.
	t.Setenv("UZI_CONTEXT", "admin")
	if tok, _ := resolveTok(&globalFlags{}); tok != "uza_admintoken" {
		t.Errorf("$UZI_CONTEXT=admin token = %q, want uza_admintoken", tok)
	}
	t.Setenv("UZI_CONTEXT", "")

	// 3) The sticky current context, set by `uzi context use admin`, selects admin.
	if _, _, code := runCLI(t, env, "context", "use", "admin"); code != uzicli.ExitOK {
		t.Fatalf("context use admin: exit %d, want 0", code)
	}
	if tok, _ := resolveTok(&globalFlags{}); tok != "uza_admintoken" {
		t.Errorf("sticky current=admin token = %q, want uza_admintoken", tok)
	}
	// The flag still beats the sticky current (precedence).
	if tok, _ := resolveTok(&globalFlags{context: "default"}); tok != "uzc_ownertoken" {
		t.Errorf("--context default over sticky admin = %q, want uzc_ownertoken", tok)
	}

	// 4) $UZI_TOKEN overrides whatever the context resolved to (D2), no matter the
	// selection mechanism — here the sticky current is still admin.
	t.Setenv("UZI_TOKEN", "uzc_envoverride")
	if tok, _ := resolveTok(&globalFlags{}); tok != "uzc_envoverride" {
		t.Errorf("$UZI_TOKEN override = %q, want uzc_envoverride", tok)
	}
	if tok, _ := resolveTok(&globalFlags{context: "admin"}); tok != "uzc_envoverride" {
		t.Errorf("$UZI_TOKEN override under -c admin = %q, want uzc_envoverride", tok)
	}
}
