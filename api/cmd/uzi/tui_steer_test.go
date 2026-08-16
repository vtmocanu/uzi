package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #112 M4. The ownership gate is the finding most likely to be waved through, so
// it gets the most direct instrument: the surface must key off the OWNER-ONLY probe,
// never off "the run loaded".

func ownedRun(id string) apitypes.RunDTO {
	return apitypes.RunDTO{ID: id, Kind: "issue", Status: "running", Health: "ok"}
}

// notOwnerErr is what uzicli returns for the owner-only endpoint's 404.
func notOwnerErr() error { return uzicli.Exitf(uzicli.ExitNotFound, "run not found") }

func TestSteerAccessIsOwnershipNotVisibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  apitypes.RunDTO
		err  error
		want steerAccess
	}{
		// The owner: RunInputs succeeds, so SubmitInput's identical owner check will
		// too — same s.GetRun(ctx, userID, runID), not two rules that agree.
		{"owner", ownedRun("r1"), nil, steerAllowed},
		// THE TRAP. An admin observer LOADS the run fine (GetRunForViewer branches on
		// isAdmin) but ListRunInputs 404s them. Deciding from "did the run load" would
		// show a steer bar whose every call 404s.
		{"admin observing another user's run", ownedRun("r1"), notOwnerErr(), steerNotOwner},
		// Chat runs are watch-only REGARDLESS of ownership: SubmitRunInput does not
		// gate kind=chat, so a raw follow-up would inject into a chat outside the
		// guarded /chats path.
		{"chat run, even when owned", apitypes.RunDTO{ID: "r2", Kind: "chat", Status: "running"}, nil, steerChatRun},
		{"chat run, not owned", apitypes.RunDTO{ID: "r2", Kind: "chat", Status: "running"}, notOwnerErr(), steerChatRun},
		// A transport failure is NOT evidence about ownership. Fail closed: a hidden
		// bar is an inconvenience, a shown one that 404s is a lie.
		{"transport failure is not a verdict", ownedRun("r1"), uzicli.Exitf(uzicli.ExitUnreachable, "connection refused"), steerUnknown},
		{"a plain error is not a verdict", ownedRun("r1"), errors.New("boom"), steerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := steerAccessFor(tc.run, tc.err); got != tc.want {
				t.Errorf("steerAccessFor(kind=%s, err=%v) = %v, want %v", tc.run.Kind, tc.err, got, tc.want)
			}
		})
	}
}

// End to end through the model: a non-owner sees no steer controls, and their
// keystrokes are INERT — not merely unrendered.
func TestSteerBarIsSuppressedAndInertForANonOwner(t *testing.T) {
	runID := "r-observed"
	fake := &uzicli.FakeClient{}
	m := tuiTestModel(t, fake, runID)
	next, _ := m.Update(detailLoadedMsg{run: ownedRun(runID)})
	m = next.(tuiModel)
	next, _ = m.Update(runInputsMsg{runID: runID, err: notOwnerErr()})
	m = next.(tuiModel)

	if m.detail.steer.access != steerNotOwner {
		t.Fatalf("access = %v, want steerNotOwner", m.detail.steer.access)
	}
	out := m.View().Content
	if !strings.Contains(out, "read-only") {
		t.Errorf("a non-owner's view does not explain why steering is unavailable; a bar that vanishes with no reason reads as a bug\n%s", out)
	}
	// Assert on the returned COMMAND as well as the mode: handleKey only BUILDS the
	// command, so checking the fake's capture alone reads "" either way and cannot
	// distinguish "no submit was produced" from "one was produced but never run".
	for _, k := range []string{"f", "x", "y", "n"} {
		nm, cmd := m.handleKey(k)
		if cmd != nil {
			t.Errorf("key %q produced a command for a non-owner; every steer verb is owner-only and must be inert, not merely unrendered", k)
		}
		if nm.(tuiModel).detail.steer.mode != steerIdle {
			t.Errorf("key %q opened a steer mode for a non-owner; suppressing the RENDER is not enough", k)
		}
	}
	if fake.LastInputKind != "" {
		t.Errorf("a non-owner's keystrokes reached SubmitRunInput (kind=%q)", fake.LastInputKind)
	}
}

