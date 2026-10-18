package slacksvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
)

// notifyCodexCurrent is the last authority read before PostBlocks. A change after
// this read can race the post; we accept that window and hold no DB lock over Slack IO.
func (n *Notifier) notifyCodexCurrent(ctx context.Context, ev notifyEvent) bool {
	if ev.deliveryTag == "" && ev.codexAccount == nil {
		return true
	}
	if ev.deliveryTag != notifysvc.DeliveryTagCodexEarlyReset || ev.codexAccount == nil || ev.credential != nil ||
		!ev.codexAccount.Valid() || ev.codexAccount.OwnerID != ev.userID {
		n.logger.Warn("slack: dropped Codex notification")
		return false
	}
	current, err := n.store.IsCodexEarlyResetCurrent(ctx, ev.codexAccount.Params())
	if err != nil || !current {
		n.logger.Warn("slack: dropped Codex notification")
		return false
	}
	return true
}
