package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimAnthropicEndpoint pins the PRD #229 D10 binding rule: only a Claude run on
// a deployment that configured an endpoint is bound, and a nil configuration leaves
// every other run exactly as it was before the feature existed.
func TestClaimAnthropicEndpoint(t *testing.T) {
	svc := &Service{}

	// Off by default: no endpoint configured means no run is bound, whatever its kind.
	if got := svc.claimAnthropicEndpoint(store.Run{Harness: "claude"}); got != nil {
		t.Fatalf("unconfigured service bound a Claude run to %+v", got)
	}

	svc.SetAnthropicEndpoint("https://gateway.example", "key-123")
	if svc.anthropicEndpoint == nil {
		t.Fatal("SetAnthropicEndpoint did not store the endpoint")
	}

	// A Claude run is bound.
	got := svc.claimAnthropicEndpoint(store.Run{Harness: "claude"})
	if got == nil || got.BaseURL != "https://gateway.example" || got.APIKey != "key-123" {
		t.Fatalf("Claude run bound to %+v, want the configured endpoint", got)
	}

	// A Codex run is NEVER bound: it has its own credential block and does not talk to
	// an Anthropic-shaped endpoint. Binding it would send repo contents somewhere the
	// operator never intended for that path.
	if got := svc.claimAnthropicEndpoint(store.Run{Harness: harnessCodex}); got != nil {
		t.Fatalf("Codex run bound to an Anthropic endpoint: %+v", got)
	}
}

// TestClaimAnthropicOAuthToken pins the mutual exclusion as data: a bound endpoint
// EMPTIES the subscription token at assembly, so the claim can never carry both and no
// downstream consumer has to remember to prefer one. This is the server-side half of
// the same boundary buildSdkEnv enforces worker-side.
func TestClaimAnthropicOAuthToken(t *testing.T) {
	subscription := []byte("subscription-token")
	endpoint := &ClaimAnthropicEndpoint{BaseURL: "https://gateway.example", APIKey: "key-123"}

	if got := claimAnthropicOAuthToken(subscription, nil); got != "subscription-token" {
		t.Fatalf("unbound run lost its token: got %q", got)
	}
	if got := claimAnthropicOAuthToken(subscription, endpoint); got != "" {
		t.Fatalf("a bound run still carried the subscription token: got %q", got)
	}
	// A bound run whose token was never opened stays empty, not a panic.
	if got := claimAnthropicOAuthToken(nil, endpoint); got != "" {
		t.Fatalf("bound run with no opened token = %q, want empty", got)
	}
}

// TestSetAnthropicEndpointEmptyLeavesFeatureOff pins that clearing the base URL
// reverts the service to today's behaviour rather than binding runs to "".
func TestSetAnthropicEndpointEmptyLeavesFeatureOff(t *testing.T) {
	svc := &Service{}
	svc.SetAnthropicEndpoint("https://gateway.example", "key-123")
	svc.SetAnthropicEndpoint("   ", "key-123")
	if svc.anthropicEndpoint != nil {
		t.Fatalf("a blank base URL left the endpoint bound: %+v", svc.anthropicEndpoint)
	}
}
