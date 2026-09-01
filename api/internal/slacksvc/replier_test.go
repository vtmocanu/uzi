package slacksvc

import (
	"context"

	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeReplierStore records the replier's confirmed-user lookups, anchor lookups,
// and gate writes.
type fakeReplierStore struct {
	user      store.User
	userErr   error
	anchor    store.SlackRunMessage
	anchorErr error
	gateSet   []store.SetSlackRunGateParams

	// CAS state (PRD #41 Decision 10c): SetSlackRunGateIf models an atomic
	// compare-and-swap — the FIRST caller finding the expected state wins (returns a
	// row); every later caller sees the state changed and gets pgx.ErrNoRows. casErr,
	// when set, is returned to every caller (a DB error path).
	mu        sync.Mutex
	gateSetIf []store.SetSlackRunGateIfParams
	casWon    int
	casErr    error

	// PRD #191 M2: chat anchor inserts.
	chatAnchors   []store.InsertSlackChatAnchorParams
	chatAnchorErr error
}

func (f *fakeReplierStore) GetConfirmedUserBySlackID(context.Context, pgtype.Text) (store.User, error) {
	return f.user, f.userErr
}
func (f *fakeReplierStore) GetSlackRunMessageByRoot(context.Context, store.GetSlackRunMessageByRootParams) (store.SlackRunMessage, error) {
	return f.anchor, f.anchorErr
}
func (f *fakeReplierStore) SetSlackRunGate(_ context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error) {
	f.gateSet = append(f.gateSet, arg)
	return store.SlackRunMessage{}, nil
}
func (f *fakeReplierStore) InsertSlackChatAnchor(_ context.Context, arg store.InsertSlackChatAnchorParams) (store.SlackRunMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatAnchors = append(f.chatAnchors, arg)
	if f.chatAnchorErr != nil {
		return store.SlackRunMessage{}, f.chatAnchorErr
	}
	return store.SlackRunMessage{RunID: arg.RunID, ChannelID: arg.ChannelID, RootTs: arg.RootTs, StatusTs: arg.StatusTs}, nil
}
func (f *fakeReplierStore) SetSlackRunGateIf(_ context.Context, arg store.SetSlackRunGateIfParams) (store.SlackRunMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateSetIf = append(f.gateSetIf, arg)
	if f.casErr != nil {
		return store.SlackRunMessage{}, f.casErr
	}
	if f.casWon > 0 {
		return store.SlackRunMessage{}, pgx.ErrNoRows // already taken → loser
	}
	f.casWon++
	return store.SlackRunMessage{RunID: arg.RunID}, nil // winner
}

func reply(text string) MessageReply {
	return MessageReply{SlackUserID: "Uauth", ChannelID: "D1", ThreadTS: "root1", MessageTS: "reply1", Text: text}
}

func anchorRow(runID uuid.UUID, gateState string) store.SlackRunMessage {
	m := store.SlackRunMessage{RunID: runID, ChannelID: "D1", RootTs: "root1"}
	if gateState != "" {
		m.GateTs = pgtype.Text{String: "gate1", Valid: true}
		m.GateState = pgtype.Text{String: gateState, Valid: true}
	}
	return m
}

func liveRun(runID, userID uuid.UUID, status string) store.Run {
	return store.Run{ID: runID, UserID: userID, Status: status}
}

// A reply while the gate is reject-pending IS the reasoned rejection: it submits
// reject_plan with the reply text, resolves the gate button-free, acks, and never
// echoes the reason back to Slack.
func TestReplierRejectPendingSubmitsReasonedReject(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRejectPending)}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("wrong approach, use pgx"))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "reject_plan" ||
		sub.submitted[0].body != "wrong approach, use pgx" ||
		sub.submitted[0].userID != user.ID || sub.submitted[0].runID != runID {
		t.Fatalf("reject-pending reply must submit reject_plan with the reason: %+v", sub.submitted)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 {
		t.Fatalf("gate must be edited button-free: %+v", fp.updateBlocks)
	}
	if strings.Contains(fp.updateBlocks[0].sectionText, "wrong approach") {
		t.Fatalf("the rejection reason must NOT be echoed back to Slack: %q", fp.updateBlocks[0].sectionText)
	}
	if len(fs.gateSet) != 1 || fs.gateSet[0].GateTs.Valid {
		t.Fatalf("gate anchor must be cleared: %+v", fs.gateSet)
	}
	if len(fp.reactions) != 1 || fp.reactions[0].ts != "reply1" || fp.reactions[0].emoji != ackReaction {
		t.Fatalf("accepted reply must get a ✅ ack: %+v", fp.reactions)
	}
}

