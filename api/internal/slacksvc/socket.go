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

// Sentinel errors the receive loop returns when the link ends for a reason other
// than our own context cancellation, so the Manager backs off and reconnects.
// None carry a Slack auth marker, so classifyState treats them as connection-class
// failures (the auth path returns errSocketInvalidAuth instead).
var (
	// errSocketRunEnded means slack-go's RunContext returned cleanly while our
	// context was still live — the managed connection stopped without being asked to.
	errSocketRunEnded = errors.New("slacksvc: socket mode run loop ended before shutdown")
	// errSocketEventsClosed means the Events channel was closed under us, so no
	// further envelopes can arrive on this connection.
	errSocketEventsClosed = errors.New("slacksvc: socket mode event stream closed unexpectedly")
	// errSocketInvalidAuth means Slack reported the app-level token as invalid. Its
	// text carries the invalid_auth marker so classifyState maps it to the auth state.
	errSocketInvalidAuth = errors.New("slacksvc: socket mode rejected the app token (invalid_auth)")
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

// ackable reports whether evt carries an envelope Slack expects an ack for: a
// non-nil Request with a non-empty EnvelopeID. Hello (and slack-go's internal
// events) carry an empty id, and acking one sends `{"envelope_id":""}` —
// protocol garbage after which Slack drops the socket ~10s later (found live
// 2026-07-10: the connection flapped on a 10s cycle and every inbound click/DM
// was lost). Every ack in the receive loop goes through this single predicate
// so the invariant is structural, not a per-branch spelling.
func ackable(evt socketmode.Event) bool {
	return evt.Request != nil && evt.Request.EnvelopeID != ""
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
// fails/drops, scoped to uzi's single app.
//
// TODO(cleanroom): implement from the behavioral spec. The connection-supervision
// scaffold (running slack-go's RunContext under a cancellable child context and
// waiting for it on every exit path) and the two connection/stream-closed error
// strings must be written INDEPENDENTLY — do NOT restore the previous expression.
// The event-routing behavior (ack-first ordering, the ackable predicate, and the
// per-event-type handling) is uzi's own; reproduce it from the spec. Keep this
// signature and the helper functions (ackable, actionIDs, routeInteractive,
// routeMessage, newSocketDialer) unchanged.
func dialSocketMode(ctx context.Context, client *http.Client, botToken, appToken string, inbound InboundHandler, messages MessageHandler, onConnecting, onConnected func()) error {
	_ = botToken // Socket Mode authenticates with the app-level token; the bot token is used elsewhere.

	// Debug is opt-in only: slack-go's debug logs include the wss handshake URL,
	// whose ?ticket= query is a live credential our Redact pass does not strip.
	debug := os.Getenv("SLACK_SOCKET_DEBUG") == "1"
	api := slack.New("",
		slack.OptionAppLevelToken(appToken),
		slack.OptionHTTPClient(client),
		slack.OptionDebug(debug),
	)
	sm := socketmode.New(api, socketmode.OptionDebug(debug))

	// slack-go's managed connection runs under a child context we own. On every
	// exit path we cancel it and wait for the goroutine to drain, so this call
	// never leaves a RunContext goroutine behind.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- sm.RunContext(runCtx) }()

	// shutdown cancels the run goroutine, waits for it, and returns the error to
	// report to the supervisor.
	shutdown := func(report error) error {
		cancelRun()
		<-runDone
		return report
	}

	for {
		select {
		case <-ctx.Done():
			// The supervisor asked us to stop: a clean shutdown, not a failure.
			return shutdown(nil)

		case err := <-runDone:
			// RunContext returned on its own. If our context is already cancelled the
			// stop was expected (clean); otherwise the link failed or ended early.
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return err
			}
			return errSocketRunEnded

		case evt, ok := <-sm.Events:
			if !ok {
				return shutdown(errSocketEventsClosed)
			}

			switch evt.Type {
			case socketmode.EventTypeConnecting:
				onConnecting()

			case socketmode.EventTypeConnected, socketmode.EventTypeHello:
				onConnected()

			case socketmode.EventTypeErrorBadMessage:
				if bad, ok := evt.Data.(*socketmode.ErrorBadMessage); ok && bad.Cause != nil {
					slog.Warn("slacksvc: socket mode bad message", "cause", Redact(bad.Cause.Error()))
				} else {
					slog.Warn("slacksvc: socket mode bad message")
				}
				continue

			case socketmode.EventTypeInvalidAuth:
				return shutdown(errSocketInvalidAuth)

			case socketmode.EventTypeInteractive:
				// Ack before doing any work so Slack does not retry the envelope.
				if ackable(evt) {
					_ = sm.Ack(*evt.Request)
				}
				if inbound != nil {
					if cb, ok := evt.Data.(slack.InteractionCallback); ok {
						slog.Info("slacksvc: interactive received",
							"callback_type", string(cb.Type),
							"action_ids", actionIDs(cb)) // ids only, never values
						routeInteractive(ctx, inbound, cb)
					}
				}
				continue

			case socketmode.EventTypeEventsAPI:
				if ackable(evt) {
					_ = sm.Ack(*evt.Request)
				}
				if messages != nil {
					if e, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
						slog.Info("slacksvc: events_api received",
							"inner_event_type", e.InnerEvent.Type)
						routeMessage(ctx, messages, e)
					}
				}
				continue

			case socketmode.EventTypeConnectionError, socketmode.EventTypeIncomingError, socketmode.EventTypeErrorWriteFailed:
				slog.Warn("slacksvc: socket mode transport event", "type", string(evt.Type))

			default:
				slog.Info("slacksvc: socket mode event", "type", string(evt.Type))
			}

			// Envelopes that fell through the switch (not the continue paths above)
			// still get acked if Slack expects one for them.
			if ackable(evt) {
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

// routeMessage turns a message.im event into a MessageReply for the handler. Two
// shapes route (PRD #191 M2): a THREAD REPLY (thread_ts set → a reply on an existing
// run/chat, resolved by the anchor) and a TOP-LEVEL DM (thread_ts empty → opens a new
// chat). Dropped either way: the bot's own posts (BotID set), non-message subtypes
// (edits/deletes/joins carry one), non-DM channel types, an empty author, and empty
// text. The actor is the authenticated envelope user (ev.User) — never a
// client-controlled value — which is what makes the downstream confirmed-user /
// ownership join trustworthy. It is the small pure seam the dial loop delegates to so
// it stays unit-testable.
func routeMessage(ctx context.Context, messages MessageHandler, e slackevents.EventsAPIEvent) {
	if e.Type != slackevents.CallbackEvent {
		return
	}
	ev, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Plain user message in a DM only: no subtype (edits/deletes/joins carry one), not
	// a bot post, a real author. A top-level DM (empty thread_ts) is kept — HandleMessage
	// routes it to the chat opener; only empty text is dropped.
	if ev.ChannelType != "im" || ev.SubType != "" || ev.BotID != "" || ev.User == "" {
		return
	}
	if strings.TrimSpace(ev.Text) == "" {
		return
	}
	messages.HandleMessage(ctx, MessageReply{
		SlackUserID: ev.User,
		ChannelID:   ev.Channel,
		ThreadTS:    ev.ThreadTimeStamp, // "" for a top-level DM → opens a chat
		MessageTS:   ev.TimeStamp,
		Text:        ev.Text,
	})
}
