package handler

import (
	"context"
	"log/slog"
	"sync"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

// anthropicGateway is the instance's Anthropic-compatible gateway, or nil when none is
// configured or the operator's ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS no longer admits its
// host. Checked at every use, not only at write: the allowlist is operator config and can
// narrow after the admin saved the URL, and a token must never go to a host it no longer
// names.
func (h *Handler) anthropicGateway(ctx context.Context) *anthropic.Gateway {
	if h.settings == nil {
		return nil
	}
	gw, ok, _ := h.settings.AnthropicGateway(ctx)
	if !ok {
		return nil
	}
	if !h.cfg.AnthropicGatewayBaseURLAllowed(gw.BaseURL) {
		gatewayBlockedLog.Do(func() {
			slog.Warn("anthropic gateway configured but not in ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS; Claude calls are blocked until it is allowlisted or cleared")
		})
		return nil
	}
	return &gw
}

// gatewayBlockedLog keeps the blocked warning to once per process: it would otherwise
// fire on every worker heartbeat.
var gatewayBlockedLog sync.Once

// anthropicGatewayBlocked: an admin saved a gateway, but it cannot be used (invalid, or
// not allowlisted). The saved tokens are gateway keys, so this is not "off": workers must
// not send them to api.anthropic.com and the token test must not judge them there.
func (h *Handler) anthropicGatewayBlocked(ctx context.Context) bool {
	return h.settings != nil && h.settings.AnthropicGatewayConfigured(ctx) && h.anthropicGateway(ctx) == nil
}

// gatewayWire is the gateway as workers receive it on register and heartbeat. Not a
// secret: the key it routes is the user's own token, delivered per claim as before.
type gatewayWire struct {
	BaseURL    string            `json:"base_url,omitempty"`
	AuthHeader string            `json:"auth_header,omitempty"`
	Models     map[string]string `json:"models,omitempty"`
	// Blocked: a gateway is configured but unusable. The worker then routes the CLI to an
	// unresolvable host, so the token fails closed instead of reaching api.anthropic.com.
	Blocked bool `json:"blocked,omitempty"`
}

// anthropicGatewayWire is the register/heartbeat field: the gateway, {"blocked":true}
// when one is configured but unusable, or nil when none is configured (the field is then
// null and the worker clears any gateway it held, so turning the setting off reaches
// running workers too).
func (h *Handler) anthropicGatewayWire(ctx context.Context) *gatewayWire {
	gw := h.anthropicGateway(ctx)
	if gw == nil {
		if h.anthropicGatewayBlocked(ctx) {
			return &gatewayWire{Blocked: true}
		}
		return nil
	}
	return &gatewayWire{BaseURL: gw.BaseURL, AuthHeader: gw.AuthHeader, Models: gw.Models}
}