// A reply arriving in the reject-pending window AFTER the run already left the
// gate (resolved from another surface, anchor not yet cleared by the notifier)
// must NOT submit a stale reject_plan — the run.Status guard makes it fall through
// to follow_up on a still-live run.
func TestReplierRejectPendingStaleRunFallsThroughToFollowUp(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRejectPending)} // stale reject-pending anchor
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}                        // run already moved on
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("late rejection reason"))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" || sub.submitted[0].body != "late rejection reason" {
		t.Fatalf("a stale reject-pending reply on a running run must fall through to follow_up, not reject_plan: %+v", sub.submitted)
	}
	if len(fp.updateBlocks) != 0 || len(fs.gateSet) != 0 {
		t.Fatalf("a stale reply must not resolve the gate: edits=%v gate=%v", fp.updateBlocks, fs.gateSet)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("the fallen-through follow_up should still be acked: %+v", fp.reactions)
	}
}

// Same race but the run already finished: falls through to the finished ephemeral,
// still no stale reject_plan.
func TestReplierRejectPendingStaleTerminalRunIsFinished(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRejectPending)}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "completed")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("late rejection reason"))

	if len(sub.submitted) != 0 {
		t.Fatalf("a stale reject-pending reply on a finished run must not submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "already finished") {
		t.Fatalf("stale reject-pending on a terminal run → finished ephemeral: %+v", fp.ephemerals)
	}
}

// A reply to an interactive task PARKED in awaiting_followup (PRD #517) is that follow-up:
// it submits a `follow_up` steering input (which resumes the parked run) and is acked. It
// must NOT go through the answer path (there is no clarification question) — that is the
// discriminating half from awaiting_input. Reddening mutation: remove the awaiting_followup
// arm from the reply switch → it still submits follow_up via the default, so this test
// stays green; its value is pinning that awaiting_followup routes to follow_up (an answer/
// nudge/finished disposition would fail it) rather than proving the arm's existence.
func TestReplierAwaitingFollowupSubmitsFollowUp(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "awaiting_followup")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("now add the integration test"))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" || sub.submitted[0].body != "now add the integration test" {
		t.Fatalf("a reply to an awaiting_followup park must submit follow_up to resume it: %+v", sub.submitted)
	}
	if len(sub.answers) != 0 {
		t.Fatalf("an awaiting_followup reply must NOT go through the answer path (no clarification question): %+v", sub.answers)
	}
	if len(fp.reactions) != 1 || fp.reactions[0].emoji != ackReaction {
		t.Fatalf("an accepted follow-up must be acked: %+v", fp.reactions)
	}
	if len(fp.ephemerals) != 0 || len(fs.gateSet) != 0 {
		t.Fatalf("an awaiting_followup reply must not ephemeral or touch the gate: eph=%v gate=%v", fp.ephemerals, fs.gateSet)
	}
}

// A reply on a live run with no open gate becomes a follow_up.
func TestReplierLiveRunSubmitsFollowUp(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("also update the docs"))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" || sub.submitted[0].body != "also update the docs" {
		t.Fatalf("a live-run reply must submit follow_up: %+v", sub.submitted)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("a follow_up must get a ✅ ack: %+v", fp.reactions)
	}
	if len(fp.ephemerals) != 0 || len(fs.gateSet) != 0 {
		t.Fatalf("a live-run reply must not ephemeral or touch the gate: eph=%v gate=%v", fp.ephemerals, fs.gateSet)
	}
}

