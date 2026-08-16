package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Action IDs on the link-confirmation DM buttons. Exported so M4 can add its
// approve/reject action kinds alongside without renaming these.
const (
	ActionLinkConfirm = "slack_link_confirm"
	ActionLinkReject  = "slack_link_reject"
)

// autoMatchCooldown bounds how often the email auto-match pass runs. It fires on
// every socket (re)connect — including token rotations and flaps — and
// users.lookupByEmail is Slack Tier-3 rate-limited (~50/min), so a cooldown keeps
// a flapping connection from hammering the lookup API. The compare-then-write
// guard makes a skipped pass harmless (a genuinely new match is picked up on the
// next pass past the cooldown, or a later reconnect).
const autoMatchCooldown = 10 * time.Minute

// DMTargetCooldown bounds how often a DM is sent to the SAME Slack target across
// the two user-triggered DM paths (the override-set Confirm card and the test DM).
// Without it, an authed user could hammer /me/slack/override or /me/slack/test-dm
// to spam an arbitrary member with Confirm cards, or drive the send result as a
// member-id enumeration oracle. The per-user rate limiter on those routes is the
// coarse backstop; this is the finer per-target dedup. Exported so the test-DM
// handler can advertise a matching Retry-After.
const DMTargetCooldown = 30 * time.Second

// ErrDMCooldown is returned by SendTestDM when a DM to the same target was sent
// within DMTargetCooldown. The handler maps it to 429 (not the 502 it uses for a
// real send failure) so a rapid re-test reads as "slow down", not "Slack is down".
var ErrDMCooldown = errors.New("slack: per-target DM cooldown")

// BlockAction is one Block Kit button press, resolved from a Socket Mode
// interactive envelope. SlackUserID is the AUTHENTICATED envelope user
// (callback.User.ID) — never a value read from a payload blob, which a caller
// could forge. The shape is intentionally the same one M4's approve/reject
// handling needs, so M4 only adds ActionID kinds.
type BlockAction struct {
	SlackUserID string // callback.User.ID — the Slack-authenticated actor
	ActionID    string // which button
	Value       string // the button's value, if any (unused for link confirm/reject)
	ChannelID   string // callback.Container.ChannelID — to edit the DM
	MessageTS   string // callback.Container.MessageTs — to edit the DM
}

// InboundHandler routes a Block Kit action from the Socket Mode receive loop. The
// Manager holds it statically (set at construction, not part of the token
// fingerprint), so a hot token restart keeps the same handler. *Linker is the
// production implementation.
type InboundHandler interface {
	HandleBlockAction(ctx context.Context, a BlockAction)
}

// LinkerStore is the slice of generated queries the linker reads/writes.
// *store.Queries satisfies it.
type LinkerStore interface {
	ListUsersForSlackLink(ctx context.Context) ([]store.ListUsersForSlackLinkRow, error)
	SetUserSlackResolvedID(ctx context.Context, arg store.SetUserSlackResolvedIDParams) (store.SetUserSlackResolvedIDRow, error)
	ConfirmUserSlackLink(ctx context.Context, slackResolvedID pgtype.Text) (int64, error)
	ClearUserSlackLink(ctx context.Context, slackResolvedID pgtype.Text) (int64, error)
}

// Linker owns Slack account linking (PRD #25 M3): the email auto-match pass, the
// link-confirmation DM round-trip, and the inbound Confirm / Not-me handling. It
// is the InboundHandler wired into the Manager and is best-effort throughout — a
// Slack failure is logged (redacted) and never affects a run or the socket.
type Linker struct {
	store  LinkerStore
	poster Poster
	logger *slog.Logger

	mu       sync.Mutex
	lastPass time.Time
	// dmSent tracks the last DM time per Slack target for the per-target cooldown.
	// Guarded by mu; pruned on each check so it stays bounded by the request rate.
	dmSent map[string]time.Time
}

// NewLinker builds a Linker. poster is the shared bot-token Slack surface (reads
// the current token per call, so a hot rotation is picked up).
func NewLinker(s LinkerStore, poster Poster, logger *slog.Logger) *Linker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Linker{store: s, poster: poster, logger: logger, dmSent: make(map[string]time.Time)}
}

