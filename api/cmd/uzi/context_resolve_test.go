package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// seedStore writes config + credentials into a fresh temp-dir store and returns
// an Env backed by it, so context resolution can be exercised end to end.
func seedStore(t *testing.T, cfg *uzicli.Config, creds *uzicli.Credentials) Env {
	t.Helper()
	store := uzicli.NewStore(t.TempDir())
	if cfg != nil {
		if err := store.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if creds != nil {
		if err := store.SaveCredentials(creds); err != nil {
			t.Fatal(err)
		}
	}
	env := storeEnv(t, "")
	env.Store = store
	return env
}

// D1 precedence: flag > $UZI_CONTEXT > config.Current > default. Each source
// stores a distinct token so resolveSettings' result pins which one won.
func TestResolveContextPrecedence(t *testing.T) {
	cfg := &uzicli.Config{
		Current: "current",
		Contexts: map[string]uzicli.Context{
			"default": {URL: "https://default.example"},
			"current": {URL: "https://current.example"},
			"env":     {URL: "https://env.example"},
			"flag":    {URL: "https://flag.example"},
		},
	}
	creds := &uzicli.Credentials{
		Contexts: map[string]uzicli.Credential{
			"default": {Token: "uzc_default"},
			"current": {Token: "uzc_current"},
			"env":     {Token: "uzc_env"},
			"flag":    {Token: "uzc_flag"},
		},
	}

	t.Run("flag beats env and current", func(t *testing.T) {
		env := seedStore(t, cfg, creds)
		t.Setenv("UZI_CONTEXT", "env")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		s, err := resolveSettings(env, &globalFlags{context: "flag"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Token != "uzc_flag" || s.URL != "https://flag.example" {
			t.Errorf("got %+v, want flag context", s)
		}
	})

	t.Run("env beats current", func(t *testing.T) {
		env := seedStore(t, cfg, creds)
		t.Setenv("UZI_CONTEXT", "env")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		s, err := resolveSettings(env, &globalFlags{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Token != "uzc_env" || s.URL != "https://env.example" {
			t.Errorf("got %+v, want env context", s)
		}
	})

	t.Run("current beats default", func(t *testing.T) {
		env := seedStore(t, cfg, creds)
		t.Setenv("UZI_CONTEXT", "")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		s, err := resolveSettings(env, &globalFlags{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Token != "uzc_current" || s.URL != "https://current.example" {
			t.Errorf("got %+v, want current context", s)
		}
	})

	t.Run("default when nothing set", func(t *testing.T) {
		noCurrent := &uzicli.Config{Contexts: cfg.Contexts}
		env := seedStore(t, noCurrent, creds)
		t.Setenv("UZI_CONTEXT", "")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		s, err := resolveSettings(env, &globalFlags{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Token != "uzc_default" || s.URL != "https://default.example" {
			t.Errorf("got %+v, want default context", s)
		}
	})
}

// D3: a context with a token but no URL inherits the default context's URL. The
// token never inherits.
func TestResolveContextURLInheritance(t *testing.T) {
	cfg := &uzicli.Config{
		Contexts: map[string]uzicli.Context{
			"default": {URL: "https://shared.example"},
			"admin":   {}, // no URL of its own
		},
	}
	creds := &uzicli.Credentials{
		Contexts: map[string]uzicli.Credential{
			"default": {Token: "uzc_owner"},
			"admin":   {Token: "uza_admin"},
		},
	}
	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	s, err := resolveSettings(env, &globalFlags{context: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://shared.example" {
		t.Errorf("URL = %q, want inherited default URL", s.URL)
	}
	if s.Token != "uza_admin" {
		t.Errorf("Token = %q, want uza_admin (token must not inherit)", s.Token)
	}
}

// D9: an explicit unknown context (present in neither map) is a usage error
// before the client is built. Cover both the flag and the env-var source.
func TestResolveUnknownExplicitContextErrors(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://default.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_default"}}}

	t.Run("via flag", func(t *testing.T) {
		env := seedStore(t, cfg, creds)
		t.Setenv("UZI_CONTEXT", "")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		_, err := resolveSettings(env, &globalFlags{context: "nope"})
		if err == nil {
			t.Fatal("want error for unknown explicit context")
		}
		if uzicli.ExitCodeFor(err) != uzicli.ExitUsage {
			t.Errorf("exit = %d, want ExitUsage", uzicli.ExitCodeFor(err))
		}
		if !strings.Contains(err.Error(), "unknown context") {
			t.Errorf("error = %q, want it to mention unknown context", err.Error())
		}
	})

	t.Run("via env", func(t *testing.T) {
		env := seedStore(t, cfg, creds)
		t.Setenv("UZI_CONTEXT", "nope")
		t.Setenv("UZI_URL", "")
		t.Setenv("UZI_TOKEN", "")
		_, err := resolveSettings(env, &globalFlags{})
		if err == nil {
			t.Fatal("want error for unknown explicit context")
		}
		if uzicli.ExitCodeFor(err) != uzicli.ExitUsage {
			t.Errorf("exit = %d, want ExitUsage", uzicli.ExitCodeFor(err))
		}
		if !strings.Contains(err.Error(), "unknown context") {
			t.Errorf("error = %q, want it to mention unknown context", err.Error())
		}
	})
}

// A context present in ONLY the credentials map (URL-less, token-only) is a
// known context and must not trip the D9 error.
func TestResolveExplicitContextKnownViaCredentialsOnly(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://default.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_default"},
		"admin":   {Token: "uza_admin"},
	}}
	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	s, err := resolveSettings(env, &globalFlags{context: "admin"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Token != "uza_admin" {
		t.Errorf("Token = %q, want uza_admin", s.Token)
	}
}

// A fresh user with an empty store, nothing set, resolves the implicit default
// without error (the explicit==false path must never fail on absence).
func TestResolveFreshUserImplicitDefault(t *testing.T) {
	env := storeEnv(t, "")
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	name, explicit, err := resolveContextName(env, &globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if name != "default" || explicit {
		t.Errorf("name=%q explicit=%v, want default/false", name, explicit)
	}
	s, err := resolveSettings(env, &globalFlags{})
	if err != nil {
		t.Fatalf("fresh user must resolve without error, got %v", err)
	}
	if s.URL != "" || s.Token != "" {
		t.Errorf("got %+v, want empty settings", s)
	}
}

// Empty $UZI_CONTEXT and -c "" are treated as unset and fall through to
// config.Current / default.
func TestResolveEmptyContextTreatedAsUnset(t *testing.T) {
	cfg := &uzicli.Config{
		Current:  "current",
		Contexts: map[string]uzicli.Context{"current": {URL: "https://current.example"}},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"current": {Token: "uzc_current"}}}

	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	s, err := resolveSettings(env, &globalFlags{context: ""})
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "uzc_current" {
		t.Errorf("Token = %q, want uzc_current (empty flag/env fell through to Current)", s.Token)
	}
}

// Back-compat: with nothing set, resolveSettings is byte-identical to
// Store.Resolve("default") plus the env/flag layering.
func TestResolveBackCompatDefault(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://default.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_default"}}}
	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	got, err := resolveSettings(env, &globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := env.Store.Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolveSettings = %+v, want Resolve(\"default\") = %+v", got, want)
	}
}

// The headless UZI_URL+UZI_TOKEN path (no store) must behave byte-identically:
// env overrides supply everything and no context error fires.
func TestResolveHeadlessEnvOnly(t *testing.T) {
	env := fakeEnv(&uzicli.FakeClient{}) // Store == nil
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "https://ci.example")
	t.Setenv("UZI_TOKEN", "uzc_ci")

	s, err := resolveSettings(env, &globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://ci.example" || s.Token != "uzc_ci" {
		t.Errorf("got %+v, want CI env settings", s)
	}
}

// $UZI_TOKEN and --url still override the resolved context (D2), applied after
// context resolution in the same order as before.
func TestResolveEnvFlagOverridesWinOverContext(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://default.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_default"}}}
	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "uzc_override")

	s, err := resolveSettings(env, &globalFlags{url: "https://flag.example"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "uzc_override" {
		t.Errorf("Token = %q, want $UZI_TOKEN override", s.Token)
	}
	if s.URL != "https://flag.example" {
		t.Errorf("URL = %q, want --url override", s.URL)
	}
}

// A config.Current naming a context absent from both maps must trip the D9
// error (explicit==true for the Current source): a hand-edited/typo'd sticky
// context should fail loudly, not silently resolve to a tokenless Settings.
func TestResolveUnknownCurrentErrors(t *testing.T) {
	cfg := &uzicli.Config{
		Current:  "ghost",
		Contexts: map[string]uzicli.Context{"default": {URL: "https://default.example"}},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_default"}}}
	env := seedStore(t, cfg, creds)
	t.Setenv("UZI_CONTEXT", "")
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	// The name resolves to the sticky Current and is marked explicit.
	name, explicit, err := resolveContextName(env, &globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if name != "ghost" || !explicit {
		t.Errorf("name=%q explicit=%v, want ghost/true", name, explicit)
	}

	_, err = resolveSettings(env, &globalFlags{})
	if err == nil {
		t.Fatal("want error for unknown config.Current context")
	}
	if uzicli.ExitCodeFor(err) != uzicli.ExitUsage {
		t.Errorf("exit = %d, want ExitUsage", uzicli.ExitCodeFor(err))
	}
	if !strings.Contains(err.Error(), "unknown context") {
		t.Errorf("error = %q, want it to mention unknown context", err.Error())
	}
}

// The -c flag is registered as a persistent flag with the short form.
func TestContextFlagRegistered(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	f := root.PersistentFlags().Lookup("context")
	if f == nil {
		t.Fatal("persistent --context flag not registered")
	}
	if f.Shorthand != "c" {
		t.Errorf("shorthand = %q, want c", f.Shorthand)
	}
}
