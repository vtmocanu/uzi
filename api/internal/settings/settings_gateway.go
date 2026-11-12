package settings

// The Anthropic-compatible gateway accessor and its write-time validators.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

// maxGatewayModelLen bounds one mapped model name; gateway model names are short
// identifiers, so this only catches a pasted blob.
const maxGatewayModelLen = 200

// AnthropicGateway returns the configured gateway, and false when the base URL is
// empty (the feature is off). It does NOT check the operator allowlist: settings
// cannot see config, so every caller pairs it with Config.AnthropicGatewayBaseURLAllowed.
// A stored value that no longer validates reads as off, never as a half gateway.
func (c *Cache) AnthropicGateway(ctx context.Context) (anthropic.Gateway, bool, error) {
	base, err := c.get(ctx, KeyAnthropicGatewayBaseURL)
	if err != nil || base == "" || validateGatewayBaseURL(base) != nil {
		return anthropic.Gateway{}, false, err
	}
	header, _ := c.get(ctx, KeyAnthropicGatewayAuthHeader)
	if header != anthropic.AuthHeaderAPIKey {
		header = anthropic.AuthHeaderBearer
	}
	raw, _ := c.get(ctx, KeyAnthropicGatewayModels)
	models, perr := ParseGatewayModels(raw)
	if perr != nil {
		return anthropic.Gateway{}, false, nil
	}
	return anthropic.Gateway{BaseURL: base, AuthHeader: header, Models: models}, true, nil
}

// validateGatewayBaseURL: empty is off; otherwise an absolute https URL with no
// userinfo, query or fragment, stored verbatim (no surrounding whitespace). A path is
// allowed: some gateways serve the Anthropic API under one (…/anthropic).
func validateGatewayBaseURL(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return errors.New("must not have leading or trailing whitespace")
	}
	u, err := url.Parse(value)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("must use https: every user's token is sent there")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	if u.User != nil {
		return errors.New("must not embed credentials in the URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry a query or fragment")
	}
	return nil
}

// ParseGatewayModels reads anthropic_gateway_models: comma-separated alias=model
// pairs. Empty means no mapping. Each alias is one of anthropic.GatewayAliases, named
// once; each model is a single token (no whitespace, comma or '='), at most
// maxGatewayModelLen characters.
func ParseGatewayModels(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		alias, model, ok := strings.Cut(strings.TrimSpace(pair), "=")
		alias, model = strings.TrimSpace(alias), strings.TrimSpace(model)
		if !ok || model == "" {
			return nil, fmt.Errorf("%q must be alias=model", pair)
		}
		if !slices.Contains(anthropic.GatewayAliases, alias) {
			return nil, fmt.Errorf("unknown alias %q (want one of %s)", alias, strings.Join(anthropic.GatewayAliases, ", "))
		}
		if _, dup := out[alias]; dup {
			return nil, fmt.Errorf("alias %q is mapped twice", alias)
		}
		if len(model) > maxGatewayModelLen || strings.ContainsAny(model, " \t\r\n=,") {
			return nil, fmt.Errorf("model %q for %q must be a single name of at most %d characters", model, alias, maxGatewayModelLen)
		}
		out[alias] = model
	}
	return out, nil
}
