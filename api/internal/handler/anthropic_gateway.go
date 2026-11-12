package handler

import (
	"context"
	"log/slog"

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
		slog.Warn("anthropic gateway configured but not in ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS; treating it as off")
		return nil
	}
	return &gw
}

// gatewayWire is the gateway as workers receive it on register and heartbeat. Not a
// secret: the key it routes is the user's own token, delivered per claim as before.
type gatewayWire struct {
	BaseURL    string            `json:"base_url"`
	AuthHeader string            `json:"auth_header"`
	Models     map[string]string `json:"models"`
}

// anthropicGatewayWire is the register/heartbeat field: the gateway, or nil (the field is
// then null and the worker clears any gateway it held, so turning the setting off reaches
// running workers too).
func (h *Handler) anthropicGatewayWire(ctx context.Context) *gatewayWire {
	gw := h.anthropicGateway(ctx)
	if gw == nil {
		return nil
	}
	return &gatewayWire{BaseURL: gw.BaseURL, AuthHeader: gw.AuthHeader, Models: gw.Models}
}
