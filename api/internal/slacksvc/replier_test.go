package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeReplierStore records the replier's confirmed-user lookups, anchor lookups,
// and gate writes.
type fakeReplierStore struct {
	user      store.User
	userErr   error
	anchor    store.SlackRunMessage
	anchorErr error
	gateSet   []store.SetSlackRunGateParams
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
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "approve or reject") {
		t.Fatalf("an open-gate reply must be nudged: %+v", fp.ephemerals)
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