func TestSteerBarSuppressedForChatRuns(t *testing.T) {
	runID := "r-chat"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: apitypes.RunDTO{ID: runID, Kind: "chat", Status: "running"}})
	m = next.(tuiModel)
	// Owner: the inputs call SUCCEEDS, so only the chat-kind rule can suppress it.
	next, _ = m.Update(runInputsMsg{runID: runID})
	m = next.(tuiModel)

	if m.detail.steer.access != steerChatRun {
		t.Fatalf("access = %v, want steerChatRun even though the caller owns the run", m.detail.steer.access)
	}
	if !strings.Contains(m.View().Content, "chat runs are steered from the web") {
		t.Error("the chat-run suppression is not explained on screen")
	}
	nm, cmd := m.handleKey("f")
	if cmd != nil || nm.(tuiModel).detail.steer.mode != steerIdle {
		t.Error("a chat run accepted a follow-up; SubmitRunInput does not gate kind=chat, so a raw follow-up would inject outside the guarded /chats path")
	}
}

// The owner's happy path: follow-up types and submits.
func TestSteerFollowUpSubmits(t *testing.T) {
	runID := "r-own"
	fake := &uzicli.FakeClient{}
	m := ownerModel(t, fake, runID, ownedRun(runID))

	// The typed body is a value, not a phrase: keep ONE definition so a message can
	// never name text the test did not type.
	const wantBody = "hi there"
	m = press(t, m, "f")
	if m.detail.steer.mode != steerTyping {
		t.Fatal("f did not open the follow-up input")
	}
	for _, k := range []string{"h", "i", keySpaceName, "t", "h", "e", "r", "e"} {
		m = press(t, m, k)
	}
	if m.detail.steer.input != wantBody {
		t.Fatalf("typed input = %q, want %q", m.detail.steer.input, wantBody)
	}
	// Now press the ACTUAL focus-change keys while still typing. steerTyping must swallow
	// them (M4 N2), so the pane focus and lane stay put and the typed body is untouched.
	// Pressing them is what BINDS this: single-rune typing alone never challenges focus, so
	// without these presses the assertions are vacuous — a mutation leaking focus keys
	// during typing left this test green until it did. Assert after EACH press: →left then
	// tab would toggle the focus back to the rail and hide a leak.
	for _, k := range []string{keyRight, keyLeft, keyTab} {
		m = press(t, m, k)
		if m.detail.steer.mode != steerTyping {
			t.Fatalf("%q ended the follow-up typing; steerTyping must swallow focus keys", k)
		}
		if m.detail.focus != focusRail {
			t.Errorf("%q changed the pane focus while typing a follow-up (focus=%d); steerTyping must swallow it", k, m.detail.focus)
		}
		if m.detail.laneIdx != 0 {
			t.Errorf("%q moved the lane selection while typing a follow-up", k)
		}
	}
	if m.detail.steer.input != wantBody {
		t.Errorf("a swallowed focus key altered the typed input: %q", m.detail.steer.input)
	}

	_, cmd := m.handleKey(keyEnter)
	if cmd == nil {
		t.Fatal("enter did not submit the follow-up")
	}
	msg, ok := cmd().(steerResultMsg)
	if !ok {
		t.Fatalf("submit produced %T, want steerResultMsg", cmd())
	}
	if msg.kind != kindFollowUp {
		t.Errorf("submitted kind = %q, want %q", msg.kind, kindFollowUp)
	}
	if fake.LastInputBody != wantBody {
		t.Errorf("SubmitRunInput got body %q, want %q", fake.LastInputBody, wantBody)
	}
	// An EMPTY follow-up submits nothing — otherwise a stray enter queues a blank
	// steer the lead has to drain.
	m2 := ownerModel(t, &uzicli.FakeClient{}, runID, ownedRun(runID))
	m2 = press(t, m2, "f")
	if _, cmd := m2.handleKey(keyEnter); cmd != nil {
		t.Error("an empty follow-up submitted; a stray enter must not queue a blank steer")
	}
}

