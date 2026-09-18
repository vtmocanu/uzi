package slacksvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// notifier_credential_switch_test.go covers the PRD #1247 M9 (task d) resume-DM token attribution:
// a token switched to during the park cycle is named in the ▶️ Resumed DM, a non-switched resume is
// unchanged, a hostile label is escaped, and the park-cycle anchor (limit_paused_at) is the `since`
// the evidence query is scoped by.

// resumeThreadBlocks names the switched-to token as a "· now on <label>" detail fragment.
func TestResumeThreadBlocksNamesSwitchedToken(t *testing.T) {
	blocks, _ := resumeThreadBlocks(baseRun("running"), time.Hour, true, "https://uzi.example", "limit_wait", "cristi")
	if ctx := contextText(blocks); !strings.Contains(ctx, "now on cristi") {
		t.Fatalf("resume context = %q, want it to name `now on cristi`", ctx)
	}
}

// A resume with no switch (switchedTo == "") carries no "now on" fragment — the DM is unchanged.
func TestResumeThreadBlocksNoSwitchOmitsTokenLine(t *testing.T) {
	blocks, _ := resumeThreadBlocks(baseRun("running"), time.Hour, true, "https://uzi.example", "limit_wait", "")
	if ctx := contextText(blocks); strings.Contains(ctx, "now on") {
		t.Fatalf("resume context = %q, must NOT carry a `now on` fragment on a non-switched resume", ctx)
	}
}

// A hostile label is escaped for Slack mrkdwn before it reaches the DM: angle brackets and
// ampersands are entity-escaped, so no raw markup or link injection survives.
func TestResumeThreadBlocksEscapesHostileLabel(t *testing.T) {
	blocks, _ := resumeThreadBlocks(baseRun("running"), time.Hour, true, "https://uzi.example", "limit_wait", "<script>&x")
	ctx := contextText(blocks)
	if strings.Contains(ctx, "<script>") {
		t.Fatalf("resume context = %q, must NOT carry the raw `<script>` label", ctx)
	}
	if !strings.Contains(ctx, "&lt;script&gt;&amp;x") {
		t.Fatalf("resume context = %q, want the escaped label `&lt;script&gt;&amp;x`", ctx)
	}
}

// credentialSwitchLabelSince parses the label out of the switch-message payload, returns "" on
// pgx.ErrNoRows (no switch this cycle), returns "" for a null/absent label, and passes the park
// anchor through as the query's `since`.
func TestCredentialSwitchLabelSince(t *testing.T) {
	anchor := pgtype.Timestamptz{Time: time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC), Valid: true}
	runID := baseRun("running").ID

	// A real label.
	fs := &fakeNotifStore{credSwitch: []byte(`{"label":"cristi","select_reason":"run_pinned","secret_id":"x"}`)}
	n := NewNotifier(fs, &fakePoster{}, fixedBase, nil)
	if got := n.credentialSwitchLabelSince(context.Background(), runID, anchor); got != "cristi" {
		t.Fatalf("label = %q, want cristi", got)
	}
	if len(fs.credSwitchArgs) != 1 || !fs.credSwitchArgs[0].Since.Time.Equal(anchor.Time) {
		t.Fatalf("query since = %+v, want the park anchor %+v", fs.credSwitchArgs, anchor)
	}

	// No switch this cycle (ErrNoRows via a nil payload) → "".
	if got := NewNotifier(&fakeNotifStore{}, &fakePoster{}, fixedBase, nil).credentialSwitchLabelSince(context.Background(), runID, anchor); got != "" {
		t.Fatalf("no-switch label = %q, want empty", got)
	}

	// A null/absent label in the payload → "" (an unlabelled token adds no line).
	nullLabel := &fakeNotifStore{credSwitch: []byte(`{"label":null,"select_reason":"run_default","secret_id":"x"}`)}
	if got := NewNotifier(nullLabel, &fakePoster{}, fixedBase, nil).credentialSwitchLabelSince(context.Background(), runID, anchor); got != "" {
		t.Fatalf("null-label = %q, want empty", got)
	}
}

// End-to-end through handle: a run parked (limit_wait) then running, with a switch APPLIED during
// the cycle, posts a ▶️ Resumed DM that names the new token — and the evidence query is scoped by
// the park-cycle anchor (the marker's limit_paused_at), not an arbitrary time.
func TestNotifierResumeNamesSwitchedTokenInPark(t *testing.T) {
	rc := baseRun("limit_wait")
	rc.StatusSince = pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	fs := &fakeNotifStore{
		rc:         rc,
		delivery:   txt("U1"),
		msg:        store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
		credSwitch: []byte(`{"label":"cristi","select_reason":"run_pinned","secret_id":"x"}`),
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)

	// Park stamps the marker at rc.StatusSince.
	fs.rc.Status = "limit_wait"
	fs.rc.LimitWaitCount = 1
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "limit_wait"})

	// Resume: the ▶️ Resumed DM names the token.
	fs.rc.Status = "running"
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	rb := resumeBlocks(fp.blocks)
	if len(rb) != 1 {
		t.Fatalf("want exactly one ▶️ Resumed reply, got %+v", fp.blocks)
	}
	if !strings.Contains(rb[0].contextText, "now on cristi") {
		t.Fatalf("resume context = %q, want it to name `now on cristi`", rb[0].contextText)
	}
	// The evidence was scoped by the park-cycle anchor (the marker's limit_paused_at = rc.StatusSince).
	if len(fs.credSwitchArgs) == 0 || !fs.credSwitchArgs[len(fs.credSwitchArgs)-1].Since.Time.Equal(rc.StatusSince.Time) {
		t.Fatalf("switch query since = %+v, want the park anchor %+v", fs.credSwitchArgs, rc.StatusSince)
	}
}

// A non-switched resume posts the ▶️ Resumed DM unchanged — no token line — even though the query
// ran (it just found no switch this cycle).
func TestNotifierResumeNoSwitchNoTokenLine(t *testing.T) {
	rc := baseRun("limit_wait")
	rc.StatusSince = pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
		// credSwitch nil → ErrNoRows → no switch this cycle.
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)

	fs.rc.Status = "limit_wait"
	fs.rc.LimitWaitCount = 1
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "limit_wait"})
	fs.rc.Status = "running"
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	rb := resumeBlocks(fp.blocks)
	if len(rb) != 1 {
		t.Fatalf("want exactly one ▶️ Resumed reply, got %+v", fp.blocks)
	}
	if strings.Contains(rb[0].contextText, "now on") {
		t.Fatalf("resume context = %q, must NOT carry a token line on a non-switched resume", rb[0].contextText)
	}
}
