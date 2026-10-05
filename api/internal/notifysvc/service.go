// Package notifysvc is the single write seam for user notifications (PRD #46
// Decision 6). Every feature that wants to notify a user calls Notify rather than
// touching the notifications table or Slack directly, so one place owns the
// ordering: the row is PERSISTED FIRST, then Slack is attempted best-effort, so a
// Slack outage, an unlinked user, or a full notifier queue never loses the row.
//
// Since PRD #1650 retired the in-app inbox, nothing reads the table back to a user:
// it is a pruned, write-only event log (capped per user by DefaultUserCap, so not a
// durable audit log). The user-facing delivery is the Slack DM.
//
// One exception (issue #1675): a notification that opts into Notification.DurableSlack
// (only the CI-autofix and MR-rework halt kinds) stores its Slack render on the row and
// is read back by the Redeliverer sweep until the DM is posted or MaxSlackAttempts is
// spent, so a dropped in-memory enqueue is retried rather than lost.
//
// The service is generic: it knows nothing about judges. The caller supplies the
// kind, a jsonb payload (the event data), optional run/review anchors, and an
// optional Slack rendering.
package notifysvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// DefaultUserCap is the per-user retention cap: the newest this-many notifications
// are kept, older ones pruned on write (Decision 6 — pruning ships with the table,
// not later), so the table can't grow without bound. Overridable via New for tests.
const DefaultUserCap = 200

// MaxSlackAttempts bounds the delivery attempts of a durable Slack notification
// (issue #1675): the initial in-memory enqueue counts as attempt 1, each redelivery
// claim adds one. At SlackRetryAfter spacing that is about 24h of retries; past it the
// row is given up on (and becomes prunable).
const MaxSlackAttempts = 288

// SlackRetryAfter is how long after a durable row's last Slack attempt it becomes
// claimable again by the Redeliverer. It must comfortably exceed the time a healthy
// notifier takes to drain its queue and post, so a redelivery does not race the
// original attempt.
const SlackRetryAfter = 5 * time.Minute

// Store is the slice of generated queries the service needs. *store.Queries
// satisfies it; tests inject a fake to assert the persist-first ordering and the
// prune call.
type Store interface {
	InsertNotification(ctx context.Context, arg store.InsertNotificationParams) (store.Notification, error)
	PruneNotificationsForUser(ctx context.Context, arg store.PruneNotificationsForUserParams) (int64, error)
	// GetSecretEnablement backs the credential re-check a credential-specific alert
	// runs before delivery (PRD #1732 D13, see NotifyEarlyReset).
	GetSecretEnablement(ctx context.Context, arg store.GetSecretEnablementParams) (store.GetSecretEnablementRow, error)
}

// Slacker is the best-effort Slack delivery seam. The slacksvc Notifier satisfies
// it via PublishNotification, which enqueues onto the notifier's own goroutine and
// returns immediately — so a Slack call never blocks or fails Notify. Optional:
// nil (Slack off, or a test) simply skips delivery. The enqueue itself is in-memory and
// lossy (a full queue drops the DM); a render with a non-zero DeliveryID is additionally
// stored on its row and redelivered by the Redeliverer until the notifier marks it
// delivered (issue #1675).
//
// The method takes the SlackRender struct by value (PRD #268 M3), so slacksvc imports
// notifysvc for the param type. That import is LEAF-WARD and one-directional: notifysvc
// depends on store only and never imports slacksvc, so there is no cycle (go build is
// the check). The struct replaces the earlier primitives-only signature because the
// Block Kit renderer needs the emoji + structured facts, not just title/body/link.
type Slacker interface {
	PublishNotification(userID uuid.UUID, r SlackRender)
}

// Service is the notify seam. slack and its render inputs are optional; only q is
// required.
type Service struct {
	q      Store
	slack  Slacker
	cap    int32
	logger *slog.Logger
}