// ownerModel builds a detail model whose ownership probe has already succeeded.
func ownerModel(t *testing.T, c uzicli.Client, runID string, run apitypes.RunDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, c, runID)
	next, _ := m.Update(detailLoadedMsg{run: run})
	m = next.(tuiModel)
	next, _ = m.Update(runInputsMsg{runID: runID})
	m = next.(tuiModel)
	if m.detail.steer.access != steerAllowed {
		t.Fatalf("fixture is wrong: access = %v, want steerAllowed", m.detail.steer.access)
	}
	return m
}

// Both destructive verbs require an affirmative key. "Not escape" is not consent.
func TestSteerCancelAndRejectRequireConfirmation(t *testing.T) {
	runID := "r-own"

	// cancel
	fake := &uzicli.FakeClient{}
	m := ownerModel(t, fake, runID, ownedRun(runID))
	nm, cmd := m.handleKey("x")
	m = nm.(tuiModel)
	if cmd != nil {
		t.Fatal("x submitted a cancel immediately; a destructive verb must be confirmed first")
	}
	if m.detail.steer.mode != steerConfirming || m.detail.steer.pending != kindCancel {
		t.Fatalf("x did not open the cancel confirmation (mode=%v pending=%q)", m.detail.steer.mode, m.detail.steer.pending)
	}
	if !strings.Contains(m.View().Content, "cancel this run") {
		t.Error("the cancel confirmation does not name what it will do")
	}
	// Any non-affirmative key backs out, including keys that mean "yes" elsewhere.
	//
	// The assertion is on the returned COMMAND, not on the fake's capture: handleKey
	// only builds the command, so a capture-based check reads "" whether or not a
	// submit was produced — a hostile case that cannot reach its sink. An earlier
	// version of this loop did exactly that and the "any key confirms" mutation
	// survived it.
	for _, k := range []string{keyEsc, "x", keyEnter, "j", "n"} {
		nb, backCmd := m.handleKey(k)
		if backCmd != nil {
			t.Fatalf("key %q produced a command from the cancel confirmation; only the affirmative key may confirm a destructive verb", k)
		}
		if nb.(tuiModel).detail.steer.mode != steerIdle {
			t.Errorf("key %q left the cancel confirmation open", k)
		}
	}
	if fake.LastInputKind != "" {
		t.Fatalf("a non-affirmative key reached SubmitRunInput (kind=%q)", fake.LastInputKind)
	}
	// y confirms.
	_, cmd = m.handleKey(keyConfirmY)
	if cmd == nil {
		t.Fatal("y did not confirm the cancel")
	}
	if msg := cmd().(steerResultMsg); msg.kind != kindCancel {
		t.Errorf("confirmed kind = %q, want %q", msg.kind, kindCancel)
	}

	// reject, which is only offered at the gate
	gate := ownedRun(runID)
	gate.Status = "awaiting_approval"
	m = ownerModel(t, &uzicli.FakeClient{}, runID, gate)
	nm, cmd = m.handleKey(keyConfirmN)
	m = nm.(tuiModel)
	if cmd != nil {
		t.Fatal("n submitted a plan rejection immediately; it destroys a plan and must be confirmed")
	}
	if m.detail.steer.pending != kindRejectPlan {
		t.Fatalf("n did not open the reject confirmation (pending=%q)", m.detail.steer.pending)
	}
}

// Approve is NOT confirmed — it is the forward, non-destructive verb — but it is only
// offered while the run is actually at its gate.
func TestSteerApproveOnlyAtThePlanGate(t *testing.T) {
	runID := "r-own"

	running := ownerModel(t, &uzicli.FakeClient{}, runID, ownedRun(runID))
	if _, cmd := running.handleKey(keyConfirmY); cmd != nil {
		t.Error("y approved a run that is not at its plan gate")
	}

	gate := ownedRun(runID)
	gate.Status = "awaiting_approval"
	m := ownerModel(t, &uzicli.FakeClient{}, runID, gate)
	// M4: the plan-gate keys live in the one-line footer as bright-key hints (y approve /
	// n reject), so assert on the labels the footer draws.
	content := m.View().Content
	if !strings.Contains(content, "approve") || !strings.Contains(content, "reject") {
		t.Errorf("approve/reject are not offered at the plan gate\n%s", content)
	}
	_, cmd := m.handleKey(keyConfirmY)
	if cmd == nil {
		t.Fatal("y did not approve at the plan gate")
	}
	if msg := cmd().(steerResultMsg); msg.kind != kindApprovePlan {
		t.Errorf("approve submitted kind %q, want %q", msg.kind, kindApprovePlan)
	}
}

