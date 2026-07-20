package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"
)

// Action IDs on the approval-gate buttons (PRD #25 M4). Distinct namespace from
// the linker's slack_link_* ids, so the InboundMux can fan every action out to
// both handlers and exactly one acts on it. Exported so tests reference them.
//
// The approve id encodes the agent SOURCE (PRD #37 M7): a CLOSED set the server
// maps to a source constant, so no client-supplied source string ever reaches the
// server (strictly stronger than the web JSON-body path). `slack_gate_approve` is
// the legacy/no-roster id and maps to "own".
const (
	ActionGateApprove        = "slack_gate_approve"      // legacy + no-roster → "own"
	ActionGateApproveRepo    = "slack_gate_approve_repo" // picker → "repo"
	ActionGateApproveOwn     = "slack_gate_approve_own"  // picker → "own"
	ActionGateReject         = "slack_gate_reject"
	ActionGateRejectNoReason = "slack_gate_reject_noreason"
	// ActionGateRequestChanges opens the plan-revision path (PRD #41): it parks the
	// run in revise_pending, and the presser's threaded reply becomes the revise_plan
	// feedback the worker runs a fresh plan turn on. Sibling of Reject, but the run
	// stays awaiting_approval and re-gates with the next plan version.
	ActionGateRequestChanges = "slack_gate_request_changes"
	// ActionGateOpen is the Open-in-uzi URL button. It carries a url (Slack opens
	// it) and is a no-op for the inbound handler.
	ActionGateOpen = "slack_gate_open"
)

// ErrSelectionRejected is the gatekeeper-facing translation of workersvc's
// ErrInvalidSelection (PRD #37 M7): the agent source the button named is no longer
// valid for the run — the roster changed under it (a requeue + re-detect emptied a
// roster the button was still labelled "(8)"). It is surfaced as an ephemeral with
// the GATE LEFT OPEN so the presser can retry, never as a hard failure. The
// adapter in main translates it, keeping slacksvc free of a workersvc import.
var ErrSelectionRejected = errors.New("slack: agent selection rejected by the server")

// ErrReviseCapReached is the replier-facing translation of workersvc's
// ErrReviseCapReached (PRD #41): the run already hit PLAN_MAX_REVISIONS persisted
// revisions, so a further revise_plan is refused server-side. Surfaced to the user
// as an ephemeral (the gate stays parked, no ack) rather than a silent drop. The
// adapter in main translates it, keeping slacksvc free of a workersvc import.
var ErrReviseCapReached = errors.New("slack: plan revision limit reached")

// maxGateAgentNames bounds how many repo agent names the gate lists (Slack's
// section text caps at 3000 chars; the roster caps at 16). Extra names collapse to
// "+N more" so the message stays scannable.
const maxGateAgentNames = 10

// gateState values recorded on slack_run_messages.gate_state.
const (
	gateStateOpen          = "open"
	gateStateRejectPending = "reject_pending"
	// gateStateRevisePending parks the gate after Request-changes is pressed: the run
	// stays awaiting_approval and the presser's threaded reply becomes the revise_plan
	// feedback (PRD #41 Decision 10). The replier's compare-and-swap clears it so
	// exactly one concurrent reply is accepted as the revision.
	gateStateRevisePending = "revise_pending"
)

// maxSlackSectionRunes bounds a Block Kit section's text to Slack's 3000-char limit
// (with headroom for the truncation ellipsis and any partial-entity slack). Applied
// to the plan-in-thread render (PRD #41 Decision 10); slicing is on a rune boundary.
const maxSlackSectionRunes = 2900

// pgText wraps a non-empty string as a valid pgtype.Text (an empty string still
// yields Valid=true; callers pass "" only where they mean a present empty value).
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// gateBlocks builds the awaiting_approval gate message: a short prompt with NO
// plan excerpt (content minimization — the plan stays behind the deep link) plus
// the approve/reject buttons and an Open-in-uzi link button. Each interactive
// button carries the run id in its value so the inbound handler knows which run;
// the ACTOR is always the Slack-authenticated envelope user and the submit is
// ownership-checked, so a forged value can only ever act on a run the presser owns.
//
// Agent picker (PRD #37 M7): when the run detected a repo roster, the gate offers
// TWO approve buttons — "Approve · repo agents (N)" (source repo) and "Approve · my
// templates" (source own) — and lists the repo agent NAMES in the body. Each
// button's native confirm dialog states which roster it uses; the confirm IS the
// opt-in record. When no roster is detected (NULL or [] — rendered identically),
// the single legacy Approve button (source own) is shown, byte-identical to today.
// Descriptions are NEVER rendered (repo-authored free text); names are
// IsValidName-validated kebab-case and additionally mrkdwn-escaped as defense in
// depth.
func gateBlocks(runID uuid.UUID, base string, repoAgentNames []string) []slack.Block {
	var section *slack.SectionBlock
	var approveElems []slack.BlockElement

	if len(repoAgentNames) > 0 {
		section = slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				"*Plan ready for review.* This repo defines its own agents in `.claude/agents/`. "+
					"Approve with the repo's agents, or with your own uzi templates.\n"+
					gateAgentNamesLine(repoAgentNames),
				false, false),
			nil, nil)

		repoBtn := slack.NewButtonBlockElement(ActionGateApproveRepo, runID.String(),
			slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Approve · repo agents (%d)", len(repoAgentNames)), false, false))
		repoBtn.Style = slack.StylePrimary
		repoBtn.Confirm = approveConfirm("repo", len(repoAgentNames))

		ownBtn := slack.NewButtonBlockElement(ActionGateApproveOwn, runID.String(),
			slack.NewTextBlockObject(slack.PlainTextType, "Approve · my templates", false, false))
		ownBtn.Confirm = approveConfirm("own", 0)

		approveElems = []slack.BlockElement{repoBtn, ownBtn}
	} else {
		section = slack.NewSectionBlock(
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
		approveElems = []slack.BlockElement{approve}
	}

	// Request changes (PRD #41): the default-styled sibling of Reject — parks the run
	// for a revision instead of ending it. No confirm dialog: the threaded reply IS
	// the deliberate step, and the revision is capped server-side.
	requestChanges := slack.NewButtonBlockElement(ActionGateRequestChanges, runID.String(),
		slack.NewTextBlockObject(slack.PlainTextType, "Request changes", false, false))

	reject := slack.NewButtonBlockElement(ActionGateReject, runID.String(),
		slack.NewTextBlockObject(slack.PlainTextType, "Reject", false, false))
	reject.Style = slack.StyleDanger

	elements := append(approveElems, requestChanges, reject)
	if link := runURL(base, runID); link != "" {
		open := slack.NewButtonBlockElement(ActionGateOpen, runID.String(),
			slack.NewTextBlockObject(slack.PlainTextType, "Open in uzi", false, false))
		open.URL = link
		elements = append(elements, open)
	}
	return []slack.Block{section, slack.NewActionBlock("slack_gate", elements...)}
}

