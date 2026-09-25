// Package notifysvc is the single write seam for user notifications (PRD #46
// Decision 6). Every feature that wants to notify a user calls Notify rather than
// touching the notifications table or Slack directly, so one place owns the
// ordering: the row is PERSISTED FIRST, then Slack is attempted best-effort, so a
// Slack outage, an unlinked user, or a full notifier queue never loses the row.
//
// Since PRD #1650 retired the in-app inbox, nothing reads the table back to a user:
// it is a pruned, write-only event log (capped per user by DefaultUserCap, so not a
// durable audit log) plus the per-run incidental-finding Slack DM latch
// (NotifyIncidentalFinding). The user-facing delivery is the Slack DM.
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

// Store is the slice of generated queries the service needs. *store.Queries
// satisfies it; tests inject a fake to assert the persist-first ordering and the
// prune call.
type Store interface {
	InsertNotification(ctx context.Context, arg store.InsertNotificationParams) (store.Notification, error)
	PruneNotificationsForUser(ctx context.Context, arg store.PruneNotificationsForUserParams) (int64, error)
	// FindNotificationForRunKind / UpdateNotificationPayload are the PRD #333 D6
	// per-run coalescing pair: find the run's finding notification, whatever its read
	// state (a miss ⇒ this is the run's first finding, insert + one Slack DM), else
	// bump its payload count WITHOUT re-firing Slack. See NotifyIncidentalFinding.
	FindNotificationForRunKind(ctx context.Context, arg store.FindNotificationForRunKindParams) (store.Notification, error)
	UpdateNotificationPayload(ctx context.Context, arg store.UpdateNotificationPayloadParams) (store.Notification, error)
}

// Slacker is the best-effort Slack delivery seam. The slacksvc Notifier satisfies
// it via PublishNotification, which enqueues onto the notifier's own goroutine and
// returns immediately — so a Slack call never blocks or fails Notify. Optional:
// nil (Slack off, or a test) simply skips delivery.
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
type SlackRender struct {
	Title     string
	Body      string
	Link      string
	LinkLabel string
	Emoji     string
	Facts     []string
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
}

// Notify persists the notification row, then prunes the user's rows to the cap
// (best-effort), then enqueues the Slack DM (best-effort). The persisted row is
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

	row, err := s.q.InsertNotification(ctx, store.InsertNotificationParams{
		UserID:   n.UserID,
		Kind:     n.Kind,
		Payload:  payload,
		RunID:    pgconv.UUIDPtr(n.RunID),
		ReviewID: pgconv.UUIDPtr(n.ReviewID),
	})
	if err != nil {
		return store.Notification{}, err
	}

	// Retention prune, best-effort and off the durable write. The query no-ops when
	// the user is under the cap (a bounded index probe), so calling it every write
	// keeps the cap tight without a scan.
	if _, err := s.q.PruneNotificationsForUser(ctx, store.PruneNotificationsForUserParams{
		UserID: n.UserID,
		Keep:   s.cap,
	}); err != nil {
		s.logger.Warn("notify: prune failed (best-effort)", "user", n.UserID.String(), "error", err)
	}

	// Slack delivery, best-effort. Enqueues and returns; a Slack failure is handled
	// entirely inside the notifier and never surfaces here.
	if s.slack != nil && n.Slack != nil {
		s.slack.PublishNotification(n.UserID, *n.Slack)
	}

	return row, nil
}

// KindIncidentalFinding is the notifications.kind for a coalesced incidental-finding
// notification (PRD #333 D6). kind is a generic text column with no CHECK, so this needs
// no migration; the value is the coalescing key alongside (user_id, run_id).
const KindIncidentalFinding = "incidental_finding"

// KindEarlyLimitReset is the notifications.kind for a LOUD alert that the Anthropic
// 7-day rate limit reset EARLIER than its expected window (PRD #1020 M3). kind is a
// generic text column with no CHECK, so this needs no migration.
const KindEarlyLimitReset = "early_limit_reset"

// maxCoalescedFindingIDs caps the finding_ids the coalesced payload accumulates so a
// noisy run cannot grow one row's jsonb without bound. The count keeps climbing past
// the cap; only the id list stops appending. The per-run
// capture cap (workersvc.MaxFindingsPerRun) is far below this, so in practice the cap is
// defense-in-depth, not a limit users meet.
const maxCoalescedFindingIDs = 50

