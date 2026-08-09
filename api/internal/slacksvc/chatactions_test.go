package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeChatActionStore resolves the confirmed presser.
type fakeChatActionStore struct {
	user    store.User
	userErr error
}

func (f *fakeChatActionStore) GetConfirmedUserBySlackID(context.Context, pgtype.Text) (store.User, error) {
	return f.user, f.userErr
}

// fakeChatActionSubmitter records the composite proposal ops and returns staged results.
type fakeChatActionSubmitter struct {
	confirms   []confirmCall
	confirmed  CreatedIssue
	confirmErr error
	dismisses  []confirmCall
	dismissErr error

	starts     []startCall
	startedRun uuid.UUID
	startErr   error
}

type confirmCall struct{ userID, runID, propID uuid.UUID }

type startCall struct {
	userID   uuid.UUID
	repoPath string
	issueIID int64
}

func (f *fakeChatActionSubmitter) ConfirmProposalForUser(_ context.Context, userID, runID, propID uuid.UUID) (CreatedIssue, error) {
	f.confirms = append(f.confirms, confirmCall{userID, runID, propID})
	return f.confirmed, f.confirmErr
}
func (f *fakeChatActionSubmitter) DismissProposalForUser(_ context.Context, userID, runID, propID uuid.UUID) error {
	f.dismisses = append(f.dismisses, confirmCall{userID, runID, propID})
	return f.dismissErr
}
func (f *fakeChatActionSubmitter) StartRunFromCard(_ context.Context, userID uuid.UUID, repoPath string, issueIID int64) (uuid.UUID, error) {
	f.starts = append(f.starts, startCall{userID, repoPath, issueIID})
	return f.startedRun, f.startErr
}

func runCardPress(actionID, repoPath string, issueIID int64) BlockAction {
	return BlockAction{
		SlackUserID: "Uauth", ActionID: actionID,
		Value: encodeRunReqValue(repoPath, issueIID), ChannelID: "D1", MessageTS: "card1",
	}
}

func chatActionPress(actionID string, runID, propID uuid.UUID) BlockAction {
	return BlockAction{
		SlackUserID: "Uauth", ActionID: actionID,
		Value: encodeChatValue(runID, propID), ChannelID: "D1", MessageTS: "card1",
	}
}