// New builds a Service. slack may be nil (the row is then recorded, no DM). A
// non-positive cap falls back to DefaultUserCap.
func New(q Store, slack Slacker, cap int, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	c := int32(cap) //nolint:gosec // G115: cap is a small per-user retention cap (DefaultUserCap 200), never near int32 range
	if c <= 0 {
		c = DefaultUserCap
	}
	return &Service{q: q, slack: slack, cap: c, logger: logger}
}

// SlackRender is the optional Slack DM rendering for a notification. Title is a
// caller-set fixed label (e.g. "judge review ready"); Body is the dynamic,
// potentially untrusted free text (a verdict summary, a repo/agent name) and Link
// is an in-app deep-link URL. Emoji is a caller-set leading glyph for the section
// head (empty ⇒ none). Facts are caller-built TRUSTED short strings that may carry
// intentional mrkdwn markup (`*bold*`, “ `code` “ chips, verdict emoji) built from
// CLOSED enums/ints — the notifier scrubs them but does NOT mrkdwn-escape them (that
// would break the intended markup). The notifier escapes + scrubs the untrusted
// fields before they leave the box; the row is recorded regardless.
//
// LinkLabel is an optional caller-set FIXED label for the deep link (e.g. "Open the
// pipeline"); empty renders the default "Open in uzi", so a render that leaves it unset
// is byte-identical to one built before the field existed.
//
// Credential, when set, fences a credential-specific alert (PRD #1732 D13): the notifier
// re-reads the credential immediately before the Slack post and drops the DM unless it
// is still current (see CredentialFence). nil for every notification not about one
// credential.
//
// DeliveryID is the notifications row id of a DURABLE delivery (issue #1675); the
// notifier stamps that row delivered once the DM is posted (or the owner has no Slack
// link). Zero means not durable. Notify and the Redeliverer set it; callers never do
// (Notify overwrites it).
type SlackRender struct {
	Title      string
	Body       string
	Link       string
	LinkLabel  string
	Emoji      string
	Facts      []string
	Credential *CredentialFence
	DeliveryID uuid.UUID
}

// durableRender is the persisted form of a SlackRender (notifications.slack_render). It is
// a dedicated struct so the stored shape is explicit: the credential fence and the
// delivery id are never stored (a fenced render is not durable; the id is the row's own).
type durableRender struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Link      string   `json:"link"`
	LinkLabel string   `json:"link_label"`
	Emoji     string   `json:"emoji"`
	Facts     []string `json:"facts"`
}

func (d durableRender) render(id uuid.UUID) SlackRender {
	return SlackRender{Title: d.Title, Body: d.Body, Link: d.Link, LinkLabel: d.LinkLabel,
		Emoji: d.Emoji, Facts: d.Facts, DeliveryID: id}
}

// CredentialFence names the one credential an alert is about and the enablement
// revision it was produced at (PRD #1732 D13). It travels with the queued Slack DM so
// the delivery goroutine can re-check it at dispatch, not only at enqueue: a DM queued
// before a disable (or a disable and re-enable) is dropped rather than posted.
type CredentialFence struct {
	SecretID      uuid.UUID
	Kind          string
	EnablementRev int64
}

// Current reports whether row, the owner-scoped GetSecretEnablement read of the fenced
// credential, is still that kind, enabled, and at the fenced revision.
func (f CredentialFence) Current(row store.GetSecretEnablementRow) bool {
	return row.Kind == f.Kind && !row.DisabledAt.Valid && row.EnablementRev == f.EnablementRev
}

