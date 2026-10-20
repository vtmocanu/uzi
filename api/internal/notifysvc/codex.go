package notifysvc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// DeliveryTagCodexEarlyReset requires an account fence even when its pointer is missing.
const DeliveryTagCodexEarlyReset = "codex_early_reset"

// CodexAccountFence binds one owner's account alert to the observed authority and aliases.
type CodexAccountFence struct {
	OwnerID, AccountID             uuid.UUID
	Generation, CredentialRevision int64
	EnablementSig                  string
}

// Valid accepts initial 0/0 authority counters, but requires scoped identities and a signature.
func (f CodexAccountFence) Valid() bool {
	return f.OwnerID != uuid.Nil && f.AccountID != uuid.Nil && strings.TrimSpace(f.EnablementSig) != "" && f.Generation >= 0 && f.CredentialRevision >= 0
}

// Params is the exact owner/account authority read shared by creation and dispatch.
func (f CodexAccountFence) Params() store.IsCodexEarlyResetCurrentParams {
	return store.IsCodexEarlyResetCurrentParams{UserID: f.OwnerID, ProviderAccountID: f.AccountID,
		ObservedGeneration: f.Generation, ObservedCredentialRevision: f.CredentialRevision, EnablementSig: f.EnablementSig}
}

// NotifyCodexEarlyReset checks current account authority before the persist-first notification seam.
func (s *Service) NotifyCodexEarlyReset(ctx context.Context, fence CodexAccountFence, expected, observed time.Time) (store.Notification, error) {
	if !fence.Valid() {
		return store.Notification{}, ErrCredentialNotCurrent
	}
	current, err := s.q.IsCodexEarlyResetCurrent(ctx, fence.Params())
	if err != nil || !current {
		return store.Notification{}, ErrCredentialNotCurrent
	}
	// Use Unix seconds so the supported RFC3339 range is not capped by
	// time.Duration's roughly 292-year span. Preserve nearest-hour rounding,
	// including subsecond boundaries and ties away from zero.
	const hourSeconds = int64(time.Hour / time.Second)
	seconds := expected.Unix() - observed.Unix()
	hours := int(seconds / hourSeconds)
	remainder, nanos := seconds%hourSeconds, expected.Nanosecond()-observed.Nanosecond()
	if remainder > hourSeconds/2 || (remainder == hourSeconds/2 && nanos >= 0) {
		hours++
	} else if remainder < -hourSeconds/2 || (remainder == -hourSeconds/2 && nanos <= 0) {
		hours--
	}
	return s.Notify(ctx, Notification{
		UserID: fence.OwnerID, Kind: KindEarlyLimitReset,
		Payload: EarlyResetPayload{Title: "Codex weekly limit reset early", Expected: expected.Format(time.RFC3339), Observed: observed.Format(time.RFC3339), HoursEarly: hours},
		Slack: &SlackRender{Emoji: "🚨", Title: "CODEX WEEKLY LIMIT RESET EARLY",
			Body: "Your weekly Codex limit reopened ahead of schedule.",
			Facts: []string{fmt.Sprintf("reset ~%dh early", hours),
				fmt.Sprintf("observed <!date^%d^{time}|%s>", observed.Unix(), observed.UTC().Format("15:04 MST")),
				fmt.Sprintf("expected <!date^%d^{time}|%s>", expected.Unix(), expected.UTC().Format("15:04 MST"))},
			DeliveryTag: DeliveryTagCodexEarlyReset, CodexAccount: &fence},
	})
}
