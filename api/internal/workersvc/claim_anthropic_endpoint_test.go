package workersvc

import (
	"encoding/json"
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

	svc.SetAnthropicEndpoint("https://gateway.example", "key-123", "gateway-model-1")
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
	svc.SetAnthropicEndpoint("https://gateway.example", "key-123", "gateway-model-1")
	svc.SetAnthropicEndpoint("   ", "key-123", "gateway-model-1")
	if svc.anthropicEndpoint != nil {
		t.Fatalf("a blank base URL left the endpoint bound: %+v", svc.anthropicEndpoint)
	}
}

// TestSetAnthropicEndpointRequiresAPIKey pins the third half of the binding, which
// Config Load enforces at boot and this setter did not. Binding an endpoint with no
// credential empties the subscription token out of the claim and puts a keyless URL
// in its place, so the run has no credential at all — worse than the feature being
// off. The setter is the second writer; a caller that reaches it having bypassed
// Load must not be able to produce that shape.
func TestSetAnthropicEndpointRequiresAPIKey(t *testing.T) {
	svc := &Service{}
	svc.SetAnthropicEndpoint("https://gateway.example", "key-123", "gateway-model-1")
	if svc.anthropicEndpoint == nil {
		t.Fatal("precondition: a fully-configured endpoint should bind")
	}

	for _, key := range []string{"", "   "} {
		svc.SetAnthropicEndpoint("https://gateway.example", key, "gateway-model-1")
		if svc.anthropicEndpoint != nil {
			t.Fatalf("an endpoint with api key %q was bound: %+v", key, svc.anthropicEndpoint)
		}
	}
}

// TestSetAnthropicEndpointRequiresModel pins the second half of the binding: an
// endpoint with no model is not a usable endpoint. A gateway that speaks the Messages
// shape need not serve Anthropic's model names, so a claim carrying a URL and a key
// but no model sends "claude-opus-5-5" to a host that has never heard of it and the
// run dies on its first turn. Config Load refuses this shape at boot; this pins the
// service's own refusal so a caller that bypasses Load cannot ship a broken claim.
func TestSetAnthropicEndpointRequiresModel(t *testing.T) {
	svc := &Service{}
	svc.SetAnthropicEndpoint("https://gateway.example", "key-123", "   ")
	if svc.anthropicEndpoint != nil {
		t.Fatalf("an endpoint with no model was bound: %+v", svc.anthropicEndpoint)
	}
}

// TestAssembleClaimEndpointOnlySkipsCredentialLadderLiveDB: on an instance bound to a
// custom endpoint with NO Anthropic token at all, claim assembly must still succeed.
//
// The endpoint IS the run's credential, so the subscription ladder has nothing to open.
// Running it anyway fails the claim on a credential the run never uses — which would make
// the endpoint feature create runs it can never start, on exactly the deployment shape it
// exists for. The claim must carry the endpoint and no subscription token.
func TestAssembleClaimEndpointOnlySkipsCredentialLadderLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID)

	// The real claim assembly opens the bot PAT, so reseal it with a genuine master-box blob.
	// Deliberately NO anthropic_token row: that is the shape under test.
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetAnthropicEndpoint("https://gateway.example", "sk-gateway", "gateway-model-1")
	wkr := store.Worker{ID: workerID, UserID: userID}

	payload, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, runID))
	if err != nil {
		t.Fatalf("assembleClaim on an endpoint-only instance: %v", err)
	}
	if payload.Secrets.AnthropicEndpoint == nil {
		t.Fatal("claim carries no endpoint; the run would have no credential at all")
	}
	if got := payload.Secrets.AnthropicEndpoint.Model; got != "gateway-model-1" {
		t.Fatalf("claim endpoint model = %q, want gateway-model-1", got)
	}
	if got := payload.Secrets.AnthropicOAuthToken; got != "" {
		t.Fatalf("claim carried a subscription token alongside the endpoint: %q", got)
	}
}

// TestClaimEndpointWireShape pins the JSON the worker actually sees, in both
// directions of the PRD #229 D10 exclusion. The shapes on the two sides are
// hand-written in different languages (ClaimAnthropicEndpoint in Go, the
// interface of the same name in agent/src/protocol.ts), and a rename on either
// side that still compiles and still unmarshals to a zero value would silently
// bind every run to an endpoint with no URL.
//
// It also pins the half that no round-trip can: a bound claim carries no
// subscription token, and an unbound claim omits the endpoint key entirely, so
// an instance with no endpoint configured keeps today's wire byte for byte.
func TestClaimEndpointWireShape(t *testing.T) {
	endpoint := &ClaimAnthropicEndpoint{
		BaseURL: "https://gateway.example",
		APIKey:  "ENDPOINT-KEY-PLACEHOLDER", //nolint:gosec // G101: placeholder fixture string, not a credential
		Model:   "gateway-model-1",
	}

	bound, err := json.Marshal(ClaimSecrets{
		AnthropicOAuthToken: claimAnthropicOAuthToken([]byte("subscription"), endpoint),
		AnthropicEndpoint:   endpoint,
	})
	if err != nil {
		t.Fatalf("marshal bound secrets: %v", err)
	}
	var boundWire map[string]json.RawMessage
	if err := json.Unmarshal(bound, &boundWire); err != nil {
		t.Fatalf("unmarshal bound secrets: %v", err)
	}
	var ep struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(boundWire["anthropic_endpoint"], &ep); err != nil {
		t.Fatalf("decode anthropic_endpoint (key present?): %v", err)
	}
	if ep.BaseURL != endpoint.BaseURL || ep.APIKey != endpoint.APIKey || ep.Model != endpoint.Model {
		t.Fatalf("endpoint wire = %+v, want base_url/api_key/model to carry through", ep)
	}
	// The exclusion, observed on the wire rather than in Go: a bound claim's token
	// field is empty. A producer that forgot claimAnthropicOAuthToken would ship
	// both credentials and no consumer would notice.
	var tok string
	if err := json.Unmarshal(boundWire["anthropic_oauth_token"], &tok); err != nil {
		t.Fatalf("decode anthropic_oauth_token: %v", err)
	}
	if tok != "" {
		t.Fatalf("bound claim carried a subscription token: %q", tok)
	}

	// An unbound claim omits the endpoint key entirely (omitempty), so an instance
	// with no endpoint configured emits byte-identical secrets to today's.
	unbound, err := json.Marshal(ClaimSecrets{
		AnthropicOAuthToken: claimAnthropicOAuthToken([]byte("subscription"), nil),
		AnthropicEndpoint:   nil,
	})
	if err != nil {
		t.Fatalf("marshal unbound secrets: %v", err)
	}
	var unboundWire map[string]json.RawMessage
	if err := json.Unmarshal(unbound, &unboundWire); err != nil {
		t.Fatalf("unmarshal unbound secrets: %v", err)
	}
	if _, present := unboundWire["anthropic_endpoint"]; present {
		t.Fatalf("unbound claim emitted anthropic_endpoint: %s", unbound)
	}
	if err := json.Unmarshal(unboundWire["anthropic_oauth_token"], &tok); err != nil || tok != "subscription" {
		t.Fatalf("unbound claim lost its token: %q (%v)", tok, err)
	}
}