// CIAutofixPayload is the jsonb carried by the poller's halt notifications:
// ci_autofix_halted and mr_rework_halted (the only producers left once the status-only
// ci_autofix_started / ci_autofix_landed kinds were retired, PRD #1650 D2). Older rows
// of the retired kinds carry the same shape. IssueIID and Reason are omitempty (an
// issueless branch has no issue iid); mr_rework_halted leaves PipelineWebURL empty.
type CIAutofixPayload struct {
	Ref            string `json:"ref"`
	PipelineWebURL string `json:"pipeline_web_url"`
	IssueIID       int64  `json:"issue_iid,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// Notification is the input to Notify: a user to notify, a kind + jsonb payload
// for the event log, optional run/review anchors (both ON DELETE CASCADE at the
// table), and an optional Slack rendering. Payload is marshaled to jsonb; a nil
// Payload persists as '{}'.
type Notification struct {
	UserID   uuid.UUID
	Kind     string
	Payload  any
	RunID    *uuid.UUID
	ReviewID *uuid.UUID
	Slack    *SlackRender
	// DurableSlack opts into at-least-once Slack delivery (issue #1675): the render is
	// stored on the row and redelivered by the Redeliverer until posted. Only the two
	// halt kinds use it. Ignored for a credential-fenced render (a stale fenced DM must
	// never be replayed) and when the Service has no Slacker.
	DurableSlack bool
}

// Notify persists the notification row, then prunes the user's rows to the cap
// (best-effort), then enqueues the Slack DM (best-effort; a DurableSlack notification
// is additionally redelivered by the Redeliverer until posted). The persisted row is
// returned. Only a failure to persist is fatal to the call; prune/Slack failures
// are logged and swallowed. The prune and
// Slack steps run after the durable write so neither can cost the caller the row.
func (s *Service) Notify(ctx context.Context, n Notification) (store.Notification, error) {
	payload := []byte("{}")
	if n.Payload != nil {
		b, err := json.Marshal(n.Payload)
		if err != nil {
			return store.Notification{}, err
		}
		payload = b
	}

	durable := n.DurableSlack && n.Slack != nil && n.Slack.Credential == nil && s.slack != nil
	var render []byte
	if durable {
		b, err := json.Marshal(durableRender{
			Title: n.Slack.Title, Body: n.Slack.Body, Link: n.Slack.Link,
			LinkLabel: n.Slack.LinkLabel, Emoji: n.Slack.Emoji, Facts: n.Slack.Facts,
		})
		if err != nil {
			return store.Notification{}, err
		}
		render = b
	}

	row, err := s.q.InsertNotification(ctx, store.InsertNotificationParams{
		UserID:      n.UserID,
		Kind:        n.Kind,
		Payload:     payload,
		RunID:       pgconv.UUIDPtr(n.RunID),
		ReviewID:    pgconv.UUIDPtr(n.ReviewID),
		SlackRender: render,
	})
	if err != nil {
		return store.Notification{}, err
	}

	// Retention prune, best-effort and off the durable write. The query no-ops when
	// the user is under the cap (a bounded index probe), so calling it every write
	// keeps the cap tight without a scan.
	if _, err := s.q.PruneNotificationsForUser(ctx, store.PruneNotificationsForUserParams{
		UserID:      n.UserID,
		Keep:        s.cap,
		MaxAttempts: MaxSlackAttempts,
	}); err != nil {
		s.logger.Warn("notify: prune failed (best-effort)", "user", n.UserID.String(), "error", err)
	}

	// Slack delivery, best-effort. Enqueues and returns; a Slack failure is handled
	// entirely inside the notifier and never surfaces here.
	if s.slack != nil && n.Slack != nil {
		r := *n.Slack
		r.DeliveryID = uuid.Nil
		if durable {
			r.DeliveryID = row.ID
		}
		s.slack.PublishNotification(n.UserID, r)
	}

	return row, nil
}

// KindEarlyLimitReset is the notifications.kind for a LOUD alert that the Anthropic
// 7-day rate limit reset EARLIER than its expected window (PRD #1020 M3). kind is a
// generic text column with no CHECK, so this needs no migration.
const KindEarlyLimitReset = "early_limit_reset"

// ErrCredentialNotCurrent is NotifyEarlyReset's refusal when the credential the alert is
// about is no longer the owner's enabled token at the revision the reading was written at
// (PRD #1732 D13). Nothing was recorded or sent.
var ErrCredentialNotCurrent = errors.New("credential no longer enabled at the alert's revision")

// EarlyResetPayload is the jsonb recorded for an early_limit_reset notification
// (PRD #1020 M3). title is the fixed headline; expected/observed
// are RFC3339 timestamps for the reset window we projected vs. the one we saw; hours_early is
// the whole-hour display figure. Every field is server-derived from trusted time.Time values —
// no untrusted text rides in here.
type EarlyResetPayload struct {
	Title      string `json:"title"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	HoursEarly int    `json:"hours_early"`
}

