package slacksvc

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// newSocketDialer returns the production DialFunc bound to a client whose Timeout
// caps the Socket Mode HTTP handshake (apps.connections.open). The websocket
// itself is not bounded by the client timeout — its liveness is the deadman
// ping — so a healthy long-lived connection is unaffected. inbound (may be nil)
// is captured once here, NOT per connection: it is static, so a hot token restart
// re-dials with the same handler.
func newSocketDialer(timeout time.Duration, inbound InboundHandler) DialFunc {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, botToken, appToken string, onConnecting, onConnected func()) error {
		return dialSocketMode(ctx, client, botToken, appToken, inbound, onConnecting, onConnected)
	}
}

// dialSocketMode opens ONE Socket Mode connection authenticated with the
// app-level token and runs its receive loop until ctx is cancelled or the link
// fails/drops. It mirrors multica's per-connection Connect (cancellable run
// context + RunContext goroutine), scoped to uzi's single app.
//
// slack-go's RunContext reconnects internally on a Slack-requested disconnect and
// returns an error only on a HARD failure (e.g. a rejected token) — exactly the
// case the Manager's outer backoff supervises. Debug logging is left OFF (the
// slack-go default): the wss connection URL carries a ?ticket= credential that
// the token-pattern redaction would miss, so it must never reach a log.
//
// Inbound interactive block_actions are ACKed first (Slack re-delivers an
// un-ACKed envelope every ~3s) and then routed to inbound (the link Confirm /
// Not-me handler in M3; M4/M5 add gate + reply kinds). message.im events are
// still ACKed and discarded until M5. The bot token authenticates the outbound
// Web API client, not the socket (the socket uses the app token), so it is
// unused here.
func dialSocketMode(ctx context.Context, client *http.Client, botToken, appToken string, inbound InboundHandler, onConnecting, onConnected func()) error {
	_ = botToken // the outbound bot-token client is built from this in the poster.
	api := slack.New("", slack.OptionAppLevelToken(appToken), slack.OptionHTTPClient(client))
	sm := socketmode.New(api)

	// Each connection runs under its own cancellable context; every exit path
	// cancels it and waits for the run goroutine to observe the cancellation and
	// exit, so a transient failure tears the socket down before the Manager
	// reconnects — no leaked goroutine draining into an unread channel.
	runCtx, runCancel := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runErr <- sm.RunContext(runCtx)
		close(done)
	}()
	defer func() {
		runCancel()
		<-done
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-runErr:
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("slack: socket mode connection closed")
		case evt, ok := <-sm.Events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("slack: socket mode event stream closed")
			}
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				onConnecting()
			case socketmode.EventTypeConnected, socketmode.EventTypeHello:
				onConnected()
			case socketmode.EventTypeInvalidAuth:
				// Surface a clean auth failure the Manager classifies as error:auth,
				// rather than waiting for the generic run error.
				return errors.New("slack: invalid_auth")
			case socketmode.EventTypeInteractive:
				// ACK FIRST, before any processing, so Slack does not re-deliver the
				// envelope in ~3s. Only then route the (Slack-authenticated) payload.
				if evt.Request != nil {
					_ = sm.Ack(*evt.Request)
				}
				if inbound != nil {
					if cb, ok := evt.Data.(slack.InteractionCallback); ok {
						routeInteractive(ctx, inbound, cb)
					}
				}
				continue
			}
			// Ack any other envelope so Slack does not re-deliver it.
			if evt.Request != nil {
				_ = sm.Ack(*evt.Request)
			}
		}
	}
}

// routeInteractive turns a block_actions callback into one BlockAction per pressed
// button and hands each to the inbound handler. The actor is ALWAYS the
// Slack-authenticated envelope user (callback.User.ID) — never a value read from a
// payload blob — which is what makes the downstream authz join trustworthy. It is
// the small, pure seam the dial loop delegates to so it stays unit-testable while
// dialSocketMode itself does not (it needs a live socket).
func routeInteractive(ctx context.Context, inbound InboundHandler, cb slack.InteractionCallback) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	for _, ba := range cb.ActionCallback.BlockActions {
		if ba == nil {
			continue
		}
		inbound.HandleBlockAction(ctx, BlockAction{
			SlackUserID: cb.User.ID,
			ActionID:    ba.ActionID,
			Value:       ba.Value,
			ChannelID:   cb.Container.ChannelID,
			MessageTS:   cb.Container.MessageTs,
		})
	}
}