// allowDM reports whether an outbound DM to slackID is outside its per-target
// cooldown, recording the send when it is. It bounds the two user-triggered DM
// paths (override Confirm card + test DM) so an authed user cannot turn them into
// a per-member spam primitive. Expired entries are pruned on each call so the map
// stays bounded by the request rate. Callers hold no lock; this takes mu itself.
func (l *Linker) allowDM(slackID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for id, at := range l.dmSent {
		if now.Sub(at) >= DMTargetCooldown {
			delete(l.dmSent, id)
		}
	}
	if at, ok := l.dmSent[slackID]; ok && now.Sub(at) < DMTargetCooldown {
		return false
	}
	l.dmSent[slackID] = now
	return true
}

// AutoMatch resolves override-free users by email and (re)sends a link-confirmation
// DM for any NEW match. Wired as the Manager's on-connected hook, so it runs on
// every (re)connect; a cooldown plus a compare-then-write guard keep that cheap
// and, critically, keep it from un-confirming an already-linked user.
//
// Best-effort: any error is logged and the pass moves on (or stops, on a rate
// limit). It never returns an error and never blocks the socket (the Manager runs
// it in its own goroutine bound to the connection context).
func (l *Linker) AutoMatch(ctx context.Context) {
	// Cooldown: skip if a pass ran recently. A flapping socket must not turn every
	// reconnect into a full workspace lookup sweep.
	l.mu.Lock()
	if !l.lastPass.IsZero() && time.Since(l.lastPass) < autoMatchCooldown {
		l.mu.Unlock()
		return
	}
	l.lastPass = time.Now()
	l.mu.Unlock()

	users, err := l.store.ListUsersForSlackLink(ctx)
	if err != nil {
		l.logf("auto-match: list users", err)
		return
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return // connection torn down (hot restart / shutdown)
		}
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		slackID, err := l.poster.LookupUserByEmail(ctx, email)
		if err != nil {
			if isRateLimited(err) {
				// Back off the whole pass rather than keep hammering a Tier-3 limit;
				// the next reconnect past the cooldown retries.
				l.logf("auto-match: rate-limited, stopping pass", err)
				return
			}
			continue // not found / other → this user simply has no match
		}
		slackID = strings.TrimSpace(slackID)
		if slackID == "" {
			continue
		}
		// Compare-then-write: SetUserSlackResolvedID resets slack_link_confirmed_at
		// UNCONDITIONALLY, so calling it for an unchanged id would un-confirm a user
		// on every reconnect. Only write when the looked-up id actually differs.
		if u.SlackResolvedID.Valid && u.SlackResolvedID.String == slackID {
			continue
		}
		if _, err := l.store.SetUserSlackResolvedID(ctx, store.SetUserSlackResolvedIDParams{
			SlackResolvedID: pgtype.Text{String: slackID, Valid: true},
			ID:              u.ID,
		}); err != nil {
			// A unique-index collision (another user already resolves to this id) or
			// any other write error is non-fatal: skip this user.
			l.logf("auto-match: cache resolved id", err)
			continue
		}
		l.SendLinkConfirmation(ctx, slackID, email)
	}
}

// SendLinkConfirmation DMs the target Slack user a Confirm / Not-me card naming
// the uzi account (accountLabel) that wants to link to them. Content flows only
// after they press Confirm; "Not me" clears the link. Best-effort — a send
// failure is logged, not returned (the mapping is already stored; a test-DM or a
// later pass can retry).
func (l *Linker) SendLinkConfirmation(ctx context.Context, slackID, accountLabel string) {
	if !l.allowDM(slackID) {
		// A Confirm card was just sent to this target; suppress the duplicate so
		// override-hammering can't spam one member. The earlier card still stands.
		return
	}
	channel, err := l.poster.OpenDM(ctx, slackID)
	if err != nil {
		l.logf("link confirm: open dm", err)
		return
	}
	fallback := "uzi wants to send you run notifications — Confirm or Not me in the uzi app."
	if _, err := l.poster.PostBlocks(ctx, channel, "", ScrubSecrets(fallback), linkConfirmBlocks(accountLabel)); err != nil {
		l.logf("link confirm: post", err)
	}
}

