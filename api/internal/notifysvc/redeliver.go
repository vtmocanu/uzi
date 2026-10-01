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
}

// NewRedeliverer builds a Redeliverer. A nil slack makes Pass a no-op; a nil logger
// falls back to slog.Default.
func NewRedeliverer(q RedeliveryStore, slack Slacker, logger *slog.Logger) *Redeliverer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Redeliverer{q: q, slack: slack, logger: logger}
}

// Pass claims one batch of pending durable rows and re-enqueues each on the Slacker with
// its row id as DeliveryID. It returns how many were handed to the Slacker: an enqueue
// the notifier drops on a full queue still counts, and that row stays pending for a later
// claim. A row whose stored render cannot be decoded is logged and skipped; it keeps being
// claimed until it exhausts MaxSlackAttempts and then stops. With no Slacker it claims
// nothing.
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