// The queued/delivered indicator uses the SHARED steerState + relAge helpers, so the
// TUI and `uzi run inputs` cannot disagree about what "delivered" means.
func TestSteerQueueIndicatorUsesTheSharedVocabulary(t *testing.T) {
	runID := "r-own"
	body := "please also update the docs"
	consumed := time.Now().Add(-time.Minute)
	m := ownerModel(t, &uzicli.FakeClient{}, runID, ownedRun(runID))

	next, _ := m.Update(runInputsMsg{runID: runID, inputs: []apitypes.SteerInputDTO{
		{ID: 1, Body: &body, CreatedAt: time.Now().Add(-2 * time.Minute)},
		{ID: 2, Body: &body, CreatedAt: time.Now().Add(-3 * time.Minute), ConsumedAt: &consumed},
	}})
	m = next.(tuiModel)

	out := m.View().Content
	// These strings come from steerState, not from this file — if the shared helper's
	// wording changes, this test follows it rather than pinning a stale copy.
	wantQueued := steerState(nil, "running")
	wantDelivered := steerState(&consumed, "running")
	if !strings.Contains(out, wantQueued) {
		t.Errorf("the queue indicator does not show %q\n%s", wantQueued, out)
	}
	if !strings.Contains(out, wantDelivered) {
		t.Errorf("the queue indicator does not show %q\n%s", wantDelivered, out)
	}
}

// An `input` frame is a prompt to RE-READ the queue: it carries no data by design,
// because the steer text is owner-gated and never rides the socket.
func TestInputFrameTriggersAQueueReRead(t *testing.T) {
	runID := "r-own"
	m := ownerModel(t, &uzicli.FakeClient{}, runID, ownedRun(runID))
	m.detail.stream = uzicli.NewRunStream(context.Background(), nil)
	defer m.detail.stream.Close()

	_, cmd := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
		{Type: uzicli.RunEventTypeInput},
	}})
	if cmd == nil {
		t.Fatal("an input frame produced no command; the steer queue would never refresh")
	}
	// A message frame alone must NOT trigger the re-read — otherwise every transcript
	// line costs an extra REST call.
	m2 := ownerModel(t, &uzicli.FakeClient{}, runID, ownedRun(runID))
	if changed := m2.detail.applyEvents([]apitypes.RunEventDTO{
		{Type: uzicli.RunEventTypeMessage, Seq: 1, Kind: "text"},
	}); changed {
		t.Error("a message frame reported the steer queue as changed; that would re-read the queue on every transcript line")
	}
}

// The outcome line reuses inputOutcome, so the TUI and the plain commands speak one
// vocabulary for what just happened.
func TestSteerOutcomeUsesTheSharedPhrasing(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	m.applySteerResult(steerResultMsg{kind: kindCancel, res: apitypes.RunInputResponse{ServerSide: true}})
	if want := inputOutcome(kindCancel, true); m.detail.steer.notice != want {
		t.Errorf("notice = %q, want the shared %q", m.detail.steer.notice, want)
	}
	m.applySteerResult(steerResultMsg{kind: kindFollowUp, err: uzicli.Exitf(uzicli.ExitConflict, "run finished")})
	if !strings.Contains(m.detail.steer.notice, "run finished") {
		t.Errorf("a failed steer did not surface the server's reason: %q", m.detail.steer.notice)
	}
}