// SendTestDM sends the user-initiated test message to a resolved Slack id (the
// Notifications settings "Send test DM" button). Unlike the notifier path it
// returns the error so the handler can report a failed send to the user.
func (l *Linker) SendTestDM(ctx context.Context, slackID string) error {
	if !l.allowDM(slackID) {
		return ErrDMCooldown
	}
	channel, err := l.poster.OpenDM(ctx, slackID)
	if err != nil {
		return err
	}
	_, err = l.poster.Post(ctx, channel, "", ScrubSecrets("✅ Test message — your Slack notifications are wired up."))
	return err
}

// HandleBlockAction handles a Confirm / Not-me press. The actor is the
// Slack-authenticated envelope user id; resolution is structural — the unique
// partial index on slack_resolved_id means at most one row matches, so there is
// no ambiguity to resolve and nothing to guess. Unknown action ids (M4's
// approve/reject) are ignored here.
func (l *Linker) HandleBlockAction(ctx context.Context, a BlockAction) {
	slackID := strings.TrimSpace(a.SlackUserID)
	if slackID == "" {
		return // no authenticated actor → nothing to act on
	}
	id := pgtype.Text{String: slackID, Valid: true}
	switch a.ActionID {
	case ActionLinkConfirm:
		n, err := l.store.ConfirmUserSlackLink(ctx, id)
		if err != nil {
			l.logf("confirm link", err)
			return
		}
		if n > 0 {
			l.updateDM(ctx, a, "✅ Linked — run notifications will arrive here.")
		} else {
			// 0 rows: already confirmed, or the link was cleared / never existed.
			l.updateDM(ctx, a, "This link was already handled.")
		}
	case ActionLinkReject:
		n, err := l.store.ClearUserSlackLink(ctx, id)
		if err != nil {
			l.logf("clear link", err)
			return
		}
		if n > 0 {
			l.updateDM(ctx, a, "Removed — uzi won't message you here.")
		} else {
			l.updateDM(ctx, a, "This link was already handled.")
		}
	default:
		// Not a link action (M4 adds the gate actions) — ignore.
	}
}

// updateDM edits the confirmation DM in place to its resolved state. Best-effort:
// the DB write already happened, so a failed edit only leaves the buttons stale.
func (l *Linker) updateDM(ctx context.Context, a BlockAction, text string) {
	if a.ChannelID == "" || a.MessageTS == "" {
		return
	}
	if err := l.poster.Update(ctx, a.ChannelID, a.MessageTS, ScrubSecrets(text)); err != nil {
		l.logf("update link dm", err)
	}
}

func (l *Linker) logf(what string, err error) {
	l.logger.Warn("slack linker: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// linkConfirmBlocks builds the Confirm / Not-me card. It names the uzi account so
// the recipient can tell whether the link is theirs (the anti-squatting decision
// the PRD requires); accountLabel is a uzi-account-controlled string, so it is
// mrkdwn-escaped (no injected <url|label> / <@Uxxx>) as well as secret-scrubbed
// before it goes into the mrkdwn section.
func linkConfirmBlocks(accountLabel string) []slack.Block {
	prompt := fmt.Sprintf("uzi wants to send you run notifications for the uzi account *%s*. Is this you?", EscapeMrkdwn(ScrubSecrets(accountLabel)))
	section := slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, prompt, false, false), nil, nil)
	confirm := slack.NewButtonBlockElement(ActionLinkConfirm, "", slack.NewTextBlockObject(slack.PlainTextType, "Confirm", false, false))
	confirm.Style = slack.StylePrimary
	reject := slack.NewButtonBlockElement(ActionLinkReject, "", slack.NewTextBlockObject(slack.PlainTextType, "Not me", false, false))
	actions := slack.NewActionBlock("slack_link", confirm, reject)
	return []slack.Block{section, actions}
}

// isRateLimited reports whether err is a Slack Tier-limit rejection, so the
// auto-match pass can stop instead of hammering the limit.
func isRateLimited(err error) bool {
	var rle *slack.RateLimitedError
	return errors.As(err, &rle)
}

// ensure *Linker satisfies InboundHandler at compile time.
var _ InboundHandler = (*Linker)(nil)
