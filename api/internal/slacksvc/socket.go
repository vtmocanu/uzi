package slacksvc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// actionIDs extracts the pressed buttons' action ids for the receipt log —
// ids only, never button values (a value may carry payload data).
func actionIDs(cb slack.InteractionCallback) []string {
	ids := make([]string, 0, len(cb.ActionCallback.BlockActions))
	for _, ba := range cb.ActionCallback.BlockActions {
		if ba != nil {
			ids = append(ids, ba.ActionID)
		}
	}
	return ids
}

// newSocketDialer returns the production DialFunc bound to a client whose Timeout
// caps the Socket Mode HTTP handshake (apps.connections.open). The websocket
// itself is not bounded by the client timeout — its liveness is the deadman
// ping — so a healthy long-lived connection is unaffected. inbound (may be nil)
// is captured once here, NOT per connection: it is static, so a hot token restart
// re-dials with the same handler.
func newSocketDialer(timeout time.Duration, inbound InboundHandler, messages MessageHandler) DialFunc {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, botToken, appToken string, onConnecting, onConnected func()) error {
		return dialSocketMode(ctx, client, botToken, appToken, inbound, messages, onConnecting, onConnected)
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
// un-ACKed envelope every ~3s) and then routed to inbound (link Confirm / Not-me
// + the M4 gate buttons). message.im events (thread replies) are ACKed first and
// routed to messages (M5). The bot token authenticates the outbound Web API
// client, not the socket (the socket uses the app token), so it is unused here.
func dialSocketMode(ctx context.Context, client *http.Client, botToken, appToken string, inbound InboundHandler, messages MessageHandler, onConnecting, onConnected func()) error {
	_ = botToken // the outbound bot-token client is built from this in the poster.
	// SLACK_SOCKET_DEBUG=1 turns on slack-go's own wire logging (every raw ws
	// frame). DIAGNOSTIC ONLY, off by default: the connect line includes the wss
	// ?ticket= credential, so never leave it on in a shared deployment.
	debug := os.Getenv("SLACK_SOCKET_DEBUG") == "1"
	api := slack.New("", slack.OptionAppLevelToken(appToken), slack.OptionHTTPClient(client), slack.OptionDebug(debug))
	sm := socketmode.New(api, socketmode.OptionDebug(debug))

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
			case socketmode.EventTypeErrorBadMessage:
				// slack-go could not parse an inbound envelope (e.g. a payload shape
				// newer than the vendored version understands). There is no Request to
				// ack, so Slack re-delivers ~3x and then surfaces a ⚠ to the clicker.
				// Log the cause (redacted) — otherwise this failure mode is fully
				// silent and looks identical to "no event arrived at all".
				if bad, ok := evt.Data.(*socketmode.ErrorBadMessage); ok && bad != nil && bad.Cause != nil {
					slog.Warn("slack: unparseable inbound envelope dropped", "error", Redact(bad.Cause.Error()))
				} else {
					slog.Warn("slack: unparseable inbound envelope dropped")
				}
				continue
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
						// Coarse receipt log (type + action ids only, never values): inbound
						// interactive events are rare and otherwise fully silent, which makes
						// a delivery failure indistinguishable from a routing one.
						slog.Info("slack: inbound interactive", "callback_type", string(cb.Type), "actions", actionIDs(cb))
						routeInteractive(ctx, inbound, cb)
					}
				}
				continue
			case socketmode.EventTypeEventsAPI:
				// ACK FIRST, then route message.im thread replies (M5). Other inner
				// events are ACKed and ignored.
				if evt.Request != nil {
					_ = sm.Ack(*evt.Request)
				}
				slog.Info("slack: inbound events-api envelope")
				if messages != nil {
					if e, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
						routeMessage(ctx, messages, e)
					}
				}
				continue
			default:
				// Coarse visibility on anything unrouted (type only, no payload) so an
				// ignored-but-relevant envelope kind is discoverable from the logs.
				slog.Info("slack: unhandled envelope type", "type", string(evt.Type))
			}
			// Ack any other envelope so Slack does not re-deliver it. Only envelopes
			// with a non-empty id are ackable: hello (and other control frames) carry
			// an empty EnvelopeID, and acking one sends `{"envelope_id":""}` — protocol
			// garbage that makes Slack drop the socket ~10s later (found live 2026-07-10:
			// the connection flapped on a 10s cycle and every inbound click/DM was lost).
			if evt.Request != nil && evt.Request.EnvelopeID != "" {
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

// routeMessage turns a message.im thread reply into a MessageReply for the handler
// (M5). Only genuine user thread replies in a DM are routed: the bot's own posts
// (BotID set), non-message subtypes (edits/deletes/joins), non-thread top-level
// DMs, non-DM channel types, and empty text are dropped. The actor is the
// authenticated envelope user (ev.User) — never a client-controlled value — which
// is what makes the downstream confirmed-user/ownership join trustworthy. It is
// the small pure seam the dial loop delegates to so it stays unit-testable.
func routeMessage(ctx context.Context, messages MessageHandler, e slackevents.EventsAPIEvent) {
	if e.Type != slackevents.CallbackEvent {
		return
	}
	ev, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Plain user message in a DM only: no subtype (edits/deletes/joins carry one),
	// not a bot post, a real author, and a thread reply (thread_ts == the run root).
	if ev.ChannelType != "im" || ev.SubType != "" || ev.BotID != "" || ev.User == "" {
		return
	}
	if strings.TrimSpace(ev.ThreadTimeStamp) == "" || strings.TrimSpace(ev.Text) == "" {
		return
	}
	messages.HandleMessage(ctx, MessageReply{
		SlackUserID: ev.User,
		ChannelID:   ev.Channel,
		ThreadTS:    ev.ThreadTimeStamp,
		MessageTS:   ev.TimeStamp,
		Text:        ev.Text,
	})
}
