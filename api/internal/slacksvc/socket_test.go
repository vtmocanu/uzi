package slacksvc

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// The regression that shipped (specs/ai.md §167): acking hello — whose Request
// is non-nil but EnvelopeID empty — sends `{"envelope_id":""}`, after which
// Slack drops the socket ~10s later and every inbound event is lost. ackable is
// the single predicate every ack site goes through; hello-shaped envelopes must
// be false, genuine app envelopes true.
func TestAckable(t *testing.T) {
	for name, tc := range map[string]struct {
		evt  socketmode.Event
		want bool
	}{
		"hello: request without envelope id": {
			evt:  socketmode.Event{Type: socketmode.EventTypeHello, Request: &socketmode.Request{Type: "hello"}},
			want: false,
		},
		"internal event: no request at all": {
			evt:  socketmode.Event{Type: socketmode.EventTypeConnected},
			want: false,
		},
		"events_api envelope": {
			evt:  socketmode.Event{Type: socketmode.EventTypeEventsAPI, Request: &socketmode.Request{Type: "events_api", EnvelopeID: "env-1"}},
			want: true,
		},
		"interactive envelope": {
			evt:  socketmode.Event{Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{Type: "interactive", EnvelopeID: "env-2"}},
			want: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ackable(tc.evt); got != tc.want {
				t.Errorf("ackable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// recordingInbound captures the BlockActions routeInteractive dispatches.
type recordingInbound struct{ actions []BlockAction }

func (r *recordingInbound) HandleBlockAction(_ context.Context, a BlockAction) {
	r.actions = append(r.actions, a)
}

// recordingMessages captures the MessageReplies routeMessage dispatches.
type recordingMessages struct{ msgs []MessageReply }

func (r *recordingMessages) HandleMessage(_ context.Context, m MessageReply) {
	r.msgs = append(r.msgs, m)
}

func imEvent(ev *slackevents.MessageEvent) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "message", Data: ev},
	}
}

// The security-load-bearing property: the actor is the Slack-authenticated
// envelope user (callback.User.ID), NEVER a value read from a payload blob. This
// test forces the two apart and asserts the authenticated id wins.
func TestRouteInteractiveUsesAuthenticatedUser(t *testing.T) {
	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "Uauth"},
	}
	cb.Container.ChannelID = "D1"
	cb.Container.MessageTs = "ts1"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: ActionLinkConfirm, Value: "Uforged"}, // a spoofed id in the value blob
	}

	rec := &recordingInbound{}
	routeInteractive(context.Background(), rec, cb)

	if len(rec.actions) != 1 {
		t.Fatalf("want one routed action, got %d", len(rec.actions))
	}
	a := rec.actions[0]
	if a.SlackUserID != "Uauth" {
		t.Errorf("actor = %q, want the authenticated envelope id Uauth (never the value blob)", a.SlackUserID)
	}
	if a.ActionID != ActionLinkConfirm || a.ChannelID != "D1" || a.MessageTS != "ts1" {
		t.Errorf("routed action mismapped: %+v", a)
	}
}

func TestRouteInteractiveIgnoresNonBlockActions(t *testing.T) {
	cb := slack.InteractionCallback{Type: slack.InteractionTypeShortcut, User: slack.User{ID: "Uauth"}}
	rec := &recordingInbound{}
	routeInteractive(context.Background(), rec, cb)
	if len(rec.actions) != 0 {
		t.Fatalf("non-block-actions callback must not route: %+v", rec.actions)
	}
}

// A genuine user thread reply in a DM routes with the authenticated author id and
// the thread/reply timestamps the replier needs.
func TestRouteMessageRoutesUserThreadReply(t *testing.T) {
	rec := &recordingMessages{}
	routeMessage(context.Background(), rec, imEvent(&slackevents.MessageEvent{
		User: "Uauth", Text: "use pgx", ThreadTimeStamp: "root1", TimeStamp: "reply1",
		Channel: "D1", ChannelType: "im",
	}))
	if len(rec.msgs) != 1 {
		t.Fatalf("want one routed reply, got %d", len(rec.msgs))
	}
	m := rec.msgs[0]
	if m.SlackUserID != "Uauth" || m.ThreadTS != "root1" || m.MessageTS != "reply1" || m.ChannelID != "D1" || m.Text != "use pgx" {
		t.Fatalf("reply mismapped: %+v", m)
	}
}

// Everything that is not a plain user message in a DM is dropped: the bot's own
// posts, edits/deletes (subtypes), non-DM channels, empty text, empty author. A
// top-level DM (no thread_ts) is NO LONGER dropped (PRD #191 M2 — it opens a chat);
// see TestRouteMessageRoutesTopLevelDM.
func TestRouteMessageIgnoresNonReplies(t *testing.T) {
	cases := map[string]*slackevents.MessageEvent{
		"subtype edit": {User: "U", Text: "x", ChannelType: "im", ThreadTimeStamp: "r", SubType: "message_changed", TimeStamp: "t"},
		"bot message":  {BotID: "B1", Text: "x", ChannelType: "im", ThreadTimeStamp: "r", TimeStamp: "t"},
		"not a dm":     {User: "U", Text: "x", ChannelType: "channel", ThreadTimeStamp: "r", TimeStamp: "t"},
		"empty text":   {User: "U", Text: "   ", ChannelType: "im", ThreadTimeStamp: "r", TimeStamp: "t"},
		"empty author": {User: "", Text: "x", ChannelType: "im", ThreadTimeStamp: "r", TimeStamp: "t"},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &recordingMessages{}
			routeMessage(context.Background(), rec, imEvent(ev))
			if len(rec.msgs) != 0 {
				t.Fatalf("%s must not route: %+v", name, rec.msgs)
			}
		})
	}
}

// A top-level DM (no thread_ts) now routes with an EMPTY ThreadTS, which HandleMessage
// reads as "open a new chat" (PRD #191 M2). MessageTS is the user's message ts — the
// ts the chat anchor stores as root_ts.
func TestRouteMessageRoutesTopLevelDM(t *testing.T) {
	rec := &recordingMessages{}
	routeMessage(context.Background(), rec, imEvent(&slackevents.MessageEvent{
		User: "Uauth", Text: "what's running?", TimeStamp: "open1",
		Channel: "D1", ChannelType: "im",
	}))
	if len(rec.msgs) != 1 {
		t.Fatalf("want one routed top-level DM, got %d", len(rec.msgs))
	}
	m := rec.msgs[0]
	if m.ThreadTS != "" || m.MessageTS != "open1" || m.SlackUserID != "Uauth" || m.Text != "what's running?" {
		t.Fatalf("top-level DM mismapped: %+v", m)
	}
}
