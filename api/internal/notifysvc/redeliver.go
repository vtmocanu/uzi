package notifysvc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// redeliverBatch caps how many pending durable rows one Pass claims.
const redeliverBatch = 50

// RedeliveryStore is the narrow query slice the Redeliverer needs. It is deliberately
// separate from Store so the Notify fakes need not implement it. *store.Queries
// satisfies it.
type RedeliveryStore interface {
	PruneNotificationsForUser(ctx context.Context, arg store.PruneNotificationsForUserParams) (int64, error)
	ClaimPendingSlackNotifications(ctx context.Context, arg store.ClaimPendingSlackNotificationsParams) ([]store.ClaimPendingSlackNotificationsRow, error)
}

// Redeliverer re-enqueues the Slack DM of durable notifications (issue #1675) whose
// delivery was never confirmed: the in-memory enqueue was dropped (full queue, restart)
// or the post failed. It runs as a sweeper pass. Delivery is at-least-once: the claim
// stamps the attempt before the publish, and the notifier marks the row delivered only
// after it posted, so a crash between the post and the mark can repeat a DM.
type Redeliverer struct {
	q      RedeliveryStore
	slack  Slacker
	logger *slog.Logger
	cap    int32
}

// RedelivererOption configures a Redeliverer at construction.
type RedelivererOption func(*Redeliverer)

// WithRedeliveryUserCap sets the per-user retention cap after exhaustion.
// Non-positive values fall back to DefaultUserCap, as in New.
func WithRedeliveryUserCap(cap int) RedelivererOption {
	c := int32(cap) //nolint:gosec // G115: small per-user retention cap, matching New
	if c <= 0 {
		c = DefaultUserCap
	}
	return func(r *Redeliverer) { r.cap = c }
}

// NewRedeliverer builds a Redeliverer. A nil slack makes Pass a no-op; a nil logger
// falls back to slog.Default.
func NewRedeliverer(q RedeliveryStore, slack Slacker, logger *slog.Logger, opts ...RedelivererOption) *Redeliverer {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Redeliverer{q: q, slack: slack, logger: logger, cap: DefaultUserCap}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Pass claims one batch of pending durable rows and re-enqueues each on the Slacker with
// its row id as DeliveryID. It returns how many were handed to the Slacker: an enqueue
// the notifier drops on a full queue still counts, and that row stays pending for a later
// claim while below MaxSlackAttempts. A row whose stored render cannot be decoded is
// logged and skipped; it keeps being claimed until it exhausts MaxSlackAttempts.
// The final claim attempts best-effort per-user pruning before decoding or publishing,
// including for a corrupt render (issue #2076). Successful pruning removes eligible
// older rows without a later Notify; pending durable rows and timestamp ties remain.
// A prune failure is logged without undoing exhaustion or blocking a valid final DM.
// This pass does not sweep historical settled rows. With no Slacker it claims nothing.
func (r *Redeliverer) Pass(ctx context.Context) (int64, error) {
	if r.slack == nil {
		return 0, nil
	}
	rows, err := r.q.ClaimPendingSlackNotifications(ctx, store.ClaimPendingSlackNotificationsParams{
		RetryAfterSecs: int32(SlackRetryAfter / time.Second),
		MaxAttempts:    MaxSlackAttempts,
		Lim:            redeliverBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("claim pending slack notifications: %w", err)
	}
	var published int64
	for _, row := range rows {
		last := row.SlackAttempts >= MaxSlackAttempts
		if last {
			if _, err := r.q.PruneNotificationsForUser(ctx, store.PruneNotificationsForUserParams{
				UserID: row.UserID, Keep: r.cap, MaxAttempts: MaxSlackAttempts,
			}); err != nil {
				r.logger.Warn("notify: prune after slack exhaustion failed", "user", row.UserID.String(), "error", err)
			}
			r.logger.Warn("notify: slack redelivery giving up after this attempt",
				"notification", row.ID.String(), "user", row.UserID.String(), "attempts", row.SlackAttempts)
		}
		var d durableRender
		if err := json.Unmarshal(row.SlackRender, &d); err != nil {
			r.logger.Warn("notify: stored slack render is corrupt; skipping",
				"notification", row.ID.String(), "error", err)
			continue
		}
		r.slack.PublishNotification(row.UserID, d.render(row.ID))
		published++
	}
	return published, nil
}
