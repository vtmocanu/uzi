package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeLinkerStore records the linker's writes so the auto-match guard and the
// Confirm / Not-me handling are observable without a database.
type fakeLinkerStore struct {
	users     []store.ListUsersForSlackLinkRow
	listCalls int
	listErr   error

	resolvedSet []store.SetUserSlackResolvedIDParams
	resolveErr  error

	confirmedIDs []string
	confirmRows  int64
	clearedIDs   []string
	clearRows    int64
}

func (f *fakeLinkerStore) ListUsersForSlackLink(context.Context) ([]store.ListUsersForSlackLinkRow, error) {
	f.listCalls++
	return f.users, f.listErr
}
func (f *fakeLinkerStore) SetUserSlackResolvedID(_ context.Context, arg store.SetUserSlackResolvedIDParams) (store.SetUserSlackResolvedIDRow, error) {
	if f.resolveErr != nil {
		return store.SetUserSlackResolvedIDRow{}, f.resolveErr
	}
	f.resolvedSet = append(f.resolvedSet, arg)
	return store.SetUserSlackResolvedIDRow{SlackResolvedID: arg.SlackResolvedID}, nil
}
func (f *fakeLinkerStore) ConfirmUserSlackLink(_ context.Context, id pgtype.Text) (int64, error) {
	f.confirmedIDs = append(f.confirmedIDs, id.String)
	return f.confirmRows, nil
}
func (f *fakeLinkerStore) ClearUserSlackLink(_ context.Context, id pgtype.Text) (int64, error) {
	f.clearedIDs = append(f.clearedIDs, id.String)
	return f.clearRows, nil
}

func linkRow(email, resolved string) store.ListUsersForSlackLinkRow {
	r := store.ListUsersForSlackLinkRow{ID: uuid.New(), Email: email}
	if resolved != "" {
		r.SlackResolvedID = pgtype.Text{String: resolved, Valid: true}
	}
	return r
}

func TestLinkerAutoMatchNewMatchLinksAndSendsConfirm(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{linkRow("dev@example.com", "")}}
	fp := &fakePoster{emailToID: map[string]string{"dev@example.com": "U123"}}
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())

	if len(fs.resolvedSet) != 1 || fs.resolvedSet[0].SlackResolvedID.String != "U123" {
		t.Fatalf("want resolved id cached as U123, got %+v", fs.resolvedSet)
	}
	if len(fp.blocks) != 1 {
		t.Fatalf("want one confirmation DM, got %+v", fp.blocks)
	}
	if got := fp.blocks[0].actionIDs; len(got) != 2 || got[0] != ActionLinkConfirm || got[1] != ActionLinkReject {
		t.Errorf("confirm DM buttons = %v, want [%s %s]", got, ActionLinkConfirm, ActionLinkReject)
	}
}

// The un-confirm hazard: SetUserSlackResolvedID resets confirmed_at, so an
// unchanged match on a reconnect must be a no-op (compare-then-write).
func TestLinkerAutoMatchSkipsUnchangedResolvedID(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{linkRow("dev@example.com", "U123")}}
	fp := &fakePoster{emailToID: map[string]string{"dev@example.com": "U123"}}
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())

	if len(fs.resolvedSet) != 0 {
		t.Errorf("unchanged match must not re-write (would un-confirm): %+v", fs.resolvedSet)
	}
	if len(fp.blocks) != 0 {
		t.Errorf("unchanged match must not re-send a confirmation DM: %+v", fp.blocks)
	}
}

func TestLinkerAutoMatchChangedResolvedIDRelinks(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{linkRow("dev@example.com", "Uold")}}
	fp := &fakePoster{emailToID: map[string]string{"dev@example.com": "Unew"}}
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())

	if len(fs.resolvedSet) != 1 || fs.resolvedSet[0].SlackResolvedID.String != "Unew" {
		t.Fatalf("changed match must re-write to Unew, got %+v", fs.resolvedSet)
	}
	if len(fp.blocks) != 1 {
		t.Errorf("changed match must send a fresh confirmation DM, got %+v", fp.blocks)
	}
}

