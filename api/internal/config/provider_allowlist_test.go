package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestParseProviderAllowlist (PRD #4320): exact provider base URLs WITH path, normalized and
// deduped; empty means the feature is off; any unsafe entry is a boot error that names the
// variable and its position but not the entry.
func TestParseProviderAllowlist(t *testing.T) {
	for _, empty := range []string{"", "   ", " , ,"} {
		got, err := parseProviderAllowlist(empty, false)
		if err != nil || got != nil {
			t.Errorf("parse(%q) = %v, %v; want nil, nil", empty, got, err)
		}
	}
	got, err := parseProviderAllowlist(" HTTPS://GW.example.com:443/anthropic/ , https://gw.example.com/anthropic, https://gw.example.com/other", false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"https://gw.example.com/anthropic", "https://gw.example.com/other"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse = %v, want %v", got, want)
	}
	bad := []string{
		"http://gw.example.com/anthropic",
		"https://gw.example.com/anthropic?x=1",
		"https://gw.example.com/anthropic#f",
		"https://user:pw@gw.example.com/anthropic",
		"https://gw.example.com/anthropic/../other",
		"https://gw.example.com/%2e%2e/other",
		"https://gw.example.com/a%2Fb",
		"https://gw.example.com/a;b",
		"https://gw.example.com\\a",
		"https://gw.exämple.com/a",
		"https://localhost:8443/anthropic",
		"https://ok.example.com/a, not a url",
		"https://gw.example.com/" + strings.Repeat("a", 2100),
	}
	for _, b := range bad {
		_, err := parseProviderAllowlist(b, false)
		if err == nil {
			t.Errorf("parse(%.60q) accepted an unsafe entry", b)
			continue
		}
		if !strings.Contains(err.Error(), "UZI_PROVIDER_ALLOWED_BASE_URLS") {
			t.Errorf("parse(%.60q) error %q does not name the variable", b, err)
		}
	}
	if _, err := parseProviderAllowlist("https://ok.example.com/a, user:pw@x", false); err == nil || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("error %v does not name the entry position", err)
	}
	// Loopback: refused without the flag, accepted (with http) with it.
	if _, err := parseProviderAllowlist("http://127.0.0.1:9000/anthropic", false); err == nil {
		t.Error("loopback entry accepted without UZI_PROVIDER_ALLOW_LOOPBACK")
	}
	got, err = parseProviderAllowlist("http://127.0.0.1:9000/anthropic/, https://gw.example.com/a", true)
	if err != nil || !reflect.DeepEqual(got, []string{"http://127.0.0.1:9000/anthropic", "https://gw.example.com/a"}) {
		t.Errorf("loopback with flag = %v, %v", got, err)
	}
	// A remote http entry stays refused even with the flag.
	if _, err := parseProviderAllowlist("http://gw.example.com/a", true); err == nil {
		t.Error("remote http entry accepted with the loopback flag")
	}
}

func TestProviderBaseURLAllowed(t *testing.T) {
	if _, ok := (Config{}).ProviderBaseURLAllowed("https://gw.example.com/anthropic"); ok {
		t.Error("an empty allowlist allowed a URL; the feature is off")
	}
	c := Config{ProviderAllowedBaseURLs: []string{"https://gw.example.com/anthropic"}}
	if e, ok := c.ProviderBaseURLAllowed("https://GW.example.com/anthropic/"); !ok || e != "https://gw.example.com/anthropic" {
		t.Errorf("exact match = (%q, %v)", e, ok)
	}
	for _, no := range []string{
		"https://gw.example.com/other",
		"https://gw.example.com/anthropic/../other",
		"https://gw.example.com.evil.test/anthropic",
		"https://gw.example.com/anthropic/v1",
	} {
		if _, ok := c.ProviderBaseURLAllowed(no); ok {
			t.Errorf("%q was allowed", no)
		}
	}
}

func TestLoadReadsProviderAllowlist(t *testing.T) {
	autoselectEnv(t)
	const env, flag = "UZI_PROVIDER_ALLOWED_BASE_URLS", "UZI_PROVIDER_ALLOW_LOOPBACK"

	// Truly unset, not just empty: t.Setenv registers the restore, then Unsetenv removes it.
	t.Setenv(env, "x")
	t.Setenv(flag, "x")
	if err := os.Unsetenv(env); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(flag); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with both unset: %v", err)
	}
	if cfg.ProviderAllowedBaseURLs != nil || cfg.ProviderAllowLoopback {
		t.Errorf("defaults = %v, %v; want nil, false", cfg.ProviderAllowedBaseURLs, cfg.ProviderAllowLoopback)
	}

	t.Setenv(env, "https://GW.example.com/anthropic/")
	cfg, err = Load()
	if err != nil || len(cfg.ProviderAllowedBaseURLs) != 1 || cfg.ProviderAllowedBaseURLs[0] != "https://gw.example.com/anthropic" {
		t.Fatalf("Load = %v, %v", cfg.ProviderAllowedBaseURLs, err)
	}

	t.Setenv(env, "http://127.0.0.1:9000/a")
	if _, err = Load(); err == nil {
		t.Fatal("Load accepted a loopback entry without the dev flag")
	}
	t.Setenv(flag, "true")
	cfg, err = Load()
	if err != nil || !cfg.ProviderAllowLoopback || len(cfg.ProviderAllowedBaseURLs) != 1 {
		t.Fatalf("Load with flag = %v, %v, %v", cfg.ProviderAllowedBaseURLs, cfg.ProviderAllowLoopback, err)
	}

	t.Setenv(flag, "maybe")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), flag) {
		t.Errorf("bad flag error = %v, want one naming %s", err, flag)
	}

	t.Setenv(flag, "false")
	t.Setenv(env, "https://gw.example.com/a/../b")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), env) {
		t.Errorf("boot error = %v, want one naming %s", err, env)
	}
}
