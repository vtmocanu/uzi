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
// ping — so a healthy long-lived connection is unaffected.
func newSocketDialer(timeout time.Duration) DialFunc {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, botToken, appToken string, onConnecting, onConnected func()) error {
		return dialSocketMode(ctx, client, botToken, appToken, onConnecting, onConnected)
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
// M2 is connection-only: inbound product events (interactive block_actions,
// message.im) are ACKed and discarded here; M4/M5 route them. ACKing keeps Slack
// from re-delivering an un-ACKed envelope every ~3s. The bot token is unused in
// M2 (the socket authenticates with the app token alone) and is threaded through
// for M3's outbound Web API client.
func dialSocketMode(ctx context.Context, client *http.Client, botToken, appToken string, onConnecting, onConnected func()) error {
	_ = botToken // M3 builds the outbound bot-token client from this.
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
			}
			// Ack any envelope so Slack does not re-deliver it; M4/M5 route the
			// payload before acking.
			if evt.Request != nil {
				_ = sm.Ack(*evt.Request)
			}
		}
	}
}
