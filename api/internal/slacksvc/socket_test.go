package slacksvc

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
)

// recordingInbound captures the BlockActions routeInteractive dispatches.
type recordingInbound struct{ actions []BlockAction }

func (r *recordingInbound) HandleBlockAction(_ context.Context, a BlockAction) {
	r.actions = append(r.actions, a)
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
