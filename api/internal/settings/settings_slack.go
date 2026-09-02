package settings

// This file holds the Slack accessors and their write-time validators
// (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// SlackEnabled reports whether the Slack integration is enabled instance-wide
// (PRD #25). Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (false) rather than silently reading true — a deliberate
// junk-tolerance defaulting OFF, so a malformed value never silently turns the
// integration on.
func (c *Cache) SlackEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeySlackEnabled)
}

// PublicBaseURL returns the base URL used to build webui deep links in Slack
// messages (PRD #25). ENV (UZI_PUBLIC_BASE_URL) over the DB row over the
// loopback default.
func (c *Cache) PublicBaseURL(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPublicBaseURL)
}

// SlackBotToken returns the effective Slack bot token in plaintext (PRD #25):
// the ENV value when set, else the sealed DB row decrypted. Empty string + nil
// error when neither is configured. Only slacksvc calls this — it is the sole
// read path for a secret key, keeping token bytes out of every other accessor.
func (c *Cache) SlackBotToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackBotToken)
}

// SlackAppToken returns the effective Slack app-level token in plaintext (PRD
// #25), same precedence as SlackBotToken.
func (c *Cache) SlackAppToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackAppToken)
}

// validateSlackToken is the format gate for a secret Slack token (PRD #25):
// non-empty and the expected xoxb-/xapp- prefix. It deliberately NEVER includes
// the value in its error — a token must not appear in a validation message.
func validateSlackToken(value, prefix, kind string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s token must not be empty", kind)
	}
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s token must start with %s", kind, prefix)
	}
	return nil
}

// ValidatePublicBaseURL enforces the deep-link base-URL rule (PRD #25): a
// parseable URL with an http or https scheme and a host. It becomes a button URL
// in every DM, so no other scheme is allowed. Reused by config to check the
// UZI_PUBLIC_BASE_URL env value at boot (single source of the rule).
func ValidatePublicBaseURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use http or https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}