// With a pending steer armed for (channel, thread, user), a thread reply is submitted as
// a follow_up to the pending's TARGET run via SteerRunFromCard — NOT as a chat turn on
// the anchored chat run, and NOT as a follow_up on the anchored run. The reply is acked.
func TestReplierSteerPendingSubmitsToTarget(t *testing.T) {
	user := store.User{ID: uuid.New()}
	targetRun := uuid.New()
	// The anchor at this thread is the CHAT run (the steer card lives in the chat thread);
	// the interception must beat the chat-turn arm and route to the target instead.
	chatRun := uuid.New()
	fs := &fakeReplierStore{
		user:   user,
		anchor: store.SlackRunMessage{RunID: chatRun, ChannelID: "D1", RootTs: "root1"},
	}
	sub := &fakeSubmitter{run: store.Run{ID: chatRun, UserID: user.ID, Kind: runKindChat, Status: "running"}}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	steer := NewSteerPendings()
	steer.Arm("D1", "root1", targetRun, user.ID)
	r.SetSteerPending(steer)

	r.HandleMessage(context.Background(), reply("focus on the auth path"))

	if len(sub.steers) != 1 || sub.steers[0].runID != targetRun || sub.steers[0].message != "focus on the auth path" || sub.steers[0].userID != user.ID {
		t.Fatalf("the reply must steer the TARGET run: %+v", sub.steers)
	}
	if len(sub.chatTurns) != 0 {
		t.Errorf("a steer reply must NOT become a chat turn, got %+v", sub.chatTurns)
	}
	if len(sub.submitted) != 0 {
		t.Errorf("a steer reply must NOT submit a follow_up on the anchor, got %+v", sub.submitted)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("a consumed steer must get a ✅ ack: %+v", fp.reactions)
	}
	if _, ok := steer.Take("D1", "root1", user.ID); ok {
		t.Errorf("the pending must have been consumed (one-shot)")
	}
}

// A steer whose target is a chat run is refused by the service (ErrChatInputNotAllowed →
// the adapter's "issue runs" message); the replier surfaces it as an ephemeral, no ack.
func TestReplierSteerChatTargetSurfacesMessage(t *testing.T) {
	user := store.User{ID: uuid.New()}
	targetRun := uuid.New()
	fs := &fakeReplierStore{user: user, anchor: store.SlackRunMessage{RunID: uuid.New(), ChannelID: "D1", RootTs: "root1"}}
	sub := &fakeSubmitter{
		run:      store.Run{ID: targetRun, UserID: user.ID, Kind: runKindChat, Status: "running"},
		steerErr: errors.New("Steering applies to issue runs, not chats — reply in the chat itself to continue it."),
	}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	steer := NewSteerPendings()
	steer.Arm("D1", "root1", targetRun, user.ID)
	r.SetSteerPending(steer)

	r.HandleMessage(context.Background(), reply("do the thing"))

	if len(fp.reactions) != 0 {
		t.Errorf("a refused steer must not be acked, got %+v", fp.reactions)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(fp.ephemerals[0].text, "issue runs") {
		t.Fatalf("want the 'issue runs' message as an ephemeral, got %+v", fp.ephemerals)
	}
}

// With no live pending, a reply falls through to normal handling (a live-run follow_up on
// the anchor), untouched by the steer path.
func TestReplierNoPendingFallsThrough(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	r.SetSteerPending(NewSteerPendings()) // empty registry

	r.HandleMessage(context.Background(), reply("also update the docs"))

	if len(sub.steers) != 0 {
		t.Errorf("no pending must mean no steer submission, got %+v", sub.steers)
	}
	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" {
		t.Fatalf("the reply must fall through to a normal follow_up: %+v", sub.submitted)
	}
}

