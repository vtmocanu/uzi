package slacksvc

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"
)

// Action IDs on the approval-gate buttons (PRD #25 M4). Distinct namespace from
// the linker's slack_link_* ids, so the InboundMux can fan every action out to
// both handlers and exactly one acts on it. Exported so tests reference them.
const (
	ActionGateApprove        = "slack_gate_approve"
	ActionGateReject         = "slack_gate_reject"
	ActionGateRejectNoReason = "slack_gate_reject_noreason"
	// ActionGateOpen is the Open-in-uzi URL button. It carries a url (Slack opens
	// it) and is a no-op for the inbound handler.
	ActionGateOpen = "slack_gate_open"
)

// gateState values recorded on slack_run_messages.gate_state.
const (
	gateStateOpen          = "open"
	gateStateRejectPending = "reject_pending"
)

// pgText wraps a non-empty string as a valid pgtype.Text (an empty string still
// yields Valid=true; callers pass "" only where they mean a present empty value).
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// gateBlocks builds the awaiting_approval gate message: a short prompt with NO
// plan excerpt (content minimization — the plan stays behind the deep link) plus
// Approve (primary, native confirm dialog), Reject, and an Open-in-uzi link
// button. Each interactive button carries the run id in its value so the inbound
// handler knows which run; the ACTOR is always the Slack-authenticated envelope
// user and SubmitInput is ownership-checked, so a forged value can only ever act
// on a run the presser already owns. The prompt is a fixed string (no dynamic
// fields), so there is nothing to escape here.
func gateBlocks(runID uuid.UUID, base string) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType,
			"*Plan ready for review.* Approve to let the run continue, or reject to send it back. The plan is in uzi.",
			false, false),
		nil, nil)

	approve := slack.NewButtonBlockElement(ActionGateApprove, runID.String(),
		slack.NewTextBlockObject(slack.PlainTextType, "Approve", false, false))
	approve.Style = slack.StylePrimary
	approve.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, "Approve this plan?", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "The run will continue and implement the plan.", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Approve", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
	)

	reject := slack.NewButtonBlockElement(ActionGateReject, runID.String(),
		slack.NewTextBlockObject(slack.PlainTextType, "Reject", false, false))
	reject.Style = slack.StyleDanger

	elements := []slack.BlockElement{approve, reject}
	if link := runURL(base, runID); link != "" {
		open := slack.NewButtonBlockElement(ActionGateOpen, runID.String(),
			slack.NewTextBlockObject(slack.PlainTextType, "Open in uzi", false, false))
		open.URL = link
		elements = append(elements, open)
	}
	return []slack.Block{section, slack.NewActionBlock("slack_gate", elements...)}
}

// rejectPendingBlocks replaces the gate buttons after Reject is pressed: a prompt
// to reply in-thread with a reason (the threaded reply is wired in M5) plus a
// "Reject without reason" escape hatch that submits the rejection immediately.
// The prompt is fixed text; the button carries the run id.
func rejectPendingBlocks(runID uuid.UUID) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType,
			"Reply in this thread with the rejection reason — or:", false, false),
		nil, nil)
	btn := slack.NewButtonBlockElement(ActionGateRejectNoReason, runID.String(),
		slack.NewTextBlockObject(slack.PlainTextType, "Reject without reason", false, false))
	btn.Style = slack.StyleDanger
	return []slack.Block{section, slack.NewActionBlock("slack_gate_reject_pending", btn)}
}

// gateResolvedBlocks renders a resolved gate as a single section with no action
// block, so editing the gate message with it removes the Approve/Reject buttons.
// text is a fixed caller-built template; it is mrkdwn-escaped as defense in depth.
func gateResolvedBlocks(text string) []slack.Block {
	return []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, EscapeMrkdwn(text), false, false), nil, nil)}
}

// isGateAction reports whether an action id belongs to the gate handler. The
// Open-in-uzi url button is deliberately excluded — it navigates, it is not a
// server action.
func isGateAction(actionID string) bool {
	switch actionID {
	case ActionGateApprove, ActionGateReject, ActionGateRejectNoReason:
		return true
	default:
		return false
	}
}

// InboundMux fans one inbound Block Kit action out to several handlers. The
// action-id namespaces are disjoint (the linker owns slack_link_*, the gatekeeper
// owns slack_gate_*), so each action is acted on by exactly one handler while the
// others hit their default-ignore branch. Order is irrelevant.
type InboundMux []InboundHandler

// HandleBlockAction implements InboundHandler.
func (m InboundMux) HandleBlockAction(ctx context.Context, a BlockAction) {
	for _, h := range m {
		if h != nil {
			h.HandleBlockAction(ctx, a)
		}
	}
}

var _ InboundHandler = InboundMux(nil)
