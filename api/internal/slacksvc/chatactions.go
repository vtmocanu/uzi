package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Action IDs on the chat cards (PRD #191 M4/M5). A THIRD disjoint namespace beside
// slack_link_* (linker) and slack_gate_* (gatekeeper): the InboundMux fans every
// action to all handlers and exactly one — here, ChatActions — acts on a slack_chat_*
// id. A chat card is NOT the Gatekeeper's job: it satisfies none of the gate's
// preconditions (awaiting_approval, gate_ts match), so it gets its own handler
// (Decision 12).
const (
	ActionChatProposalCreate  = "slack_chat_proposal_create"
	ActionChatProposalDismiss = "slack_chat_proposal_dismiss"
)

// isChatAction reports whether an action id is a chat-card action ChatActions owns.
func isChatAction(actionID string) bool {
	return strings.HasPrefix(actionID, "slack_chat_")
}

// CreatedIssue is the forge issue a confirmed proposal created — the subset the card
// renders. Local to slacksvc so the ChatActionSubmitter seam carries no workersvc type.
type CreatedIssue struct {
	IID    int64
	WebURL string
	Title  string
}

// Chat-action sentinels (PRD #191 M4). The adapter in main translates the workersvc
// proposal sentinels into these, so ChatActions can distinguish already-handled (edit
// the card) from not-yours (ephemeral) from a forge failure (retry), without importing
// workersvc.
var (
	ErrChatProposalGone    = errors.New("slack: proposal not found or not yours")
	ErrChatProposalHandled = errors.New("slack: proposal already resolved")
	ErrChatProposalForge   = errors.New("slack: forge rejected the issue")
)

// ChatActionStore is the store slice ChatActions reads: the confirmed-user resolve
// that turns the Slack-authenticated presser into a uzi user. *store.Queries satisfies it.
type ChatActionStore interface {
	GetConfirmedUserBySlackID(ctx context.Context, slackResolvedID pgtype.Text) (store.User, error)
}

// ChatActionSubmitter is the run service slice the chat cards drive (PRD #191 M4): the
// lifted composite proposal ops, ownership-scoped by userID. The adapter in main
// satisfies it over *workersvc.Service, keeping slacksvc free of a workersvc import.
type ChatActionSubmitter interface {
	// ConfirmProposalForUser files the proposed issue on the forge via the user's own
	// connection (claim-first, so a double-click files exactly one). Translated errors:
	// ErrChatProposalHandled, ErrChatProposalGone, ErrChatProposalForge.
	ConfirmProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) (CreatedIssue, error)
	// DismissProposalForUser marks a pending proposal dismissed (never touches the forge).
	DismissProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) error
}

// ChatActions handles the chat cards' Block Kit buttons (PRD #191 M4): Create files
// the proposed issue, Dismiss drops it. Like the Gatekeeper it is an InboundHandler in
// the InboundMux and best-effort on the Slack side, but the forge write rides
// workersvc's ownership-checked, claim-first ConfirmProposalForUser — a forged value
// can only ever act on a proposal the confirmed presser owns, and a double-click files
// one issue.
type ChatActions struct {
	store  ChatActionStore
	svc    ChatActionSubmitter
	poster Poster
	logger *slog.Logger
}

// NewChatActions builds a ChatActions. poster is the shared bot-token Slack surface.
func NewChatActions(s ChatActionStore, svc ChatActionSubmitter, poster Poster, logger *slog.Logger) *ChatActions {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChatActions{store: s, svc: svc, poster: poster, logger: logger}
}

// HandleBlockAction routes a chat-card press. The actor is the Slack-authenticated
// envelope user ONLY; the value blob carries (run, proposal) and is authorized through
// the ownership-scoped service, so a forged value acts on nothing the presser doesn't
// own. Non-chat actions (the linker's, the gate's) are ignored.
func (c *ChatActions) HandleBlockAction(ctx context.Context, a BlockAction) {
	if !isChatAction(a.ActionID) {
		return
	}
	slackID := strings.TrimSpace(a.SlackUserID)
	if slackID == "" {
		return
	}
	// Only the chat-card actions ChatActions owns proceed; an unknown slack_chat_* id
	// returns before the confirmed-user DB lookup.
	if a.ActionID != ActionChatProposalCreate && a.ActionID != ActionChatProposalDismiss {
		return
	}

	user, err := c.store.GetConfirmedUserBySlackID(ctx, pgText(slackID))
	if errors.Is(err, pgx.ErrNoRows) {
		c.ephemeral(ctx, a, "This Slack account isn't linked to uzi — open uzi → Settings → Notifications to link it.")
		return
	}
	if err != nil {
		c.logf("resolve confirmed user", err)
		return
	}

	runID, propID, ok := decodeChatValue(a.Value)
	if !ok {
		c.logf("parse chat action value", errors.New("malformed value"))
		return
	}

	switch a.ActionID {
	case ActionChatProposalCreate:
		c.createProposal(ctx, a, user.ID, runID, propID)
	case ActionChatProposalDismiss:
		c.dismissProposal(ctx, a, user.ID, runID, propID)
	}
}