// An expired pending is not consumed — the reply falls through to normal handling.
func TestReplierExpiredPendingFallsThrough(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	now := time.Now()
	steer := NewSteerPendings()
	steer.now = func() time.Time { return now }
	steer.Arm("D1", "root1", uuid.New(), user.ID)
	now = now.Add(steerPendingTTL + time.Second) // the pending has since expired
	r.SetSteerPending(steer)

	r.HandleMessage(context.Background(), reply("late instruction"))

	if len(sub.steers) != 0 {
		t.Errorf("an expired pending must not steer, got %+v", sub.steers)
	}
	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" {
		t.Fatalf("an expired pending must fall through to a normal follow_up: %+v", sub.submitted)
	}
}

// A pending armed for a DIFFERENT user is not consumed by this user's reply — it falls
// through, and the pending stays armed for its real requester.
func TestReplierSteerWrongUserDoesNotConsume(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	steer := NewSteerPendings()
	other := uuid.New()
	steer.Arm("D1", "root1", uuid.New(), other) // armed for someone else
	r.SetSteerPending(steer)

	r.HandleMessage(context.Background(), reply("not mine to steer"))

	if len(sub.steers) != 0 {
		t.Errorf("a wrong-user reply must not steer, got %+v", sub.steers)
	}
	if len(sub.submitted) != 1 || sub.submitted[0].kind != "follow_up" {
		t.Fatalf("a wrong-user reply must fall through to a normal follow_up: %+v", sub.submitted)
	}
	if _, ok := steer.Take("D1", "root1", other); !ok {
		t.Errorf("the pending must remain armed for its real requester")
	}
}

// A scrub-to-empty reply does NOT consume the pending — it stays armed for the real
// instruction (PRD "an empty reply is a no-op").
func TestReplierEmptySteerReplyLeavesPendingArmed(t *testing.T) {
	user := store.User{ID: uuid.New()}
	targetRun := uuid.New()
	// No anchor at this thread, so once the empty reply skips the steer branch it resolves
	// to nothing and returns — isolating the "pending untouched" assertion.
	fs := &fakeReplierStore{user: user, anchorErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	steer := NewSteerPendings()
	steer.Arm("D1", "root1", targetRun, user.ID)
	r.SetSteerPending(steer)

	r.HandleMessage(context.Background(), reply("   ")) // whitespace → boundReply ""

	if len(sub.steers) != 0 {
		t.Errorf("an empty reply must not steer, got %+v", sub.steers)
	}
	got, ok := steer.Take("D1", "root1", user.ID)
	if !ok || got != targetRun {
		t.Fatalf("an empty reply must leave the pending armed: got=%v ok=%v", got, ok)
	}
}

// A bare reply while the gate is OPEN (not reject-pending) is nudged, never
// submitted (the worker would queue a follow_up rather than treat it as a verdict).
func TestReplierOpenGateNudges(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateOpen)}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("please approve"))

	if len(sub.submitted) != 0 || len(fp.reactions) != 0 {
		t.Fatalf("a bare reply during an open gate must not submit or ack: submitted=%v acks=%v", sub.submitted, fp.reactions)
	}
	nudge := strings.ToLower(fp.ephemerals[0].text)
	if len(fp.ephemerals) != 1 || !strings.Contains(nudge, "request changes") || !strings.Contains(nudge, "reject") || !strings.Contains(nudge, "approve") {
		t.Fatalf("an open-gate reply must be nudged naming all three actions: %+v", fp.ephemerals)
	}
}

// A reply while the gate is revise-pending IS the revision feedback: it submits
// revise_plan with the reply text, acks, and edits the message to a neutral
// "Revising…" — but does NOT resolve the gate (the run stays parked for the next plan
// version). The accept rides a compare-and-swap so exactly one reply wins.
func TestReplierRevisePendingSubmitsRevise(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRevisePending)}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("use a worker pool instead"))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "revise_plan" ||
		sub.submitted[0].body != "use a worker pool instead" ||
		sub.submitted[0].userID != user.ID || sub.submitted[0].runID != runID {
		t.Fatalf("revise-pending reply must submit revise_plan with the feedback: %+v", sub.submitted)
	}
	if len(fs.gateSetIf) != 1 || fs.gateSetIf[0].ExpectedGateState.String != gateStateRevisePending ||
		fs.gateSetIf[0].GateState.Valid {
		t.Fatalf("revise accept must CAS-clear revise_pending: %+v", fs.gateSetIf)
	}
	if len(fs.gateSet) != 0 {
		t.Fatalf("revise accept must NOT unconditionally clear the gate (no resolveGate): %+v", fs.gateSet)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 ||
		!strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "revising") {
		t.Fatalf("gate message must be edited to a neutral 'Revising…' state: %+v", fp.updateBlocks)
	}
	if strings.Contains(fp.updateBlocks[0].sectionText, "worker pool") {
		t.Fatalf("the feedback must NOT be echoed back to Slack: %q", fp.updateBlocks[0].sectionText)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("an accepted revise reply must get a ✅ ack: %+v", fp.reactions)
	}
}