func TestLinkerAutoMatchSkipsUnmatchedAndBlankEmail(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{
		linkRow("", ""),                   // no email → skip
		linkRow("nobody@example.com", ""), // no Slack match → skip
	}}
	fp := &fakePoster{} // emailToID nil → every lookup is users_not_found
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())

	if len(fs.resolvedSet) != 0 || len(fp.blocks) != 0 {
		t.Errorf("no matches expected: resolved=%v blocks=%v", fs.resolvedSet, fp.blocks)
	}
}

// A Tier-3 rate limit stops the whole pass rather than hammering the limit: a
// later user that WOULD match must not be processed.
func TestLinkerAutoMatchRateLimitStopsPass(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{
		linkRow("first@example.com", ""),  // not in emailToID → returns lookupErr (rate limited) → stop
		linkRow("second@example.com", ""), // would match, must NOT be reached
	}}
	fp := &fakePoster{
		emailToID: map[string]string{"second@example.com": "U999"},
		lookupErr: &slack.RateLimitedError{RetryAfter: time.Second},
	}
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())

	if len(fs.resolvedSet) != 0 {
		t.Errorf("pass must stop on rate limit before writing anything: %+v", fs.resolvedSet)
	}
}

func TestLinkerAutoMatchCooldown(t *testing.T) {
	fs := &fakeLinkerStore{users: []store.ListUsersForSlackLinkRow{linkRow("dev@example.com", "U1")}}
	fp := &fakePoster{emailToID: map[string]string{"dev@example.com": "U1"}}
	l := NewLinker(fs, fp, nil)
	l.AutoMatch(context.Background())
	l.AutoMatch(context.Background()) // within the cooldown → must be a no-op

	if fs.listCalls != 1 {
		t.Errorf("second pass within cooldown should not re-list users, listCalls=%d", fs.listCalls)
	}
}

// The account label named in the confirmation DM is escaped too, so a
// registration email carrying mrkdwn markup can't inject a link/mention into the
// trusted bot DM.
func TestLinkerConfirmDMEscapesAccountLabel(t *testing.T) {
	fp := &fakePoster{}
	l := NewLinker(&fakeLinkerStore{}, fp, nil)
	l.SendLinkConfirmation(context.Background(), "U1", "evil <@U999> <https://phishing.example|click>")

	if len(fp.blocks) != 1 {
		t.Fatalf("want one confirmation DM, got %+v", fp.blocks)
	}
	sec := fp.blocks[0].sectionText
	if strings.Contains(sec, "<@U999>") || strings.Contains(sec, "<https://phishing.example|click>") {
		t.Errorf("raw markup survived in the confirmation DM: %q", sec)
	}
	if !strings.Contains(sec, "&lt;@U999&gt;") {
		t.Errorf("account label was not mrkdwn-escaped: %q", sec)
	}
}

// The per-target cooldown suppresses a second Confirm card to the SAME member
// (override-hammering spam guard) while leaving a distinct target and the normal
// single-DM flow unaffected.
func TestLinkerConfirmDMPerTargetCooldown(t *testing.T) {
	fp := &fakePoster{}
	l := NewLinker(&fakeLinkerStore{}, fp, nil)

	l.SendLinkConfirmation(context.Background(), "U1", "acct")
	l.SendLinkConfirmation(context.Background(), "U1", "acct") // within cooldown → suppressed
	if len(fp.blocks) != 1 {
		t.Fatalf("a duplicate Confirm card to the same target must be suppressed, got %d DMs", len(fp.blocks))
	}
	l.SendLinkConfirmation(context.Background(), "U2", "acct") // distinct target → still sent
	if len(fp.blocks) != 2 {
		t.Fatalf("a distinct target must still receive its Confirm card, got %d DMs", len(fp.blocks))
	}
}

