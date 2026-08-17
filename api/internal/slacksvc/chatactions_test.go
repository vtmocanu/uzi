package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeChatActionStore resolves the confirmed presser and records anchor inserts.
type fakeChatActionStore struct {
	user      store.User
	userErr   error
	anchors   []store.InsertSlackChatAnchorParams
	anchorErr error
}

func (f *fakeChatActionStore) GetConfirmedUserBySlackID(context.Context, pgtype.Text) (store.User, error) {
	return f.user, f.userErr
}
func (f *fakeChatActionStore) InsertSlackChatAnchor(_ context.Context, arg store.InsertSlackChatAnchorParams) (store.SlackRunMessage, error) {
	f.anchors = append(f.anchors, arg)
	return store.SlackRunMessage{RunID: arg.RunID, ChannelID: arg.ChannelID, RootTs: arg.RootTs, StatusTs: arg.StatusTs}, f.anchorErr
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

	cancels   []endCall
	cancelErr error

	ends         []endCall
	endErr       error
	continues    []endCall
	continuedRun uuid.UUID
	continueErr  error
	liveChatOK   bool // M6: LiveChatForUser (default false = no live chat)
	liveChatErr  error
}

type endCall struct {
	userID, runID uuid.UUID
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
func (f *fakeChatActionSubmitter) CancelRunFromCard(_ context.Context, userID, runID uuid.UUID) error {
	f.cancels = append(f.cancels, endCall{userID, runID})
	return f.cancelErr
}
func (f *fakeChatActionSubmitter) EndChat(_ context.Context, userID, runID uuid.UUID) error {
	f.ends = append(f.ends, endCall{userID, runID})
	return f.endErr
}
func (f *fakeChatActionSubmitter) ContinueChat(_ context.Context, userID, runID uuid.UUID) (uuid.UUID, error) {
	f.continues = append(f.continues, endCall{userID, runID})
	return f.continuedRun, f.continueErr
}
func (f *fakeChatActionSubmitter) LiveChatForUser(context.Context, uuid.UUID) (store.Run, bool, error) {
	return store.Run{}, f.liveChatOK, f.liveChatErr
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
		`{"id":"`+propID.String()+`","title":"tok glpat-ABCDEF1234567890abcd ping <@U9>",`+ //gitleaks:allow // fake PAT fixture: asserts a credential-shaped title is scrubbed, never a real secret
			`"description":"`+longDesc+`","labels":["glpat-ZZZZZZZZZZZZZZZZZZZZ"],"repo_path":"grp/repo"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want one card, got %+v", fp.blocks)
	}
	body := fp.blocks[0].sectionText
	if strings.Contains(body, "glpat-ABCDEF1234567890abcd") || strings.Contains(body, "glpat-ZZZZZZZZZZZZZZZZZZZZ") { //gitleaks:allow // fake PAT fixtures: asserts credential-shaped title/label are scrubbed, never a real secret
		t.Errorf("credential-shaped title/label must be scrubbed: %q", body[:min(200, len(body))])
	}
	if strings.Contains(body, "<@U9>") {
		t.Errorf("a mention in the title must be neutralized: %q", body[:min(200, len(body))])
	}
	if n := len([]rune(body)); n > maxSlackSectionRunes+2 { // +2 for the "\n…" ellipsis
		t.Errorf("assembled card section must be bounded to Slack's limit, got %d runes", n)
	}
}

// The proposal card's description field shares renderChatBody (PRD #292 M2), so a
// markdown description is RENDERED into Slack mrkdwn on the card — bold becomes *bold*,
// a list becomes • bullets — while an injected mention stays inert (SlackMrkdwn owns its
// escaping). The chrome title stays on cardField/EscapeMrkdwn (Decision 2), untouched.
func TestProposalCardRendersDescriptionMarkdown(t *testing.T) {
	runID, propID := uuid.New(), uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("proposal",
		`{"id":"`+propID.String()+`","title":"Add retries",`+
			`"description":"**do** this\n\n- one\n- two","repo_path":"grp/repo"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want one card, got %+v", fp.blocks)
	}
	body := fp.blocks[0].sectionText
	if !strings.Contains(body, "*do*") {
		t.Errorf("**do** in the description must render as *do*, got %q", body)
	}
	if !strings.Contains(body, "• one") || !strings.Contains(body, "• two") {
		t.Errorf("a markdown list in the description must render as • bullets, got %q", body)
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

// A cancel_request run message posts a Cancel/Dismiss card, threaded in the conversation,
// carrying the target run id (PRD #322).
func TestCancelRequestFramePostsCard(t *testing.T) {
	runID := uuid.New()
	targetRun := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("cancel_request", `{"run_id":"`+targetRun.String()+`"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want one card, got %+v", fp.blocks)
	}
	card := fp.blocks[0]
	if card.thread != "user1" {
		t.Errorf("card must thread on root_ts, got %q", card.thread)
	}
	ids := strings.Join(card.actionIDs, ",")
	if !strings.Contains(ids, ActionChatRunCancel) || !strings.Contains(ids, ActionChatRunCancelDismiss) {
		t.Errorf("card must carry Cancel + Dismiss, got %v", card.actionIDs)
	}
	if !strings.Contains(card.sectionText, targetRun.String()) {
		t.Errorf("card should name the target run id: %q", card.sectionText)
	}
}

// A malformed cancel_request (missing/invalid run_id) posts no card.
func TestCancelRequestFrameMalformedPostsNothing(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("cancel_request", `{"run_id":"not-a-uuid"}`))
	feed(n, runID, frame("cancel_request", `{}`))

	if len(fp.blocks) != 0 {
		t.Fatalf("a malformed cancel_request must post no card, got %+v", fp.blocks)
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

func lifecyclePress(actionID string, runID uuid.UUID) BlockAction {
	return BlockAction{SlackUserID: "Uauth", ActionID: actionID, Value: runID.String(), ChannelID: "D1", MessageTS: "status1"}
}

// cancelCardPress builds a cancel-run card press: the value is the bare run id (PRD #322).
func cancelCardPress(actionID string, runID uuid.UUID) BlockAction {
	return BlockAction{SlackUserID: "Uauth", ActionID: actionID, Value: runID.String(), ChannelID: "D1", MessageTS: "card1"}
}

// A Cancel press routes to the ownership-scoped CancelRunFromCard and, on success, edits
// the card button-free to the cancelled outcome. The value carries the bare run id.
func TestChatActionCancelRun(t *testing.T) {
	user := store.User{ID: uuid.New()}
	runID := uuid.New()
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: user}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), cancelCardPress(ActionChatRunCancel, runID))

	if len(sub.cancels) != 1 || sub.cancels[0].runID != runID || sub.cancels[0].userID != user.ID {
		t.Fatalf("cancel mis-routed: %+v", sub.cancels)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 {
		t.Fatalf("card should be edited button-free on cancel, got %+v", fp.updateBlocks)
	}
	if !strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "run cancelled") {
		t.Errorf("resolved card should say the run was cancelled: %q", fp.updateBlocks[0].sectionText)
	}
}

// A refused Cancel (a foreign/terminal run) surfaces the user-safe message as an
// ephemeral and leaves the card pressable.
func TestChatActionCancelRunRefused(t *testing.T) {
	sub := &fakeChatActionSubmitter{cancelErr: errors.New("That run has already finished.")}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), cancelCardPress(ActionChatRunCancel, uuid.New()))

	if len(fp.updateBlocks) != 0 {
		t.Errorf("a refused cancel must not edit the card, got %+v", fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(fp.ephemerals[0].text, "already finished") {
		t.Fatalf("want the refusal reason as an ephemeral, got %+v", fp.ephemerals)
	}
}

// A malformed (non-UUID) cancel value calls nothing and posts nothing.
func TestChatActionCancelRunMalformedValue(t *testing.T) {
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), BlockAction{
		SlackUserID: "Uauth", ActionID: ActionChatRunCancel, Value: "not-a-uuid", ChannelID: "D1", MessageTS: "card1",
	})

	if len(sub.cancels) != 0 {
		t.Errorf("a malformed value must reach no submitter call, got %+v", sub.cancels)
	}
	if len(fp.updateBlocks) != 0 || len(fp.ephemerals) != 0 {
		t.Fatalf("a malformed value must post nothing: edits=%v eph=%v", fp.updateBlocks, fp.ephemerals)
	}
}

// Dismiss on a cancel card cancels nothing and just edits the card away.
func TestChatActionCancelRunDismiss(t *testing.T) {
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), cancelCardPress(ActionChatRunCancelDismiss, uuid.New()))

	if len(sub.cancels) != 0 {
		t.Errorf("dismiss must cancel no run, got %+v", sub.cancels)
	}
	if len(fp.updateBlocks) != 1 {
		t.Fatalf("dismiss should edit the card, got %+v", fp.updateBlocks)
	}
}

// End routes to EndChat and edits the status message to an "ending" state.
func TestChatActionEndChat(t *testing.T) {
	user := store.User{ID: uuid.New()}
	runID := uuid.New()
	sub := &fakeChatActionSubmitter{}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: user}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatEnd, runID))

	if len(sub.ends) != 1 || sub.ends[0].runID != runID || sub.ends[0].userID != user.ID {
		t.Fatalf("end mis-routed: %+v", sub.ends)
	}
	// End confirms via an ephemeral and does NOT edit status_ts (M2b's terminal
	// transition owns that edit and swaps in Continue) — avoids a two-goroutine race.
	if len(fp.updateBlocks) != 0 {
		t.Errorf("End must not edit the status message (M2b owns it), got %+v", fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "ending") {
		t.Fatalf("End should confirm via an ephemeral, got %+v", fp.ephemerals)
	}
}

// A second Continue while the first resumed run is still live is refused (idempotent
// double-press): mints nothing. Serial socket processing + this live-chat check give
// exactly-one-per-press.
func TestChatActionContinueRefusedWhileLive(t *testing.T) {
	sub := &fakeChatActionSubmitter{liveChatOK: true} // a live chat already exists
	fp := &fakePoster{}
	fs := &fakeChatActionStore{user: store.User{ID: uuid.New()}}
	c := NewChatActions(fs, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatContinue, uuid.New()))

	if len(sub.continues) != 0 {
		t.Errorf("Continue must mint nothing while a chat is live, got %+v", sub.continues)
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("want one 'already have a live chat' ephemeral, got %+v", fp.ephemerals)
	}
}

// The Continue spend guard refuses when over budget, minting nothing.
func TestChatActionContinueSpendGuard(t *testing.T) {
	sub := &fakeChatActionSubmitter{continuedRun: uuid.New()}
	fp := &fakePoster{}
	fs := &fakeChatActionStore{user: store.User{ID: uuid.New()}}
	c := NewChatActions(fs, sub, fp, fixedBase, nil)
	c.SetChatSpendGuard(func(uuid.UUID) bool { return false }) // over budget

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatContinue, uuid.New()))

	if len(sub.continues) != 0 {
		t.Errorf("a rate-limited Continue must mint nothing, got %+v", sub.continues)
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("want one rate-limit ephemeral, got %+v", fp.ephemerals)
	}
}

// End on an already-terminal chat is an ephemeral, no edit.
func TestChatActionEndAlreadyEnded(t *testing.T) {
	sub := &fakeChatActionSubmitter{endErr: ErrChatEnded}
	fp := &fakePoster{}
	c := NewChatActions(&fakeChatActionStore{user: store.User{ID: uuid.New()}}, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatEnd, uuid.New()))

	if len(fp.updateBlocks) != 0 || len(fp.ephemerals) != 1 {
		t.Fatalf("already-ended End should ephemeral, not edit: edits=%v eph=%v", fp.updateBlocks, fp.ephemerals)
	}
}

// Continue mints exactly one resumed run, posts + anchors a NEW status message, and
// edits the old card away.
func TestChatActionContinueMintsOneRunAndAnchors(t *testing.T) {
	user := store.User{ID: uuid.New()}
	oldRun, newRun := uuid.New(), uuid.New()
	sub := &fakeChatActionSubmitter{continuedRun: newRun}
	fs := &fakeChatActionStore{user: user}
	fp := &fakePoster{}
	c := NewChatActions(fs, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatContinue, oldRun))

	if len(sub.continues) != 1 || sub.continues[0].runID != oldRun {
		t.Fatalf("continue mis-routed: %+v", sub.continues)
	}
	// A NEW top-level status message (blocks with End) for the resumed conversation.
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "" || strings.Join(fp.blocks[0].actionIDs, ",") != ActionChatEnd {
		t.Fatalf("continue should post a new top-level status with End, got %+v", fp.blocks)
	}
	// Anchored on that new bot message (root_ts == status_ts == the posted ts).
	if len(fs.anchors) != 1 || fs.anchors[0].RunID != newRun || fs.anchors[0].RootTs != "ts1" || fs.anchors[0].StatusTs.String != "ts1" {
		t.Fatalf("resumed run must be anchored on the new status message, got %+v", fs.anchors)
	}
	// The old card is edited button-free.
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 {
		t.Fatalf("the old status card should be edited button-free, got %+v", fp.updateBlocks)
	}
}

// Continue on a still-active chat is refused with an ephemeral, mints nothing.
func TestChatActionContinueStillActive(t *testing.T) {
	sub := &fakeChatActionSubmitter{continueErr: ErrChatNotEndedYet}
	fp := &fakePoster{}
	fs := &fakeChatActionStore{user: store.User{ID: uuid.New()}}
	c := NewChatActions(fs, sub, fp, fixedBase, nil)

	c.HandleBlockAction(context.Background(), lifecyclePress(ActionChatContinue, uuid.New()))

	if len(fp.blocks) != 0 || len(fs.anchors) != 0 {
		t.Errorf("a refused Continue must post/anchor nothing")
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("want one ephemeral, got %+v", fp.ephemerals)
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