// createProposal files the proposed issue and edits the card to its outcome. A
// double-click reaches the forge once (claim-first): the winner shows the created
// issue, the loser gets the "already handled" edit. A non-owner's press files nothing
// (ErrChatProposalGone) and gets an ephemeral. A forge failure reverts the proposal to
// pending (in the service) so the presser can retry.
func (c *ChatActions) createProposal(ctx context.Context, a BlockAction, userID, runID, propID uuid.UUID) {
	issue, err := c.svc.ConfirmProposalForUser(ctx, userID, runID, propID)
	switch {
	case err == nil:
		c.editCard(ctx, a, fmt.Sprintf("✅ Filed issue #%d: %s", issue.IID, issue.Title), issue.WebURL)
	case errors.Is(err, ErrChatProposalHandled):
		c.editCard(ctx, a, "This proposal was already handled.", "")
	case errors.Is(err, ErrChatProposalGone):
		c.ephemeral(ctx, a, "That proposal isn't yours, or it no longer exists.")
	case errors.Is(err, ErrChatProposalForge):
		c.ephemeral(ctx, a, "Couldn't file the issue on the forge — the proposal is back to pending, so press Create again or open it in uzi.")
	default:
		c.logf("confirm proposal", err)
		c.ephemeral(ctx, a, "Couldn't file the issue right now — try again, or open it in uzi.")
	}
}

// dismissProposal drops a pending proposal and edits the card. Never touches the forge.
func (c *ChatActions) dismissProposal(ctx context.Context, a BlockAction, userID, runID, propID uuid.UUID) {
	err := c.svc.DismissProposalForUser(ctx, userID, runID, propID)
	switch {
	case err == nil:
		c.editCard(ctx, a, "🗑️ Proposal dismissed.", "")
	case errors.Is(err, ErrChatProposalHandled):
		c.editCard(ctx, a, "This proposal was already handled.", "")
	case errors.Is(err, ErrChatProposalGone):
		c.ephemeral(ctx, a, "That proposal isn't yours, or it no longer exists.")
	default:
		c.logf("dismiss proposal", err)
		c.ephemeral(ctx, a, "Couldn't dismiss that proposal — try again.")
	}
}

// editCard replaces the card's blocks with a button-free resolved state, so the
// buttons are gone (a second press hits nothing) and the outcome is visible.
func (c *ChatActions) editCard(ctx context.Context, a BlockAction, text, url string) {
	if a.ChannelID == "" || a.MessageTS == "" {
		return
	}
	// A FIXED fallback (the notification text) — never the untrusted title, which Slack
	// would process for mentions/links in the fallback field even though the card blocks
	// are inert. The visible outcome is in proposalResolvedBlocks (scrubbed + escaped).
	if err := c.poster.UpdateBlocks(ctx, a.ChannelID, a.MessageTS, "Issue proposal updated", proposalResolvedBlocks(text, url)); err != nil {
		c.logf("edit proposal card", err)
	}
}

func (c *ChatActions) ephemeral(ctx context.Context, a BlockAction, text string) {
	if a.ChannelID == "" || a.SlackUserID == "" {
		return
	}
	if err := c.poster.PostEphemeral(ctx, a.ChannelID, a.SlackUserID, ScrubSecrets(text)); err != nil {
		c.logf("post ephemeral", err)
	}
}