// Two revise-pending replies (modelled as concurrent via the CAS fake) → exactly ONE
// revise_plan submitted; the loser is nudged, never a second submit (PRD #41 Decision
// 10c single-winner).
func TestReplierRevisePendingSingleWinner(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRevisePending)}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("first feedback"))
	r.HandleMessage(context.Background(), reply("second feedback"))

	if len(sub.submitted) != 1 || sub.submitted[0].body != "first feedback" {
		t.Fatalf("exactly one revise must be submitted (the CAS winner): %+v", sub.submitted)
	}
	if len(fs.gateSetIf) != 2 {
		t.Fatalf("both replies must attempt the CAS: %+v", fs.gateSetIf)
	}
	if len(fp.reactions) != 1 {
		t.Fatalf("only the winning reply is acked: %+v", fp.reactions)
	}
	// The loser falls through to the open-gate nudge.
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "request changes") {
		t.Fatalf("the CAS loser must be nudged, not submitted: %+v", fp.ephemerals)
	}
}

// Hitting the server-side revision cap: the CAS wins but SubmitInput returns
// ErrReviseCapReached — the reply is NOT acked, and the user is told the limit was
// reached (PRD #41).
func TestReplierRevisePendingCapReached(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, gateStateRevisePending)}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID), submitErr: ErrReviseCapReached}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("one more change please"))

	if len(fp.reactions) != 0 {
		t.Fatalf("a cap-refused revise must NOT be acked: %+v", fp.reactions)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "limit") {
		t.Fatalf("a cap-refused revise must tell the user the limit was reached: %+v", fp.ephemerals)
	}
}

func TestReplierFinishedRunEphemeral(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "completed")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("thanks"))

	if len(sub.submitted) != 0 {
		t.Fatalf("a finished-run reply must not submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "already finished") {
		t.Fatalf("a finished-run reply must get an ephemeral: %+v", fp.ephemerals)
	}
}

