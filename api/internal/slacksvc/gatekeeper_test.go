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

// fakeGateStore records the gatekeeper's confirmed-user lookups and gate writes.
type fakeGateStore struct {
	user    store.User
	userErr error
	gateSet []store.SetSlackRunGateParams
}

func (f *fakeGateStore) GetConfirmedUserBySlackID(context.Context, pgtype.Text) (store.User, error) {
	return f.user, f.userErr
}
func (f *fakeGateStore) SetSlackRunGate(_ context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error) {
	f.gateSet = append(f.gateSet, arg)
	return store.SlackRunMessage{}, nil
}

type submittedInput struct {
	userID, runID uuid.UUID
	kind, body    string
}

// fakeSubmitter stands in for workersvc: it serves one run (for the stale check)
// and records the steering inputs the gatekeeper submits.
type fakeSubmitter struct {
	run       store.Run
	runErr    error
	submitted []submittedInput
	submitErr error
}

func (f *fakeSubmitter) GetRun(context.Context, uuid.UUID, uuid.UUID) (store.Run, error) {
	return f.run, f.runErr
}
func (f *fakeSubmitter) SubmitInput(_ context.Context, userID, runID uuid.UUID, kind, body string) error {
	f.submitted = append(f.submitted, submittedInput{userID, runID, kind, body})
	return f.submitErr
}

func gateAction(actionID string, runID uuid.UUID) BlockAction {
	return BlockAction{SlackUserID: "Uauth", ActionID: actionID, Value: runID.String(), ChannelID: "D1", MessageTS: "gts"}
}

func awaitingRun(runID, userID uuid.UUID) store.Run {
	return store.Run{ID: runID, UserID: userID, Status: "awaiting_approval"}
}

func TestGatekeeperApproveSubmitsAndResolves(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	gs := &fakeGateStore{user: user}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateApprove, runID))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "approve_plan" ||
		sub.submitted[0].userID != user.ID || sub.submitted[0].runID != runID {
		t.Fatalf("approve must submit approve_plan for the resolved user+run: %+v", sub.submitted)
	}
	if len(gs.gateSet) != 1 || gs.gateSet[0].GateTs.Valid || gs.gateSet[0].GateState.Valid {
		t.Fatalf("approve must clear the gate anchor: %+v", gs.gateSet)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 ||
		!strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "approved") {
		t.Fatalf("gate message must be edited button-free to an approved state: %+v", fp.updateBlocks)
	}
}

func TestGatekeeperRejectEntersRejectPending(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	gs := &fakeGateStore{user: user}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateReject, runID))

	if len(sub.submitted) != 0 {
		t.Fatalf("plain Reject must NOT submit yet (it awaits a reason): %+v", sub.submitted)
	}
	if len(gs.gateSet) != 1 || gs.gateSet[0].GateState.String != gateStateRejectPending || !gs.gateSet[0].GateTs.Valid {
		t.Fatalf("Reject must record reject_pending keeping gate_ts: %+v", gs.gateSet)
	}
	if len(fp.updateBlocks) != 1 || !containsID(fp.updateBlocks[0].actionIDs, ActionGateRejectNoReason) {
		t.Fatalf("Reject must edit the gate to offer 'Reject without reason': %+v", fp.updateBlocks)
	}
}

func TestGatekeeperRejectNoReasonSubmitsAndResolves(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	gs := &fakeGateStore{user: user}
	sub := &fakeSubmitter{run: awaitingRun(runID, user.ID)}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateRejectNoReason, runID))

	if len(sub.submitted) != 1 || sub.submitted[0].kind != "reject_plan" || sub.submitted[0].body != "" {
		t.Fatalf("reject-without-reason must submit reject_plan with an empty body: %+v", sub.submitted)
	}
	if len(gs.gateSet) != 1 || gs.gateSet[0].GateTs.Valid {
		t.Fatalf("reject-without-reason must clear the gate anchor: %+v", gs.gateSet)
	}
	if len(fp.updateBlocks) != 1 || len(fp.updateBlocks[0].actionIDs) != 0 ||
		!strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "rejected") {
		t.Fatalf("gate message must be edited button-free to a rejected state: %+v", fp.updateBlocks)
	}
}