// A proposal run message posts a Block Kit card with Create + Dismiss, threaded in the
// conversation, with the title escaped in the body.
func TestProposalFramePostsCard(t *testing.T) {
	runID := uuid.New()
	propID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("proposal",
		`{"id":"`+propID.String()+`","title":"Add retries","description":"back off and retry","labels":["bug"],"repo_path":"grp/repo"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want exactly one card posted, got %+v", fp.blocks)
	}
	card := fp.blocks[0]
	if card.thread != "user1" {
		t.Errorf("card must thread on root_ts, got %q", card.thread)
	}
	ids := strings.Join(card.actionIDs, ",")
	if !strings.Contains(ids, ActionChatProposalCreate) || !strings.Contains(ids, ActionChatProposalDismiss) {
		t.Errorf("card must carry Create + Dismiss actions, got %v", card.actionIDs)
	}
	if !strings.Contains(card.sectionText, "Add retries") {
		t.Errorf("card body should name the proposal title: %q", card.sectionText)
	}
}

// A credential-shaped string in a model-authored title/label is scrubbed on the card
// (not just markup-escaped), and a mention is neutralized. The whole assembled section
// stays within Slack's section limit even with a maximal description + title + labels.
func TestProposalCardScrubsAndBounds(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	longDesc := strings.Repeat("x", 5000)
	feed(n, runID, frame("proposal",
		`{"id":"`+propID.String()+`","title":"tok glpat-ABCDEF1234567890abcd ping <@U9>",`+
			`"description":"`+longDesc+`","labels":["glpat-ZZZZZZZZZZZZZZZZZZZZ"],"repo_path":"grp/repo"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want one card, got %+v", fp.blocks)
	}
	body := fp.blocks[0].sectionText
	if strings.Contains(body, "glpat-ABCDEF1234567890abcd") || strings.Contains(body, "glpat-ZZZZZZZZZZZZZZZZZZZZ") {
		t.Errorf("credential-shaped title/label must be scrubbed: %q", body[:min(200, len(body))])
	}
	if strings.Contains(body, "<@U9>") {
		t.Errorf("a mention in the title must be neutralized: %q", body[:min(200, len(body))])
	}
	if n := len([]rune(body)); n > maxSlackSectionRunes+2 { // +2 for the "\n…" ellipsis
		t.Errorf("assembled card section must be bounded to Slack's limit, got %d runes", n)
	}
}

// Create files the issue via the ownership-scoped service and edits the card to the
// created issue; a SECOND press (already resolved) gets the already-handled edit and
// files nothing more (claim-first → exactly one issue).
func TestChatActionCreateFilesOnceThenHandled(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	user := store.User{ID: uuid.New()}
	sub := &fakeChatActionSubmitter{confirmed: CreatedIssue{IID: 7, WebURL: "https://f/-/issues/7", Title: "Add retries"}}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: user}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalCreate, runID, propID))

	if len(sub.confirms) != 1 {
		t.Fatalf("want one confirm, got %d", len(sub.confirms))
	}
	if sub.confirms[0].userID != user.ID || sub.confirms[0].runID != runID || sub.confirms[0].propID != propID {
		t.Errorf("confirm mis-routed: %+v", sub.confirms[0])
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 {
		t.Fatalf("card must be edited button-free to the outcome, got %+v", fp.updateBlocks)
	}
	if !strings.Contains(fp.updateBlocks[0].sectionText, "#7") {
		t.Errorf("resolved card should name the created issue: %q", fp.updateBlocks[0].sectionText)
	}

	// Second press: already resolved.
	sub.confirmErr = ErrChatProposalHandled
	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalCreate, runID, propID))
	if len(fp.updateBlocks) != 2 || !strings.Contains(strings.ToLower(fp.updateBlocks[1].sectionText), "already handled") {
		t.Fatalf("second press should get an already-handled edit, got %+v", fp.updateBlocks)
	}
}

// A press by a non-owner files nothing and gets an ephemeral, no card edit.
func TestChatActionCreateNonOwnerRefused(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	sub := &fakeChatActionSubmitter{confirmErr: ErrChatProposalGone}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalCreate, runID, propID))

	if len(fp.updateBlocks) != 0 {
		t.Errorf("a refused press must not edit the card, got %+v", fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("want one ephemeral notice, got %+v", fp.ephemerals)
	}
}

// A forge failure (the service reverted the proposal to pending) gets an ephemeral
// telling the user to retry, and does not edit the card away.
func TestChatActionCreateForgeFailureOffersRetry(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	sub := &fakeChatActionSubmitter{confirmErr: ErrChatProposalForge}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalCreate, runID, propID))

	if len(fp.updateBlocks) != 0 {
		t.Errorf("a forge failure must leave the card pressable, got %+v", fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "pending") {
		t.Fatalf("want a retry ephemeral naming the pending revert, got %+v", fp.ephemerals)
	}
}

// Dismiss drops the proposal (never touches the forge) and edits the card.
func TestChatActionDismiss(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	user := store.User{ID: uuid.New()}
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: user}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalDismiss, runID, propID))

	if len(sub.dismisses) != 1 || len(sub.confirms) != 0 {
		t.Fatalf("dismiss must call dismiss only (no forge confirm): dismiss=%d confirm=%d", len(sub.dismisses), len(sub.confirms))
	}
	if len(fp.updateBlocks) != 1 || !strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "dismiss") {
		t.Fatalf("dismiss should edit the card, got %+v", fp.updateBlocks)
	}
}