// approveConfirm is the native confirm dialog for a source-scoped approve (PRD #37
// M7): the opt-in record for choosing the repo's agents (vs the user's templates).
// A repo confirm names the count; an own confirm needs none.
func approveConfirm(source string, n int) *slack.ConfirmationBlockObject {
	title, body := "Use your agent templates?", "The run will implement the plan using your uzi agent templates."
	if source == "repo" {
		title = "Use the repo's agents?"
		body = fmt.Sprintf(
			"The run will implement the plan using the %d agent(s) the repository defines in .claude/agents/ — not your uzi templates. "+
				"They are authored by the repo, so their review is not uzi's own.", n)
	}
	return slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, title, false, false),
		slack.NewTextBlockObject(slack.PlainTextType, body, false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Approve", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
	)
}

// gateAgentNamesLine renders the repo agent names as a mrkdwn line, capped at
// maxGateAgentNames with a "+N more" tail. Each name is IsValidName-validated
// kebab-case (so it cannot carry mrkdwn), and mrkdwn-escaped anyway as defense in
// depth. Descriptions are never included.
func gateAgentNamesLine(names []string) string {
	shown, extra := names, 0
	if len(shown) > maxGateAgentNames {
		extra = len(shown) - maxGateAgentNames
		shown = shown[:maxGateAgentNames]
	}
	quoted := make([]string, len(shown))
	for i, n := range shown {
		quoted[i] = "`" + EscapeMrkdwn(n) + "`"
	}
	line := "Repo agents: " + strings.Join(quoted, ", ")
	if extra > 0 {
		line += fmt.Sprintf(" +%d more", extra)
	}
	return line
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

// revisePendingBlocks replaces the gate buttons after Request-changes is pressed
// (PRD #41 Decision 10): a prompt to reply in-thread with what should change. Mirrors
// rejectPendingBlocks, but there is no escape-hatch button — a revision needs the
// feedback text, so the only affordance is the threaded reply. The prompt is fixed
// text; posting it is best-effort (the server-side supersede + CAS are the real
// guarantees, not this edit).
func revisePendingBlocks(_ uuid.UUID) []slack.Block {
	return []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType,
			"Reply in this thread with what should change — the plan will be revised and re-posted for approval.",
			false, false),
		nil, nil)}
}

// truncateForSlackSection bounds text to Slack's 3000-char section-block limit on a
// RUNE boundary (PRD #41 Decision 10) — a multi-byte rune is never split. Applied to
// the already-escaped plan blob before it becomes a section; the deep link is a
// SEPARATE block outside this bound, so it can never be truncated away.
func truncateForSlackSection(s string) string {
	r := []rune(s)
	if len(r) <= maxSlackSectionRunes {
		return s
	}
	return string(r[:maxSlackSectionRunes]) + "\n…"
}

// planThreadBlocks renders the plan into the run's DM thread at the approval gate
// (PRD #41 Decision 10 — Slack gate parity). Pipeline: ScrubSecrets (credential
// defense in depth) → EscapeMrkdwn of the WHOLE blob → truncate → deep link appended
// as a SEPARATE trusted block.
//
// The whole-blob EscapeMrkdwn is the DOCUMENTED EXCEPTION to EscapeMrkdwn's per-field
// rule (see redact.go): that rule exists so escaping never breaks trusted <url|label>
// markup interpolated beside an untrusted field — but the plan blob carries NO trusted
// markup of its own, so escaping it wholesale is exactly right and neutralizes any
// <, >, &, <@Uxxx> mention, or spoofed <https://evil|Open> link a hostile plan embeds.
// The one trusted element, the "full plan in uzi" deep link, is added in its own block
// OUTSIDE the truncated region so it is never split or displaced by an over-long plan.
func planThreadBlocks(runID uuid.UUID, planMD, base string) []slack.Block {
	body := truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(planMD)))
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, body, false, false), nil, nil),
	}
	if u := runURL(base, runID); u != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_gate_plan_link",
			slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("<%s|Open the full plan in uzi>", u), false, false)))
	}
	return blocks
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
	case ActionGateApprove, ActionGateApproveRepo, ActionGateApproveOwn,
		ActionGateReject, ActionGateRejectNoReason, ActionGateRequestChanges:
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
