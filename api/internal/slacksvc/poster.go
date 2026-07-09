package slacksvc

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// slackPoster is the production Poster: it builds a bot-token Web API client per
// call from the CURRENT token (settings.SlackBotToken) so a hot token rotation is
// picked up mid-run, bounded by the shared Slack HTTP client.
type slackPoster struct {
	botToken func(context.Context) (string, error)
	client   *http.Client
	apiURL   string // "" = real Slack (set by tests)
}

// NewPoster builds the production Poster. botToken reads the current bot token;
// client bounds the outbound calls (SlackHTTPTimeout).
func NewPoster(botToken func(context.Context) (string, error), client *http.Client) Poster {
	return &slackPoster{botToken: botToken, client: client}
}

func (p *slackPoster) api(ctx context.Context) (*slack.Client, error) {
	tok, err := p.botToken(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("slack: bot token not configured")
	}
	opts := []slack.Option{}
	if p.client != nil {
		opts = append(opts, slack.OptionHTTPClient(p.client))
	}
	if p.apiURL != "" {
		base := p.apiURL
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		opts = append(opts, slack.OptionAPIURL(base))
	}
	return slack.New(tok, opts...), nil
}

func (p *slackPoster) OpenDM(ctx context.Context, slackUserID string) (string, error) {
	c, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	ch, _, _, err := c.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{slackUserID}})
	if err != nil {
		return "", err
	}
	return ch.ID, nil
}

func (p *slackPoster) Post(ctx context.Context, channelID, threadTS, text string) (string, error) {
	c, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := c.PostMessageContext(ctx, channelID, opts...)
	return ts, err
}

func (p *slackPoster) Update(ctx context.Context, channelID, ts, text string) error {
	c, err := p.api(ctx)
	if err != nil {
		return err
	}
	_, _, _, err = c.UpdateMessageContext(ctx, channelID, ts, slack.MsgOptionText(text, false))
	return err
}

func (p *slackPoster) PostBlocks(ctx context.Context, channelID, threadTS, fallbackText string, blocks []slack.Block) (string, error) {
	c, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	// fallbackText is the plain-text fallback (notifications, no-blocks clients);
	// the blocks carry the actual buttons.
	opts := []slack.MsgOption{slack.MsgOptionText(fallbackText, false), slack.MsgOptionBlocks(blocks...)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := c.PostMessageContext(ctx, channelID, opts...)
	return ts, err
}

func (p *slackPoster) LookupUserByEmail(ctx context.Context, email string) (string, error) {
	c, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	u, err := c.GetUserByEmailContext(ctx, email)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}