// An unlinked presser is refused with the link notice, and no service call is made.
func TestChatActionUnlinkedPresserRefused(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{userErr: pgx.ErrNoRows}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), chatActionPress(ActionChatProposalCreate, runID, propID))

	if len(sub.confirms) != 0 || len(sub.dismisses) != 0 {
		t.Errorf("an unlinked presser must reach no service call")
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("want one link-account ephemeral, got %+v", fp.ephemerals)
	}
}

// A run_request run message posts a Start/Dismiss card with the issue iid and repo,
// scrubbed and inert.
func TestRunRequestFramePostsCard(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("run_request",
		`{"repo_path":"grp/repo","issue_iid":42,"title":"speed up the poller"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want one card, got %+v", fp.blocks)
	}
	card := fp.blocks[0]
	ids := strings.Join(card.actionIDs, ",")
	if !strings.Contains(ids, ActionChatRunStart) || !strings.Contains(ids, ActionChatRunDismiss) {
		t.Errorf("card must carry Start + Dismiss, got %v", card.actionIDs)
	}
	if !strings.Contains(card.sectionText, "#42") || !strings.Contains(card.sectionText, "grp/repo") {
		t.Errorf("card should name the issue and repo: %q", card.sectionText)
	}
}

// A malformed run_request (missing repo/iid) posts no card.
func TestRunRequestFrameMalformedPostsNothing(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)
	feed(n, runID, frame("run_request", `{"repo_path":"","issue_iid":0}`))
	if len(fp.blocks) != 0 {
		t.Fatalf("a malformed run_request must post no card, got %+v", fp.blocks)
	}
}

// Start routes to the ownership-scoped StartRunFromCard and, on success, edits the card
// to "run started" with the run link. The value carries (repo_path, issue_iid).
func TestChatActionStartRun(t *testing.T) {
	user := store.User{ID: uuid.New()}
	newRun := uuid.New()
	sub := &fakeChatActionSubmitter{startedRun: newRun}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: user}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), runCardPress(ActionChatRunStart, "grp/repo", 42))

	if len(sub.starts) != 1 || sub.starts[0].repoPath != "grp/repo" || sub.starts[0].issueIID != 42 || sub.starts[0].userID != user.ID {
		t.Fatalf("start mis-routed: %+v", sub.starts)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 {
		t.Fatalf("card should be edited button-free on start, got %+v", fp.updateBlocks)
	}
	if !strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "run started") {
		t.Errorf("resolved card should say the run started: %q", fp.updateBlocks[0].sectionText)
	}
}

// A refused Start (a gate reason) surfaces the user-safe message and leaves the card.
func TestChatActionStartRunRefused(t *testing.T) {
	sub := &fakeChatActionSubmitter{startErr: errors.New("This issue has no PRD link — add a prds/*.md link before starting a run.")}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), runCardPress(ActionChatRunStart, "grp/repo", 42))

	if len(fp.updateBlocks) != 0 {
		t.Errorf("a refused start must not edit the card, got %+v", fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(fp.ephemerals[0].text, "PRD link") {
		t.Fatalf("want the gate reason as an ephemeral, got %+v", fp.ephemerals)
	}
}

// Dismiss on a run card starts nothing and just edits the card.
func TestChatActionRunDismiss(t *testing.T) {
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), runCardPress(ActionChatRunDismiss, "grp/repo", 42))

	if len(sub.starts) != 0 {
		t.Errorf("dismiss must start no run, got %+v", sub.starts)
	}
	if len(fp.updateBlocks) != 1 {
		t.Fatalf("dismiss should edit the card, got %+v", fp.updateBlocks)
	}
}

// A non-chat action id is ignored (the mux fans every action to every handler).
func TestChatActionIgnoresForeignNamespace(t *testing.T) {
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "Uauth", ActionID: ActionGateApprove, Value: uuid.New().String()})

	if len(sub.confirms) != 0 || len(sub.dismisses) != 0 || len(fp.ephemerals) != 0 {
		t.Errorf("a slack_gate_* action must be ignored by ChatActions")
	}
}