// The test DM is dedup'd per target too: a rapid re-test to the same id returns
// ErrDMCooldown (mapped to 429 by the handler) without a second Slack call, while
// a distinct target still sends.
func TestLinkerTestDMPerTargetCooldown(t *testing.T) {
	fp := &fakePoster{}
	l := NewLinker(&fakeLinkerStore{}, fp, nil)

	if err := l.SendTestDM(context.Background(), "U1"); err != nil {
		t.Fatalf("first test DM should send: %v", err)
	}
	if err := l.SendTestDM(context.Background(), "U1"); !errors.Is(err, ErrDMCooldown) {
		t.Fatalf("a second test DM within the cooldown should be ErrDMCooldown, got %v", err)
	}
	if len(fp.posts) != 1 {
		t.Fatalf("only the first test DM should reach Slack, got %d posts", len(fp.posts))
	}
	if err := l.SendTestDM(context.Background(), "U2"); err != nil {
		t.Fatalf("a test DM to a distinct target should send: %v", err)
	}
	if len(fp.posts) != 2 {
		t.Fatalf("a distinct-target test DM should reach Slack, got %d posts", len(fp.posts))
	}
}

func TestLinkerConfirmMarksLinkedAndEditsDM(t *testing.T) {
	fs := &fakeLinkerStore{confirmRows: 1}
	fp := &fakePoster{}
	l := NewLinker(fs, fp, nil)
	l.HandleBlockAction(context.Background(), BlockAction{
		SlackUserID: "U123", ActionID: ActionLinkConfirm, ChannelID: "D1", MessageTS: "ts1",
	})

	if len(fs.confirmedIDs) != 1 || fs.confirmedIDs[0] != "U123" {
		t.Fatalf("confirm must be scoped by the authenticated Slack id, got %v", fs.confirmedIDs)
	}
	if len(fp.updates) != 1 || fp.updates[0].ts != "ts1" || !strings.Contains(strings.ToLower(fp.updates[0].text), "linked") {
		t.Fatalf("DM not edited to a linked state: %+v", fp.updates)
	}
}

func TestLinkerConfirmAlreadyHandled(t *testing.T) {
	fs := &fakeLinkerStore{confirmRows: 0} // already confirmed / cleared
	fp := &fakePoster{}
	l := NewLinker(fs, fp, nil)
	l.HandleBlockAction(context.Background(), BlockAction{
		SlackUserID: "U123", ActionID: ActionLinkConfirm, ChannelID: "D1", MessageTS: "ts1",
	})
	if len(fp.updates) != 1 || !strings.Contains(strings.ToLower(fp.updates[0].text), "already handled") {
		t.Fatalf("0-rows confirm should edit to 'already handled': %+v", fp.updates)
	}
}

func TestLinkerRejectClearsLinkAndEditsDM(t *testing.T) {
	fs := &fakeLinkerStore{clearRows: 1}
	fp := &fakePoster{}
	l := NewLinker(fs, fp, nil)
	l.HandleBlockAction(context.Background(), BlockAction{
		SlackUserID: "U123", ActionID: ActionLinkReject, ChannelID: "D1", MessageTS: "ts1",
	})

	if len(fs.clearedIDs) != 1 || fs.clearedIDs[0] != "U123" {
		t.Fatalf("reject must clear the authenticated Slack id, got %v", fs.clearedIDs)
	}
	if len(fp.updates) != 1 || !strings.Contains(strings.ToLower(fp.updates[0].text), "removed") {
		t.Fatalf("DM not edited to a removed state: %+v", fp.updates)
	}
}

func TestLinkerHandleBlockActionIgnoresEmptyActorAndUnknownAction(t *testing.T) {
	fs := &fakeLinkerStore{confirmRows: 1, clearRows: 1}
	fp := &fakePoster{}
	l := NewLinker(fs, fp, nil)

	// No authenticated actor → no store touch.
	l.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "", ActionID: ActionLinkConfirm})
	// Unknown action (an M4 gate button) → ignored by the linker.
	l.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "U123", ActionID: "slack_gate_approve"})

	if len(fs.confirmedIDs) != 0 || len(fs.clearedIDs) != 0 {
		t.Fatalf("empty actor / unknown action must not touch the store: confirmed=%v cleared=%v", fs.confirmedIDs, fs.clearedIDs)
	}
}