func (c *ChatActions) logf(what string, err error) {
	c.logger.Warn("slack chat actions: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

var _ InboundHandler = (*ChatActions)(nil)

// -------------------------------------------------------------------------
// value encoding + card blocks
// -------------------------------------------------------------------------

// encodeChatValue packs (run, proposal) into a button value. uuids never contain a
// colon, so it is an unambiguous delimiter. The value is untrusted on the way back —
// every field is re-validated against the ownership-scoped service — so this is a
// convenience encoding, not a trust boundary.
func encodeChatValue(runID, propID uuid.UUID) string {
	return runID.String() + ":" + propID.String()
}

// decodeChatValue parses a (run, proposal) button value, reporting false for anything
// malformed.
func decodeChatValue(v string) (runID, propID uuid.UUID, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(v), ":", 2)
	if len(parts) != 2 {
		return uuid.Nil, uuid.Nil, false
	}
	r, err1 := uuid.Parse(parts[0])
	p, err2 := uuid.Parse(parts[1])
	if err1 != nil || err2 != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return r, p, true
}

// proposalCardMaxLabels bounds the label chips shown on a card.
const proposalCardMaxLabels = 12

// proposalCardBlocks builds the issue-proposal card (PRD #191 M4): repo + title +
// description + labels, with Create (confirm-gated) and Dismiss buttons. Every
// model-authored field (title, description) is scrubbed and mrkdwn-escaped so an
// injected `<@mention>`, `<https://evil|link>` or credential is inert; the repo path
// and labels are escaped too.
func proposalCardBlocks(runID, propID uuid.UUID, title, description string, labels []string, repoPath string) []slack.Block {
	var b strings.Builder
	b.WriteString("*📝 Issue proposal*")
	if rp := cardField(repoPath); rp != "" {
		b.WriteString(" for `" + rp + "`")
	}
	if t := cardField(title); t != "" {
		b.WriteString("\n*" + t + "*")
	}
	if d := renderChatBody(description); d != "" {
		b.WriteString("\n" + d)
	}
	if line := proposalLabelsLine(labels); line != "" {
		b.WriteString("\n" + line)
	}
	// Bound the WHOLE assembled section, not just the description: title + repo + labels
	// ride the same 3000-char section text object, and Slack rejects the entire blocks
	// payload if it overflows (the card would then silently never post).
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, truncateForSlackSection(b.String()), false, false), nil, nil)

	val := encodeChatValue(runID, propID)
	create := slack.NewButtonBlockElement(ActionChatProposalCreate, val,
		slack.NewTextBlockObject(slack.PlainTextType, "Create issue", false, false))
	create.Style = slack.StylePrimary
	create.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, "File this issue?", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "This creates a real issue on the forge using your own connection.", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Create", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
	)
	dismiss := slack.NewButtonBlockElement(ActionChatProposalDismiss, val,
		slack.NewTextBlockObject(slack.PlainTextType, "Dismiss", false, false))
	dismiss.Style = slack.StyleDanger

	return []slack.Block{section, slack.NewActionBlock("slack_chat_proposal", create, dismiss)}
}

// cardField renders one model-authored single-line card field (title, repo path,
// label) inert AND secret-scrubbed — the same last-line-of-defense scrub the
// description gets, so a credential-shaped string in a title can't leave the box.
func cardField(s string) string {
	return EscapeMrkdwn(ScrubSecrets(strings.TrimSpace(s)))
}

// proposalLabelsLine renders proposal labels as scrubbed+escaped `code` chips, capped.
func proposalLabelsLine(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	shown, extra := labels, 0
	if len(shown) > proposalCardMaxLabels {
		extra = len(shown) - proposalCardMaxLabels
		shown = shown[:proposalCardMaxLabels]
	}
	chips := make([]string, len(shown))
	for i, l := range shown {
		chips[i] = "`" + cardField(l) + "`"
	}
	line := "Labels: " + strings.Join(chips, " ")
	if extra > 0 {
		line += fmt.Sprintf(" +%d more", extra)
	}
	return line
}

// proposalResolvedBlocks renders a resolved card: one button-free section (so a second
// press finds nothing) plus an optional trusted link to the created issue. text is a
// fixed template with an untrusted title interpolated, so it is escaped wholesale.
func proposalResolvedBlocks(text, url string) []slack.Block {
	// text interpolates the forge-echoed (originally model-authored) issue title, so it
	// is scrubbed as well as escaped — parity with the pending card's fields.
	blocks := []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, EscapeMrkdwn(ScrubSecrets(text)), false, false), nil, nil)}
	if u := strings.TrimSpace(url); u != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_chat_issue_link",
			slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("<%s|View the issue>", u), false, false)))
	}
	return blocks
}