// NotifyEarlyReset fires a LOUD Slack DM (and records the row) when the Anthropic
// 7-day rate limit reset EARLIER than expected (PRD #1020 M3). It builds a distinctive
// SlackRender — the "loud" alert is produced entirely here, since slacksvc flattens every
// notification through one generic render with no per-kind dispatch — and delivers it via
// the existing persist-first Notify seam (persist row, prune, best-effort Slack).
//
// The two Facts carrying timestamps use Slack's <!date^unix^{time}|utc-fallback> markup so
// they render in the READER's timezone with a UTC fallback; Facts pass through ScrubSecrets
// but NOT mrkdwn-escaping in slacksvc, so the markup survives. Both times are built from
// trusted numeric time.Time values, so no field can carry a <@…> mention.
//
// Wording is "observed": the reset is seen on the next poll tick, so this understates true
// earliness by up to one poll interval — it does not claim an exact reset instant.
//
// The alert is about ONE credential, so it is re-checked before anything is recorded or
// sent (PRD #1732 D13): secretID must still be userID's Anthropic token, enabled, and at
// enablementRev, the revision the poller's fenced write landed at. A credential disabled
// (or disabled and re-enabled) since that write, deleted, or not the owner's delivers
// nothing and returns ErrCredentialNotCurrent. The same fence rides the queued Slack DM
// (SlackRender.Credential), and the notifier re-checks it just before posting, so a
// disable landing while the DM waits in the queue drops it too. A DM already handed to
// Slack before a later disable cannot be recalled and is out of scope.
func (s *Service) NotifyEarlyReset(ctx context.Context, userID, secretID uuid.UUID, enablementRev int64, expected, observed time.Time) (store.Notification, error) {
	fence := CredentialFence{SecretID: secretID, Kind: store.KindAnthropicToken, EnablementRev: enablementRev}
	cur, err := s.q.GetSecretEnablement(ctx, store.GetSecretEnablementParams{ID: secretID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Notification{}, ErrCredentialNotCurrent
	}
	if err != nil {
		return store.Notification{}, fmt.Errorf("re-check credential: %w", err)
	}
	if !fence.Current(cur) {
		return store.Notification{}, ErrCredentialNotCurrent
	}
	hoursEarly := expected.Sub(observed)
	hoursEarlyInt := int(hoursEarly.Round(time.Hour).Hours())

	return s.Notify(ctx, Notification{
		UserID: userID,
		Kind:   KindEarlyLimitReset,
		Payload: EarlyResetPayload{
			Title:      "7-day rate limit reset early",
			Expected:   expected.Format(time.RFC3339),
			Observed:   observed.Format(time.RFC3339),
			HoursEarly: hoursEarlyInt,
		},
		Slack: &SlackRender{
			Emoji: "🚨",
			Title: "7-DAY RATE LIMIT RESET EARLY",
			Body:  "Anthropic reopened your weekly window ahead of schedule.",
			Facts: []string{
				fmt.Sprintf("reset ~%dh early", hoursEarlyInt),
				fmt.Sprintf("observed <!date^%d^{time}|%s>", observed.Unix(), observed.UTC().Format("15:04 MST")),
				fmt.Sprintf("expected <!date^%d^{time}|%s>", expected.Unix(), expected.UTC().Format("15:04 MST")),
			},
			Credential: &fence,
		},
	})
}
