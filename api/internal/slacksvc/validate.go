// Package slacksvc owns uzi's Slack integration (PRD #25). This file (M1) holds
// only the save-time token validation the settings PUT runs; the Socket Mode
// manager, notifier, and inbound router land in later milestones. The api service
// is the sole holder of Slack tokens — they never reach a worker or the agent.
package slacksvc

import (
	"context"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// Validator performs live validation of Slack tokens at save time (PRD #25 M1),
// mirroring multica's bring-your-own-app checks (byo_install.go): auth.test for
// the bot token, and apps.connections.open (the Socket Mode handshake) for the
// app-level token — so uzi never persists a token that would silently never
// authenticate or receive events.
//
// The zero value talks to the real Slack API. Tests set APIURL to an httptest
// server (and optionally HTTPClient) so validation runs offline.
type Validator struct {
	// HTTPClient overrides the outbound client; nil uses http.DefaultClient.
	HTTPClient *http.Client
	// APIURL overrides the Slack Web API base (must be reachable; a trailing
	// slash is added if missing, since the SDK appends the method name). Empty
	// targets the real Slack API.
	APIURL string
}

// opts builds the slack.Client options shared by both checks, honoring the
// APIURL/HTTPClient overrides. A fresh slice is returned each call.
func (v Validator) opts() []slack.Option {
	hc := v.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	opts := []slack.Option{slack.OptionHTTPClient(hc)}
	if v.APIURL != "" {
		base := v.APIURL
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		opts = append(opts, slack.OptionAPIURL(base))
	}
	return opts
}

// ValidateBotToken confirms the xoxb- bot token authenticates against Slack
// (auth.test). The prefix is assumed already checked (settings.Validate); this is
// the live half.
func (v Validator) ValidateBotToken(ctx context.Context, token string) error {
	_, err := slack.New(token, v.opts()...).AuthTestContext(ctx)
	return err
}

// ValidateAppToken confirms the xapp- app-level token can open a Socket Mode
// connection (apps.connections.open) — proving it is live for this app before we
// store it.
func (v Validator) ValidateAppToken(ctx context.Context, token string) error {
	api := slack.New("", append(v.opts(), slack.OptionAppLevelToken(token))...)
	_, _, err := api.StartSocketModeContext(ctx)
	return err
}