// An unlinked author is told once, not per reply (coalesced), and nothing submits.
func TestReplierUnlinkedIsCoalesced(t *testing.T) {
	fs := &fakeReplierStore{userErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("hi"))
	r.HandleMessage(context.Background(), reply("still there?"))

	if len(sub.submitted) != 0 {
		t.Fatalf("an unlinked author must never submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "linked") {
		t.Fatalf("an unlinked author must be told exactly once (coalesced): %+v", fp.ephemerals)
	}
}

// A confirmed author who doesn't own the anchored run (ownership-scoped GetRun
// finds nothing) is refused, nothing submitted.
func TestReplierNotOwnedIsRefused(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{runErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("approve pls"))

	if len(sub.submitted) != 0 {
		t.Fatalf("a non-owned run must never submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "isn't yours") {
		t.Fatalf("a non-owned run must be refused: %+v", fp.ephemerals)
	}
}

// A deactivated (but previously confirmed) user cannot reply-act on a run either:
// the guarded GetConfirmedUserBySlackID returns no row, so the reply is refused and
// nothing is submitted. Same chokepoint as the gate path; the SQL filter is pinned
// by the live-DB TestSlackConfirmedLookupSkipsDeactivatedLiveDB.
func TestReplierDeactivatedUserRefused(t *testing.T) {
	fs := &fakeReplierStore{userErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("approve"))

	if len(sub.submitted) != 0 {
		t.Fatalf("a deactivated user must never submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 {
		t.Fatalf("a deactivated user's reply must be refused with an ephemeral: %+v", fp.ephemerals)
	}
}

// The per-Slack-user flood limit drops events beyond the budget.
func TestReplierInboundFloodLimit(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	for i := 0; i < inboundMax; i++ {
		r.HandleMessage(context.Background(), reply("x"))
	}
	if len(sub.submitted) != inboundMax {
		t.Fatalf("the budget of %d replies should all submit, got %d", inboundMax, len(sub.submitted))
	}
	r.HandleMessage(context.Background(), reply("one too many"))
	if len(sub.submitted) != inboundMax {
		t.Fatalf("the reply beyond the budget must be dropped, got %d submits", len(sub.submitted))
	}
}

// A credential a user pastes into a reply is scrubbed before it becomes
// worker-bound input (defense in depth — it must not land in run inputs or agent
// context). The reply never goes back out to Slack.
func TestReplierScrubsSecretsFromReply(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	// A ≥16-char body is required since PRD #954 widened the GitLab scrub pattern; assembled
	// from two halves so no token-shaped literal sits in source (gitleaks' gitlab-pat rule and
	// GitHub push protection both blocked the contiguous form, 2026-09-01).
	const fakeGitLabPAT = "glpat-" + "notARealValue" + "0123456"
	r.HandleMessage(context.Background(), reply("token is "+fakeGitLabPAT+", use it"))

	if len(sub.submitted) != 1 {
		t.Fatalf("want one submit, got %d", len(sub.submitted))
	}
	if strings.Contains(sub.submitted[0].body, fakeGitLabPAT) {
		t.Fatalf("a credential in the reply must be scrubbed before the worker sees it: %q", sub.submitted[0].body)
	}
	if !strings.Contains(sub.submitted[0].body, "[redacted]") {
		t.Fatalf("scrubbed reply should carry the redaction marker: %q", sub.submitted[0].body)
	}
}

func TestReplierBoundsReplyLength(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: anchorRow(runID, "")}
	sub := &fakeSubmitter{run: liveRun(runID, user.ID, "running")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply(strings.Repeat("a", maxReplyRunes+500)))

	if len(sub.submitted) != 1 || len([]rune(sub.submitted[0].body)) != maxReplyRunes {
		t.Fatalf("an over-long reply must be capped to %d runes, got %d", maxReplyRunes, len([]rune(sub.submitted[0].body)))
	}
}

// --- PRD #88 M3: answering a clarification question from Slack ----------------

// Slack ts values are "<epoch-seconds>.<microseconds>". These are ordered around one
// question card so the tests read as "before" / "after" rather than as digits.
const (
	questionCardTS = "1700000100.000200"
	beforeCardTS   = "1700000099.999999"
	afterCardTS    = "1700000100.000201"
)

// questionAnchor is a run's DM anchor carrying a posted question card: the question's
// identity and the ts of the message that delivered it. Both are needed — the id is
// what the answer names, the ts is what an inbound reply is ordered against.
func questionAnchor(runID uuid.UUID, questionID, questionTS string) store.SlackRunMessage {
	m := anchorRow(runID, "")
	if questionID != "" {
		m.QuestionID = pgtype.Text{String: questionID, Valid: true}
	}
	if questionTS != "" {
		m.QuestionTs = pgtype.Text{String: questionTS, Valid: true}
	}
	return m
}

func replyAt(text, ts string) MessageReply {
	m := reply(text)
	m.MessageTS = ts
	return m
}

func parkedRun(runID, userID uuid.UUID) store.Run {
	return liveRun(runID, userID, "awaiting_input")
}

// A reply that FOLLOWS the question card is the answer to that question: it submits
// through SubmitAnswer naming the id the user was actually shown, and is acked.
//
// The kind is the discriminating half — the default arm would have made this a
// follow_up, which the worker queues for an implement turn that never arrives, because
// the run is parked waiting for the answer that just became a follow-up.
func TestReplierAwaitingInputSubmitsAnswer(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
	sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), replyAt("use redis, not memcached", afterCardTS))

	if len(sub.answers) != 1 {
		t.Fatalf("a reply to a parked run must submit an answer: answers=%+v inputs=%+v", sub.answers, sub.submitted)
	}
	if len(sub.submitted) != 0 {
		t.Fatalf("it must NOT fall through to the default follow_up arm: %+v", sub.submitted)
	}
	got := sub.answers[0]
	if got.questionID != "q-9" {
		t.Fatalf("the answer must name the question the user was shown: %+v", got)
	}
	if got.text != "use redis, not memcached" || got.userID != user.ID || got.runID != runID {
		t.Fatalf("answer must carry the reply text for the resolved user + anchored run: %+v", got)
	}
	if len(fp.reactions) != 1 || fp.reactions[0].emoji != ackReaction {
		t.Fatalf("an accepted answer must be acked: %+v", fp.reactions)
	}
}