// IncidentalFindingPayload is the jsonb recorded for an incidental_finding
// notification (PRD #333 D6). run_id/repo_id anchor it; repo_path is the human label;
// count is the coalesced headline ("Run flagged M findings") and finding_ids the deep-link
// set. All fields are server-built from the run/repo, never untrusted agent text (the
// finding's title/location live on the backlog behind the deep link, already sanitised).
type IncidentalFindingPayload struct {
	RunID      uuid.UUID   `json:"run_id"`
	RepoID     uuid.UUID   `json:"repo_id"`
	RepoPath   string      `json:"repo_path"`
	Count      int         `json:"count"`
	FindingIDs []uuid.UUID `json:"finding_ids"`
}

// IncidentalFindingNotifyInput carries everything NotifyIncidentalFinding needs. Every
// field is server-derived (the run/repo the api resolved, the server-built deep link) —
// no untrusted agent text rides in here.
type IncidentalFindingNotifyInput struct {
	UserID    uuid.UUID
	RunID     uuid.UUID
	RepoID    uuid.UUID
	RepoPath  string
	FindingID uuid.UUID
	Link      string
}

// NotifyIncidentalFinding is the PRD #333 D6 coalescing entry point: a finding with no
// latch row for its (user, run) inserts one row and fires one Slack DM (via the existing
// Notify persist-first + prune + Slack path); a later finding for the SAME run finds that
// row, bumps its payload count and appends the finding id WITHOUT re-firing Slack. The
// lookup ignores read state (PRD #1650 D4), so a row read before the inbox was retired
// still latches.
//
// Coalescing is BEST-EFFORT, not exactly-once: the per-user prune (DefaultUserCap) can
// evict a long run's latch row, and two concurrent first findings on one run can both
// miss the lookup before either inserts. Either case sends the user a second DM. The
// caller resolves whether to notify at all (a suppressed matching-hash re-report never
// calls this, R2) and logs-and-swallows any error — the finding is already durably stored,
// so a notification failure must never fail the capture.
func (s *Service) NotifyIncidentalFinding(ctx context.Context, in IncidentalFindingNotifyInput) error {
	existing, err := s.q.FindNotificationForRunKind(ctx, store.FindNotificationForRunKindParams{
		UserID: in.UserID,
		RunID:  in.RunID,
		Kind:   KindIncidentalFinding,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No latch row ⇒ treated as the run's FIRST finding: persist the row and fire one
		// Slack DM. Notify owns persist-first + prune + best-effort Slack.
		runID := in.RunID
		_, nerr := s.Notify(ctx, Notification{
			UserID: in.UserID,
			Kind:   KindIncidentalFinding,
			Payload: IncidentalFindingPayload{
				RunID:      in.RunID,
				RepoID:     in.RepoID,
				RepoPath:   in.RepoPath,
				Count:      1,
				FindingIDs: []uuid.UUID{in.FindingID},
			},
			RunID: &runID,
			Slack: &SlackRender{
				Title: "🐛 Run flagged an incidental finding",
				Body:  in.RepoPath,
				Link:  in.Link,
				Emoji: "🐛",
			},
		})
		return nerr
	case err != nil:
		return err
	default:
		// A latch row exists ⇒ a SUBSEQUENT finding on the same run: bump the count and
		// append the id, then rewrite the payload. NO Slack (D6: the DM fired on the first
		// finding).
		var payload IncidentalFindingPayload
		if derr := json.Unmarshal(existing.Payload, &payload); derr != nil {
			return derr
		}
		payload.Count++
		if len(payload.FindingIDs) < maxCoalescedFindingIDs {
			payload.FindingIDs = append(payload.FindingIDs, in.FindingID)
		}
		b, merr := json.Marshal(payload)
		if merr != nil {
			return merr
		}
		_, uerr := s.q.UpdateNotificationPayload(ctx, store.UpdateNotificationPayloadParams{
			Payload: b,
			ID:      existing.ID,
			UserID:  in.UserID,
		})
		return uerr
	}
}

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
func (s *Service) NotifyEarlyReset(ctx context.Context, userID uuid.UUID, expected, observed time.Time) (store.Notification, error) {
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
		},
	})
}