// A click on a run that already left awaiting_approval (resolved from another
// surface) submits nothing and gets an ephemeral "already handled" notice.
func TestGatekeeperStaleClickIsEphemeral(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	gs := &fakeGateStore{user: user}
	sub := &fakeSubmitter{run: store.Run{ID: runID, UserID: user.ID, Status: "running"}}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateApprove, runID))

	if len(sub.submitted) != 0 || len(gs.gateSet) != 0 || len(fp.updateBlocks) != 0 {
		t.Fatalf("a stale click must not submit, edit, or write the gate: submitted=%v gate=%v edits=%v",
			sub.submitted, gs.gateSet, fp.updateBlocks)
	}
	if len(fp.ephemerals) != 1 || fp.ephemerals[0].user != "Uauth" ||
		!strings.Contains(strings.ToLower(fp.ephemerals[0].text), "already handled") {
		t.Fatalf("a stale click must get an 'already handled' ephemeral to the actor: %+v", fp.ephemerals)
	}
}

func TestGatekeeperUnlinkedUserIsEphemeral(t *testing.T) {
	runID := uuid.New()
	gs := &fakeGateStore{userErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateApprove, runID))

	if len(sub.submitted) != 0 {
		t.Fatalf("an unlinked actor must never submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "linked") {
		t.Fatalf("an unlinked actor must get a 'not linked' ephemeral: %+v", fp.ephemerals)
	}
}

func TestGatekeeperRunNotOwnedIsEphemeral(t *testing.T) {
	runID, user := uuid.New(), store.User{ID: uuid.New()}
	gs := &fakeGateStore{user: user}
	sub := &fakeSubmitter{runErr: pgx.ErrNoRows} // ownership-scoped GetRun found nothing
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), gateAction(ActionGateApprove, runID))

	if len(sub.submitted) != 0 {
		t.Fatalf("a run the actor doesn't own must never submit: %+v", sub.submitted)
	}
	if len(fp.ephemerals) != 1 || !strings.Contains(strings.ToLower(fp.ephemerals[0].text), "isn't yours") {
		t.Fatalf("a not-owned run must get an ephemeral refusal: %+v", fp.ephemerals)
	}
}

// The gatekeeper owns only slack_gate_* actions: link actions and the Open-in-uzi
// url button are ignored (they never even resolve a user).
func TestGatekeeperIgnoresNonGateActions(t *testing.T) {
	runID := uuid.New()
	gs := &fakeGateStore{userErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	g := NewGatekeeper(gs, sub, fp, nil)

	g.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "Uauth", ActionID: ActionLinkConfirm, Value: runID.String()})
	g.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "Uauth", ActionID: ActionGateOpen, Value: runID.String()})
	// Empty actor on a real gate action is also a no-op.
	g.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "", ActionID: ActionGateApprove, Value: runID.String()})

	if len(sub.submitted) != 0 || len(gs.gateSet) != 0 || len(fp.ephemerals) != 0 || len(fp.updateBlocks) != 0 {
		t.Fatalf("non-gate actions / empty actor must be inert: submitted=%v gate=%v eph=%v edits=%v",
			sub.submitted, gs.gateSet, fp.ephemerals, fp.updateBlocks)
	}
}

// InboundMux delivers each action to every handler; disjoint action-id namespaces
// mean exactly one acts, but the fan-out itself must reach all of them.
func TestInboundMuxFansOutToAll(t *testing.T) {
	a, b := &recordingInbound{}, &recordingInbound{}
	mux := InboundMux{a, nil, b} // nil handlers are skipped
	mux.HandleBlockAction(context.Background(), BlockAction{SlackUserID: "U", ActionID: "x"})
	if len(a.actions) != 1 || len(b.actions) != 1 {
		t.Fatalf("mux must fan out to every non-nil handler: a=%d b=%d", len(a.actions), len(b.actions))
	}
}

func containsID(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