// 🔴 D-E case 1, the race identity keying exists to close. A reply written against an
// EARLIER question — recognisable only by arriving before the current card — must be
// DISCARDED. The refuted derivation ("whichever question is open now") would have
// stamped it with the live question's id server-side, and it would then have passed
// every downstream equality check precisely because the server supplied the id.
//
// The boundary is pinned rather than sampled: equal ts is also refused, since a message
// cannot follow itself, and that is what discriminates `>` from `>=`.
func TestReplierAwaitingInputOrdersReplyAgainstTheQuestionCard(t *testing.T) {
	cases := []struct {
		name     string
		replyTS  string
		accepted bool
	}{
		{"after the card answers it", afterCardTS, true},
		{"before the card is a superseded question", beforeCardTS, false},
		{"the card's own ts is not after itself", questionCardTS, false},
		{"an unparseable reply ts cannot be ordered", "not-a-ts", false},
		// ParseFloat accepts these with a NIL error, and +Inf beats every card ts — an
		// error-only guard would let them through.
		{"an infinite reply ts cannot be ordered", "Inf", false},
		{"a NaN reply ts cannot be ordered", "NaN", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, user := uuid.New(), store.User{ID: uuid.New()}
			fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
			sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
			fp := &fakePoster{}
			r := NewReplier(fs, sub, fp, nil)

			r.HandleMessage(context.Background(), replyAt("use redis", tc.replyTS))

			if tc.accepted {
				if len(sub.answers) != 1 || len(fp.reactions) != 1 {
					t.Fatalf("want the answer submitted and acked: answers=%+v acks=%+v", sub.answers, fp.reactions)
				}
				return
			}
			if len(sub.answers) != 0 || len(sub.submitted) != 0 {
				t.Fatalf("a reply that does not follow the card must not be submitted at all: answers=%+v inputs=%+v",
					sub.answers, sub.submitted)
			}
			if len(fp.reactions) != 0 {
				t.Fatalf("a discarded reply must NOT be acked — the ✅ would claim it was recorded: %+v", fp.reactions)
			}
			if len(fp.ephemerals) != 1 {
				t.Fatalf("the user must be told their reply was not taken as the answer: %+v", fp.ephemerals)
			}
		})
	}
}

// An unparseable ts on the ANCHOR side fails closed too. Same rule, stated separately
// because it is the half a reader is likely to assume is impossible: the ordering is
// what binds an answer to a question, so an ordering that cannot be established must
// refuse rather than default to accepting.
func TestReplierAwaitingInputFailsClosedOnUnorderableCard(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", "garbage")}
	sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), replyAt("use redis", afterCardTS))

	if len(sub.answers) != 0 || len(fp.reactions) != 0 {
		t.Fatalf("an unorderable card must refuse: answers=%+v acks=%+v", sub.answers, fp.reactions)
	}
}

