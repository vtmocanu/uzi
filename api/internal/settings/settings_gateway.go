package settings

// The Anthropic-compatible gateway accessor and its write-time validators.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

// maxGatewayModelLen bounds one mapped model name; gateway model names are short
// identifiers, so this only catches a pasted blob.
const maxGatewayModelLen = 200

// gatewayModelRE is the one model-name rule, shared verbatim by the worker decoder
// (agent/src/anthropic-gateway.ts) and the admin card, so a value the api stores is
// never one the worker refuses. It covers provider-prefixed and versioned names
// (anthropic/claude-x, claude-x@20250101, bedrock/anthropic.claude-x:0).
var gatewayModelRE = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,200}$`)

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

// AnthropicGatewayConfigured reports whether an admin saved a gateway URL at all, valid
// or not. A configured gateway that cannot be used (invalid, or its host no longer
// allowlisted) is BLOCKED, not off: the saved tokens are gateway keys, so nothing may
// send them to api.anthropic.com.
func (c *Cache) AnthropicGatewayConfigured(ctx context.Context) bool {
	base, _ := c.get(ctx, KeyAnthropicGatewayBaseURL)
	return base != ""
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
		if !gatewayModelRE.MatchString(model) {
			return nil, fmt.Errorf("model %q for %q must be 1-%d characters of letters, digits and . _ : / @ -", model, alias, maxGatewayModelLen)
		}
		out[alias] = model
	}
	return out, nil
}
