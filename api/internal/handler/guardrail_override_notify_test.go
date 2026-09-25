package handler

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestGuardrailOverrideDecidedSlackRender pins PRD #1650 D3: the guardrail override
// decision now DMs the requester, keyed on the closed decision enum for its glyph, with
// the repo path passed RAW in the body (the notifier escapes it once) and a deep link to
// the Repos page, where the member acts on the decision.
func TestGuardrailOverrideDecidedSlackRender(t *testing.T) {
	req := store.GuardrailOverrideRequest{ID: uuid.New(), RepoID: uuid.New(), RequestedBy: uuid.New()}
	const hostilePath = "grp/<!here>&proj"

	for _, tc := range []struct {
		status, emoji, title string
	}{
		{"approved", "✅", "Guardrail override approved"},
		{"rejected", "⛔", "Guardrail override rejected"},
	} {
		n := buildGuardrailOverrideDecidedNotification("https://uzi.example.com/", req, hostilePath, tc.status)
		if n.Kind != guardrailOverrideDecidedKind || n.UserID != req.RequestedBy {
			t.Fatalf("%s: kind/user = %q/%v, want %q/%v", tc.status, n.Kind, n.UserID, guardrailOverrideDecidedKind, req.RequestedBy)
		}
		r := n.Slack
		if r == nil {
			t.Fatalf("%s: guardrail_override_decided must carry a Slack render", tc.status)
		}
		if r.Emoji != tc.emoji || r.Title != tc.title {
			t.Fatalf("%s: emoji/title = %q/%q, want %q/%q", tc.status, r.Emoji, r.Title, tc.emoji, tc.title)
		}
		if r.Link != "https://uzi.example.com/repos" {
			t.Fatalf("%s: link = %q, want the Repos page", tc.status, r.Link)
		}
		if !strings.Contains(r.Body, hostilePath) {
			t.Fatalf("%s: body must carry the repo path RAW: %q", tc.status, r.Body)
		}
		if len(r.Facts) != 0 {
			t.Fatalf("%s: the repo path must never ride in the trusted Facts: %v", tc.status, r.Facts)
		}
	}
}

// An unset public base URL yields a DM with no link rather than a relative one.
func TestGuardrailOverrideDecidedSlackNoBaseNoLink(t *testing.T) {
	n := buildGuardrailOverrideDecidedNotification("  ", store.GuardrailOverrideRequest{}, "", "approved")
	if n.Slack == nil {
		t.Fatalf("expected a Slack render")
	}
	if n.Slack.Link != "" {
		t.Fatalf("link = %q, want none without a base URL", n.Slack.Link)
	}
	if !strings.Contains(n.Slack.Body, "your repository") {
		t.Fatalf("a missing repo path must degrade to %q: %q", "your repository", n.Slack.Body)
	}
}