// No question card recorded on this thread — the post failed, or the state report
// outran the question message. There is nothing to bind the reply to, and the one
// thing this path must never do is guess.
func TestReplierAwaitingInputWithNoPostedQuestionTellsTheUser(t *testing.T) {
	for _, tc := range []struct {
		name     string
		qid, qts string
	}{
		{"nothing recorded", "", ""},
		{"id without a ts", "q-9", ""},
		{"ts without an id", "", questionCardTS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, user := uuid.New(), store.User{ID: uuid.New()}
			fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, tc.qid, tc.qts)}
			sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
			fp := &fakePoster{}
			r := NewReplier(fs, sub, fp, nil)

			r.HandleMessage(context.Background(), replyAt("use redis", afterCardTS))

			if len(sub.answers) != 0 || len(sub.submitted) != 0 {
				t.Fatalf("no card ⇒ nothing to answer: answers=%+v inputs=%+v", sub.answers, sub.submitted)
			}
			if len(fp.ephemerals) != 1 || len(fp.reactions) != 0 {
				t.Fatalf("want one notice and no ack: eph=%+v acks=%+v", fp.ephemerals, fp.reactions)
			}
		})
	}
}

// An empty reply (a file- or emoji-only message) must not resolve the question with
// nothing — the run would continue on no information at all.
func TestReplierAwaitingInputIgnoresEmptyReply(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
	sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), replyAt("   ", afterCardTS))

	if len(sub.answers) != 0 {
		t.Fatalf("an empty reply must not be submitted as an answer: %+v", sub.answers)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(fp.ephemerals[0].text, "no text") {
		t.Fatalf("the user must be told the reply was empty: %+v", fp.ephemerals)
	}
	if len(fp.reactions) != 0 {
		t.Fatalf("nothing was accepted, so nothing is acked: %+v", fp.reactions)
	}
}

// The run can leave the question between the status read and the submit — another
// surface answered, the deadline fired, the run was cancelled. The server's sentinels
// are translated so the user gets an accurate notice and NO ✅: a checkmark for an
// answer the server rejected would be a lie, and this feature already carries one
// documented false confirmation (the mixed-fleet case) without adding a second.
func TestReplierAwaitingInputSurfacesALostRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"another surface answered first", ErrAnswerStale},
		{"the run stopped waiting", ErrNotAwaitingInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, user := uuid.New(), store.User{ID: uuid.New()}
			fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
			sub := &fakeSubmitter{run: parkedRun(runID, user.ID), answerErr: tc.err}
			fp := &fakePoster{}
			r := NewReplier(fs, sub, fp, nil)

			r.HandleMessage(context.Background(), replyAt("use redis", afterCardTS))

			if len(fp.ephemerals) != 1 || !strings.Contains(fp.ephemerals[0].text, "moved on") {
				t.Fatalf("a lost race must be surfaced, not dropped: %+v", fp.ephemerals)
			}
			if len(fp.reactions) != 0 {
				t.Fatalf("a rejected answer must NOT be acked: %+v", fp.reactions)
			}
		})
	}
}

// Any other submit failure still gets a notice and no ack, so a reply is never dropped
// in silence.
func TestReplierAwaitingInputSurfacesAGenericFailure(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
	sub := &fakeSubmitter{run: parkedRun(runID, user.ID), answerErr: errors.New("boom")}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), replyAt("use redis", afterCardTS))

	if len(fp.ephemerals) != 1 || len(fp.reactions) != 0 {
		t.Fatalf("want one notice and no ack: eph=%+v acks=%+v", fp.ephemerals, fp.reactions)
	}
}

// A credential pasted into an answer is scrubbed on the Slack path before it reaches
// the submitter, the same as every other accepted reply. (workersvc scrubs again for
// the web/CLI paths — this pins the Slack half.)
func TestReplierAwaitingInputScrubsAnswer(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, anchor: questionAnchor(runID, "q-9", questionCardTS)}
	sub := &fakeSubmitter{run: parkedRun(runID, user.ID)}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), replyAt("use glpat-supersecretvalue for it", afterCardTS))

	if len(sub.answers) != 1 || strings.Contains(sub.answers[0].text, "glpat-supersecretvalue") {
		t.Fatalf("a credential in an answer must be scrubbed before it leaves Slack: %+v", sub.answers)
	}
}