// A FINISHED run must not offer verbs the server will refuse. SubmitInput's second
// statement is terminalStatuses[run.Status] -> ErrRunTerminal, so "x cancel run" on a
// completed run is the same lie the ownership gate exists to prevent, reached through a
// different predicate.
func TestSteerBarSuppressedOnATerminalRun(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			runID := "r-done"
			run := ownedRun(runID)
			run.Status = status

			fake := &uzicli.FakeClient{}
			m := tuiTestModel(t, fake, runID)
			next, _ := m.Update(detailLoadedMsg{run: run})
			m = next.(tuiModel)
			// The ownership probe SUCCEEDS — the caller does own it — so only the
			// terminal-status rule can suppress the bar.
			next, _ = m.Update(runInputsMsg{runID: runID})
			m = next.(tuiModel)

			if m.detail.steer.access != steerTerminal {
				t.Fatalf("access on a %s run = %v, want steerTerminal even though the caller owns it", status, m.detail.steer.access)
			}
			out := m.View().Content
			// The suppression message itself says "follow-ups, approvals and cancel are
			// refused", so a whole-frame Contains matches the explanation. Assert on the
			// FOOTER line instead (M4 one-line footer): a terminal run's footer is
			// navigation-only, so none of the action LABELS may appear there.
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			footer := lines[len(lines)-1]
			for _, label := range []string{"follow-up", "cancel", "review", "approve", "reject"} {
				if strings.Contains(footer, label) {
					t.Errorf("a %s run's footer still offers %q; every steer verb is refused server-side with ErrRunTerminal\nfooter: %q", status, label, footer)
				}
			}
			if !strings.Contains(out, "has finished") {
				t.Errorf("the terminal-run suppression is not explained on screen\n%s", out)
			}
			// And the keys are inert, not merely unrendered.
			for _, k := range []string{"f", "x", "y", "n"} {
				nm, cmd := m.handleKey(k)
				if cmd != nil {
					t.Errorf("key %q produced a command on a %s run", k, status)
				}
				if nm.(tuiModel).detail.steer.mode != steerIdle {
					t.Errorf("key %q opened a steer mode on a %s run", k, status)
				}
			}
		})
	}
}

// steerUnknown must not be a ONE-WAY door. If the first ownership probe fails for a
// reason that is not a 404 — an api restart, a transient 5xx — nothing else would ever
// ask again, and the bar would render "checking whether you can steer this run…" for
// the rest of the session: a message promising a check that cannot happen.
func TestSteerUnknownIsRetriedOnAStateFrame(t *testing.T) {
	runID := "r-blip"
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	next, _ := m.Update(detailLoadedMsg{run: ownedRun(runID)})
	m = next.(tuiModel)

	// A transport failure: not evidence about ownership, so the bar fails closed.
	next, _ = m.Update(runInputsMsg{runID: runID, err: uzicli.Exitf(uzicli.ExitUnreachable, "connection refused")})
	m = next.(tuiModel)
	if m.detail.steer.access != steerUnknown {
		t.Fatalf("access after a transport failure = %v, want steerUnknown (fail closed)", m.detail.steer.access)
	}
	if !strings.Contains(m.View().Content, "checking whether you can steer") {
		t.Fatal("the unknown state does not say it is still checking")
	}

	// A state frame must trigger a re-probe. Without this the message above is a
	// promise the model cannot keep.
	m.detail.stream = uzicli.NewRunStream(context.Background(), nil)
	defer m.detail.stream.Close()
	_, cmd := m.Update(streamEventsMsg{runID: runID, events: []apitypes.RunEventDTO{
		{Type: uzicli.RunEventTypeState, Status: "running"},
	}})
	if cmd == nil {
		t.Fatal("a state frame produced no command while access was unknown; the ownership probe would never be retried and the bar would say \"checking…\" forever")
	}

	// A MESSAGE frame alone must not retry — that would re-probe on every transcript
	// line, turning a recovery path into a per-frame REST call.
	//
	// Asserted on the PREDICATE rather than by invoking the returned command: the
	// non-retry command is readStreamCmd, which blocks until the stream produces an
	// event, so calling it here hangs the test rather than answering it. (It did: the
	// first version of this test timed out at 6m40s.) hasStateFrame is the whole
	// decision, so testing it directly is both faster and more precise than inferring
	// it from a command's behaviour.
	if hasStateFrame([]apitypes.RunEventDTO{{Type: uzicli.RunEventTypeMessage, Seq: 1, Kind: "text"}}) {
		t.Error("a message frame reported as a state frame; the ownership re-probe would fire on every transcript line")
	}
	if !hasStateFrame([]apitypes.RunEventDTO{
		{Type: uzicli.RunEventTypeMessage, Seq: 1},
		{Type: uzicli.RunEventTypeState, Status: "running"},
	}) {
		t.Error("a batch containing a state frame was not recognised; the retry would never fire")
	}
}
