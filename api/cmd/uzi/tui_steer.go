package main

import (
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The steer bar (PRD #112 M4, D4/D8): run-level steering only — follow_up,
// approve_plan, reject_plan, cancel. There is no wire to address one live subagent,
// so the TUI observes per-agent and steers the run.

// steerMode is what the bar is currently doing.
type steerMode int

const (
	steerIdle steerMode = iota
	steerTyping
	steerConfirming
)

// steerAccess is whether the steer surface may be shown at all, and WHY not when it
// may not. The reason is rendered: a suppressed bar with no explanation reads as a
// bug, and the two reasons have genuinely different remedies.
type steerAccess int

const (
	// steerUnknown is the state before the ownership probe answers. The bar is hidden
	// rather than optimistically shown — showing controls that then vanish is worse
	// than showing them a beat late.
	steerUnknown steerAccess = iota
	steerAllowed
	// steerNotOwner: visible but not steerable. An admin observing another user's run.
	steerNotOwner
	// steerChatRun: chat runs are watch-only in the TUI.
	steerChatRun
	// steerTerminal: the run has finished, so every verb is refused server-side.
	steerTerminal
)

type steerState2 struct {
	access steerAccess
	mode   steerMode
	input  string
	// pending is the verb awaiting confirmation (cancel / reject_plan).
	pending string
	// queue is the follow-up steer queue (PRD #95), refreshed off the `input` frame.
	queue []apitypes.SteerInputDTO
	// notice is the last outcome or error line.
	notice string
}

// ---- the ownership gate ---------------------------------------------------

// THE GATE. The steer surface is shown only when the caller OWNS the run, never
// merely because the run loaded.
//
// The trap it avoids: GetRunForViewer branches on isAdmin (workersvc/service.go:2138-2150),
// so an admin observer loads another user's run perfectly well. Deciding "can I steer"
// from "did the run load" would show a full steer bar to someone whose every steer call
// 404s.
//
// HOW OWNERSHIP IS DETERMINED, and why not by comparing ids: no run DTO carries the
// owner. RunDTO has no user id at all and RunListItemDTO carries only OwnerEmail, which
// is `omitempty` and populated on the ADMIN list alone — so `run.UserID == caller.ID`
// cannot be computed on this wire.
//
// Instead the server is ASKED, using the endpoint that shares the write's predicate.
// ListFollowUpInputs resolves ownership with `s.GetRun(ctx, userID, runID)`
// (service.go:2128-2132) and SubmitInput's FIRST statement is the same
// `s.GetRun(ctx, userID, runID)` (service.go:2239) — one function, not two that agree.
// So RunInputs succeeding is not an approximation of "may steer": it is the same
// predicate the write will evaluate, evaluated by the same code. A client-side id
// comparison would be a SECOND copy of that rule, free to drift from it — the exact
// failure D6 exists to prevent.
//
// A 404 here means "not the owner" rather than "no such run", because the run has
// already loaded through the viewer path by this point: visibility is established, so
// the only thing the owner-only endpoint can be refusing is ownership.
func steerAccessFor(run apitypes.RunDTO, inputsErr error) steerAccess {
	// Chat runs are watch-only regardless of ownership. A raw follow-up on a chat run
	// is now rejected at the service boundary (SubmitInput -> ErrChatInputNotAllowed,
	// #192), and chat turns belong on the guarded, cookie-only /chats path anyway — so
	// the TUI must not offer the steer affordance at all.
	if run.Kind == "chat" {
		return steerChatRun
	}
	// A finished run refuses every verb: SubmitInput's SECOND statement is
	// terminalStatuses[run.Status] -> ErrRunTerminal (service.go:2243). Offering
	// "x cancel run" on a completed run is the same lie this function exists to
	// prevent — a shown control whose call cannot succeed — reached through a
	// different predicate than ownership.
	if isTerminalRunStatus(run.Status) {
		return steerTerminal
	}
	if inputsErr == nil {
		return steerAllowed
	}
	var ex *uzicli.ExitError
	if errors.As(inputsErr, &ex) && ex.Code == uzicli.ExitNotFound {
		return steerNotOwner
	}
	// Any other failure (transport, 5xx) is not evidence about ownership, so the bar
	// stays hidden until a later probe answers. Failing CLOSED is the right direction:
	// a hidden bar is an inconvenience, a shown one that 404s is a lie.
	//
	// It must not be a ONE-WAY door, though, and it was. The three fetchInputsCmd call
	// sites are the initial load, a steer result (only reachable once steering already
	// works) and the `input`-frame path, which is itself gated on steerAllowed — so an
	// api restart or a transient 5xx on the FIRST probe pinned access here for the rest
	// of the session while the bar rendered "checking whether you can steer this run…",
	// a message promising a check that could never run again. A `state` frame now
	// re-probes while access is unknown; see the streamEventsMsg handler.
	return steerUnknown
}

// steerSuppressedReason is the line rendered in place of the bar.
func steerSuppressedReason(a steerAccess) string {
	switch a {
	case steerNotOwner:
		return "read-only: you can watch this run but not steer it — follow-ups, approvals and cancel are owner-only"
	case steerChatRun:
		return "read-only: chat runs are steered from the web chat surface, not here"
	case steerTerminal:
		return "read-only: this run has finished — follow-ups, approvals and cancel are refused once a run is terminal"
	default:
		return "checking whether you can steer this run…"
	}
}

// ---- queue indicator ------------------------------------------------------

// renderSteerQueue is the queued/delivered indicator (PRD #95), built on the SAME
// steerState + relAge helpers `uzi run inputs` prints, so the two cannot disagree
// about what "delivered" means.
func (m tuiModel) renderSteerQueue() string {
	q := m.detail.steer.queue
	if len(q) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(m.pal.faint.Render("STEER QUEUE") + "\n")
	for _, in := range q {
		body := "-"
		if in.Body != nil {
			// The user's own follow-up text, but it still goes through the cell path:
			// it round-trips through the server and a terminal cannot tell whose
			// bytes these are.
			body = m.renderer.Plain(*in.Body, 48)
		}
		sb.WriteString("  " + m.pal.faint.Render(padCell(steerKindLabel(in.Kind), 10)+" "+
			padCell(steerState(in.Kind, in.ConsumedAt, in.Disposition, m.detail.run.Status), 30)+" "+
			padCell(relAge(in.CreatedAt), 5)) + " " + body + "\n")
	}
	return sb.String()
}

// ---- rendering ------------------------------------------------------------

// atPlanGate reports whether the run is parked at its approval gate, which is the only
// state where approve/reject are meaningful.
func atPlanGate(run apitypes.RunDTO) bool { return run.Status == "awaiting_approval" }

func (m tuiModel) renderSteerBar() string {
	s := &m.detail.steer
	if s.access != steerAllowed {
		return m.pal.faint.Render(steerSuppressedReason(s.access))
	}

	var sb strings.Builder
	if q := m.renderSteerQueue(); q != "" {
		sb.WriteString(q)
	}
	if s.notice != "" {
		sb.WriteString(m.pal.faint.Render(cellText(s.notice)) + "\n")
	}

	switch s.mode {
	case steerConfirming:
		// Both confirmable verbs destroy work: cancel stops a live run, reject_plan
		// throws away a plan the agent spent tokens on.
		sb.WriteString(m.pal.box.Render(
			"Really " + steerVerbLabel(s.pending) + "?  [y] yes   [n] no"))
	case steerTyping:
		sb.WriteString(m.pal.title.Render("follow-up> ") + cellText(s.input) + "▌")
		sb.WriteString("\n" + m.pal.faint.Render("enter send · esc cancel"))
	default:
		// Idle: the action keys (follow-up, cancel, review, and approve/reject at a plan
		// gate) live in the detail footer now (PRD #325 M4), so this case emits only the
		// queue indicator and notice already written above. The ownership gate still holds
		// — a non-owner returned above with the read-only reason and never reaches here.
	}
	return sb.String()
}

func steerVerbLabel(kind string) string {
	switch kind {
	case kindCancel:
		return "cancel this run"
	case kindRejectPlan:
		return "reject this plan"
	default:
		return kind
	}
}

// ---- keys + commands ------------------------------------------------------

// steerKey handles the steer bar's keys. It returns handled=false when the key is not
// the bar's, so the detail view keeps its own bindings.
func (m tuiModel) steerKey(k string) (tuiModel, tea.Cmd, bool) {
	s := &m.detail.steer

	switch s.mode {
	case steerConfirming:
		switch k {
		case keyConfirmY:
			kind := s.pending
			s.mode, s.pending = steerIdle, ""
			return m, m.submitSteerCmd(kind, ""), true
		default:
			// ANY other key cancels. A destructive verb must require the affirmative
			// key, never merely "not the escape key".
			s.mode, s.pending = steerIdle, ""
			return m, nil, true
		}

	case steerTyping:
		switch k {
		case keyEsc:
			s.mode, s.input = steerIdle, ""
			return m, nil, true
		case keyEnter:
			body := strings.TrimSpace(s.input)
			s.mode, s.input = steerIdle, ""
			if body == "" {
				return m, nil, true
			}
			return m, m.submitSteerCmd(kindFollowUp, body), true
		case "backspace":
			if n := len([]rune(s.input)); n > 0 {
				s.input = string([]rune(s.input)[:n-1])
			}
			return m, nil, true
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				s.input += k
			}
			return m, nil, true
		}
	}

	// Idle. Every verb below is owner-only, so the gate is checked ONCE here rather
	// than per-key: a non-owner's keystrokes are inert, not merely unrendered.
	if s.access != steerAllowed {
		return m, nil, false
	}
	switch k {
	case "f":
		s.mode, s.input = steerTyping, ""
		return m, nil, true
	case "x":
		s.mode, s.pending = steerConfirming, kindCancel
		return m, nil, true
	case keyConfirmY:
		if atPlanGate(m.detail.run) {
			return m, m.submitSteerCmd(kindApprovePlan, ""), true
		}
	case keyConfirmN:
		if atPlanGate(m.detail.run) {
			s.mode, s.pending = steerConfirming, kindRejectPlan
			return m, nil, true
		}
	}
	return m, nil, false
}

type steerResultMsg struct {
	runID string
	kind  string
	res   apitypes.RunInputResponse
	err   error
}

type runInputsMsg struct {
	runID  string
	inputs []apitypes.SteerInputDTO
	err    error
}

func (m tuiModel) submitSteerCmd(kind, body string) tea.Cmd {
	c, ctx, runID := m.client, m.ctx, m.detail.runID
	return func() tea.Msg {
		res, err := c.SubmitRunInput(ctx, runID, kind, body, nil)
		return steerResultMsg{runID: runID, kind: kind, res: res, err: err}
	}
}

// fetchInputsCmd doubles as the ownership probe — see steerAccessFor.
func (m tuiModel) fetchInputsCmd(runID string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		in, err := c.RunInputs(ctx, runID)
		return runInputsMsg{runID: runID, inputs: in, err: err}
	}
}

func (m *tuiModel) applySteerResult(msg steerResultMsg) {
	if msg.err != nil {
		m.detail.steer.notice = "could not " + steerVerbLabel(msg.kind) + ": " + fmtErr(msg.err)
		return
	}
	// inputOutcome is the SAME phrasing `uzi run approve|cancel|follow-up` print, so a
	// user moving between the TUI and the plain commands reads one vocabulary.
	m.detail.steer.notice = inputOutcome(msg.kind, msg.res.ServerSide)
}
