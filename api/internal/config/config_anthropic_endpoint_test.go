package config

import "testing"

// TestAnthropicEndpointBaseURLAllowed pins the custom-endpoint allowlist gate (PRD
// #229 D10). Repo contents travel to whichever endpoint a run's Claude harness talks
// to, so this is the security control that keeps a run from naming its own egress
// target: an empty allowlist must allow NOTHING, and matching must be on the
// normalized origin so a path or a trailing slash cannot smuggle a different host.
func TestAnthropicEndpointBaseURLAllowed(t *testing.T) {
	// Empty default: the feature is off, so no URL is bindable. This is the posture
	// every existing install has and the reason it behaves exactly as before.
	off := Config{}
	for _, raw := range []string{"https://gateway.example", "", "not a url", "http://insecure.example"} {
		if off.AnthropicEndpointBaseURLAllowed(raw) {
			t.Errorf("empty allowlist allowed %q; the feature must be off by default", raw)
		}
	}

	on := Config{AnthropicEndpointAllowedBaseURLs: []string{"https://gateway.example"}}

	// Allowed, including shapes that normalize to the same origin.
	for _, raw := range []string{
		"https://gateway.example",
		"https://GATEWAY.example",
		"https://gateway.example/",
		"https://gateway.example/v1/messages",
	} {
		if !on.AnthropicEndpointBaseURLAllowed(raw) {
			t.Errorf("allowlisted origin refused %q", raw)
		}
	}

	// Refused: a different host, a different scheme, userinfo, and a loopback
	// address that was never allowlisted.
	for _, raw := range []string{
		"https://other.example",
		"https://gateway.example.attacker.test",
		"http://gateway.example",
		"https://user:pass@gateway.example",
		"http://169.254.169.254",
	} {
		if on.AnthropicEndpointBaseURLAllowed(raw) {
			t.Errorf("non-allowlisted %q was allowed", raw)
		}
	}
}

// TestAnthropicEndpointAllowlistEnvParsing pins the env parse: an empty value turns
// the feature off without erroring (unlike the forge list, which hard-errors), and a
// malformed present entry fails closed rather than widening the allowlist.
func TestAnthropicEndpointAllowlistEnvParsing(t *testing.T) {
	for _, empty := range []string{"", "   ", " , , "} {
		got, err := parseBaseURLAllowlist("ANTHROPIC_ENDPOINT_ALLOWED_BASE_URLS", empty)
		if err != nil {
			t.Errorf("empty value errored: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("empty value parsed to %v, want empty", got)
		}
	}

	got, err := parseBaseURLAllowlist("ANTHROPIC_ENDPOINT_ALLOWED_BASE_URLS", "https://a.example, https://a.example/, https://b.example")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("got %v, want normalized+deduped [https://a.example https://b.example]", got)
	}

	if _, err := parseBaseURLAllowlist("ANTHROPIC_ENDPOINT_ALLOWED_BASE_URLS", "http://insecure.example"); err == nil {
		t.Error("an http entry must fail the parse (fail-closed)")
	}
}
