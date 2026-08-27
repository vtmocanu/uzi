// Package notifysvc is the single write seam for the notifications inbox
// (PRD #46 Decision 6, M2). Every feature that wants to notify a user — the
// judge (M4), the self-improvement engine (M5), anything later — calls Notify
// rather than touching the table or Slack directly, so one place owns the
// load-bearing ordering: the row is PERSISTED FIRST, then Slack is attempted
// best-effort. That mirrors the run_messages discipline (persist, then
// broadcast): a Slack outage, an unlinked user, or a full notifier queue can
// never lose the inbox row, and the inbox is the source of truth the SPA reads.
//
// The service is generic: it knows nothing about judges. The caller supplies the
// kind, a jsonb payload (the inbox render data), optional run/review deep-link
// anchors, and an optional Slack rendering. The judge is simply tenant #1.
package notifysvc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// DefaultUserCap is the per-user retention cap: the newest this-many notifications
// are kept, older ones pruned on write (Decision 6 — pruning ships with the table,
// not later). Sized so a busy opted-in user's judge history stays browsable while
// the table can't grow without bound. Overridable via New for tests.
const DefaultUserCap = 200

// Store is the slice of generated queries the service needs. *store.Queries
// satisfies it; tests inject a fake to assert the persist-first ordering and the
// prune call.
type Store interface {
	InsertNotification(ctx context.Context, arg store.InsertNotificationParams) (store.Notification, error)
	PruneNotificationsForUser(ctx context.Context, arg store.PruneNotificationsForUserParams) (int64, error)
	// GetRunByID loads a run by id (PRD #284 M5). The run-failure notifier reads
	// through its own narrower runReader, but keeping this on Store lets the same
	// *store.Queries value back both the notify seam and the adapter.
	GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error)
	// FindUnreadNotificationForRunKind / UpdateNotificationPayload are the PRD #333 D6
	// per-run coalescing pair: find the run's still-unread finding notification (a miss
	// ⇒ this is the run's first finding, insert + one Slack DM), else bump its payload
	// count WITHOUT re-firing Slack. See NotifyIncidentalFinding.
	FindUnreadNotificationForRunKind(ctx context.Context, arg store.FindUnreadNotificationForRunKindParams) (store.Notification, error)
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

// New builds a Service. slack may be nil (delivery is then inbox-only). A
// non-positive cap falls back to DefaultUserCap.
func New(q Store, slack Slacker, cap int, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	c := int32(cap)
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
// fields before they leave the box; the inbox row is the durable copy regardless.
type SlackRender struct {
	Title string
	Body  string
	Link  string
	Emoji string
	Facts []string
}

// CIAutofixPayload is the jsonb the inbox renders for every ci_autofix_* notification
// kind (started / halted from the poller, landed from forgesvc). It lives here — the
// one package all three producers already import — so all three kinds carry one shape
// instead of drifting between a typed poller struct and a forgesvc map. Optional
// fields are omitempty: landed carries no issue iid or reason, started carries no
// reason, so only halted sets Reason and only the poller kinds set IssueIID.
type CIAutofixPayload struct {
	Ref            string `json:"ref"`
	PipelineWebURL string `json:"pipeline_web_url"`
	IssueIID       int64  `json:"issue_iid,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// Notification is the input to Notify: a user to notify, a kind + jsonb payload
// for the inbox render, optional run/review anchors (both ON DELETE CASCADE at the
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

// Notify persists the notification row, then prunes the user's inbox to the cap
// (best-effort), then enqueues the Slack DM (best-effort). The persisted row is
// returned. Only a failure to persist is fatal to the call — the inbox is the
// source of truth, so prune/Slack failures are logged and swallowed. The prune and
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
		RunID:    optionalUUID(n.RunID),
		ReviewID: optionalUUID(n.ReviewID),
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

// optionalUUID maps a *uuid.UUID to the pgtype the generated params expect: a nil
// pointer becomes an SQL NULL (no anchor), a set pointer a valid uuid.
func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// KindIncidentalFinding is the notifications.kind for a coalesced incidental-finding
// notification (PRD #333 D6). kind is a generic text column with no CHECK, so this needs
// no migration; the value is the coalescing key alongside (user_id, run_id).
const KindIncidentalFinding = "incidental_finding"

// maxCoalescedFindingIDs caps the finding_ids the coalesced payload accumulates so a
// noisy run cannot grow one inbox row's jsonb without bound. The count keeps climbing past
// the cap (it is the badge/headline number); only the id list stops appending. The per-run
// capture cap (workersvc.MaxFindingsPerRun) is far below this, so in practice the cap is
// defense-in-depth, not a limit users meet.
const maxCoalescedFindingIDs = 50

// IncidentalFindingPayload is the jsonb the inbox renders for an incidental_finding
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

// NotifyIncidentalFinding is the PRD #333 D6 coalescing entry point: the run's FIRST
// finding inserts one inbox row and fires exactly one Slack DM (via the existing Notify
// persist-first + prune + Slack path); every SUBSEQUENT finding for the SAME run bumps
// that unread row's payload count and appends the finding id WITHOUT re-firing Slack, so
// the bell badge and inbox read "Run flagged M findings" while the user is DM'd once. The
// caller resolves whether to notify at all (a suppressed matching-hash re-report never
// calls this, R2) and logs-and-swallows any error — the finding is already durably stored,
// so a notification failure must never fail the capture.
func (s *Service) NotifyIncidentalFinding(ctx context.Context, in IncidentalFindingNotifyInput) error {
	existing, err := s.q.FindUnreadNotificationForRunKind(ctx, store.FindUnreadNotificationForRunKindParams{
		UserID: in.UserID,
		RunID:  in.RunID,
		Kind:   KindIncidentalFinding,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No coalescible unread row ⇒ the run's FIRST finding: persist the inbox row and
		// fire one Slack DM. Notify owns persist-first + prune + best-effort Slack.
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
		// A coalescible unread row exists ⇒ a SUBSEQUENT finding on the same run: bump the
		// count and append the id, then rewrite the payload. NO Slack (D6: the DM fired on
		// the first finding). The row stays unread so it keeps counting toward the bell.
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
