package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Gateway is an Anthropic-compatible endpoint (LiteLLM, Portkey, Cloudflare AI Gateway,
// a corporate proxy) that every Claude call is routed through instead of
// api.anthropic.com (admin setting, see settings.AnthropicGateway). The user's saved
// Anthropic token is then that gateway's key.
type Gateway struct {
	// BaseURL is what Claude Code reads as ANTHROPIC_BASE_URL: the gateway serves
	// <BaseURL>/v1/messages.
	BaseURL string
	// AuthHeader is how the key is sent: AuthHeaderBearer (Authorization: Bearer, Claude
	// Code's ANTHROPIC_AUTH_TOKEN) or AuthHeaderAPIKey (x-api-key, ANTHROPIC_API_KEY).
	AuthHeader string
	// Models maps a Claude Code model alias (opus, sonnet, haiku, fable) to the name the
	// gateway routes. An unmapped alias is sent as Claude Code resolves it.
	Models map[string]string
}

const (
	AuthHeaderBearer = "authorization"
	AuthHeaderAPIKey = "x-api-key"
)

// GatewayAliases are the Claude Code model aliases a gateway model map may name, in
// the order the probe prefers them (cheapest first).
var GatewayAliases = []string{"haiku", "sonnet", "opus", "fable"}

// probeModelDefault is the probe's model when the map names none: the same pinned
// cheapest Haiku the direct probe uses.
const probeModelDefault = "claude-haiku-4-5"

// ProbeModel is the model the gateway probe asks for: the cheapest mapped alias, else
// the pinned default.
func (g Gateway) ProbeModel() string {
	for _, a := range GatewayAliases {
		if m := g.Models[a]; m != "" {
			return m
		}
	}
	return probeModelDefault
}

// MessagesURL is <BaseURL>/v1/messages, the path Claude Code calls.
func (g Gateway) MessagesURL() string {
	return strings.TrimRight(g.BaseURL, "/") + "/v1/messages"
}

// GatewayProbe sends one minimal Messages request (max_tokens 1, a fixed prompt) to the
// gateway with the key in the configured header. A 2xx proves the gateway accepts the
// key; a gateway exposes no Anthropic subscription windows, so there is no reading.
// Errors follow the package invariant: they never carry the token.
func (c *Client) GatewayProbe(ctx context.Context, g Gateway, token []byte) error {
	body, err := json.Marshal(map[string]any{
		"model":      g.ProbeModel(),
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return &Error{Kind: KindTransport, msg: "anthropic: build gateway probe failed"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.MessagesURL(), strings.NewReader(string(body)))
	if err != nil {
		return &Error{Kind: KindTransport, msg: "anthropic: build gateway request failed"}
	}
	if g.AuthHeader == AuthHeaderAPIKey {
		req.Header.Set("x-api-key", string(token))
	} else {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	// Never follow a redirect: Go strips Authorization on a cross-host hop but forwards
	// x-api-key, so a redirect could hand the key to another host. A 3xx is a refusal.
	hc := *c.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return &Error{Kind: KindTransport, msg: "anthropic: gateway request failed (transport error)"}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// No body excerpt: a gateway's error text may echo the key it refused.
		return httpError("gateway probe", resp.StatusCode, nil)
	}
	return nil
}
