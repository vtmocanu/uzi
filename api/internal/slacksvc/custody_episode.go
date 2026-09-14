// This file is the owner-level blocked-custody episode Slack DM reconciler (PRD #1349 M6,
// Decision D10). It is modeled EXACTLY on schedsvc.VaultLockReconciler: a STANDALONE type —
// deliberately NOT folded into any tick — that main.go wires from its boot path. Per episode it
// sends AT MOST ONE DM to each owner whose unpublished-work custody holds have crossed the
// admission limit and are blocking new code runs. It COALESCES by owner (one DM per blocked
// episode, never one per queued run), reusing the SAME atomic-claim dedup the vault-lock notice
// uses, and re-arms when the owner drops back below the limit so a later crossing notifies afresh.
//
// It lives in slacksvc (not schedsvc) because the DM it composes IS a Slack surface and because
// it reuses the health-notification ENABLEMENT gate (Decision D10: no new enable flag). slacksvc
// already imports workersvc (the admission limit) and notifysvc (the persist-first Notify seam),
// so there is no new dependency and no import cycle — notifysvc never imports slacksvc.
//
// The per-run custody-limit health NUDGE is suppressed in workersvc/health.go so this owner-level
// DM owns that crossing; the per-run health STATE (the pill) is unchanged there. See health.go.
package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// KindCustodyEpisode is the notifications.kind for the owner blocked-custody episode DM. kind is
// a free-text column with no CHECK, so this needs no migration (like notifysvc's kinds and
// schedsvc.KindVaultLocked). The web inbox renderer keys off this kind and reads the payload.
const KindCustodyEpisode = "custody_episode"

// custodyEpisodeStore is the DB surface the reconciler needs: the two M6 find-owners reads, the
// M1 atomic claim / re-arm pair, and the frozen M1 owner aggregate. *store.Queries satisfies it;
// tests inject a fake.
type custodyEpisodeStore interface {
	ListOwnersOverCustodyLimit(ctx context.Context, custodyHoldLimit int32) ([]uuid.UUID, error)
	ListOwnersWithClearedCustodyEpisode(ctx context.Context, custodyHoldLimit int32) ([]uuid.UUID, error)
	ClaimCustodyEpisodeNotice(ctx context.Context, userID uuid.UUID) (uuid.UUID, error)
	ClearCustodyEpisodeNotice(ctx context.Context, userID uuid.UUID) error
	GetCustodyAggregateForOwner(ctx context.Context, arg store.GetCustodyAggregateForOwnerParams) (store.GetCustodyAggregateForOwnerRow, error)
}

// custodyEpisodeNotifier is the notifysvc write seam (persist-first, best-effort Slack).
// *notifysvc.Service satisfies it — the same seam the vault-lock reconciler uses.
type custodyEpisodeNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// custodyEpisodeSettings is the settings surface the reconciler reads: the health-notification
// enablement gate it REUSES (Decision D10: no new enable flag) and the operator base URL its deep
// link is built from. *settings.Cache satisfies it.
type custodyEpisodeSettings interface {
	HealthEnabled(ctx context.Context) (bool, error)
	PublicBaseURL(ctx context.Context) (string, error)
}

// CustodyEpisodeReconciler sends the one-per-episode owner blocked-custody Slack DM. It holds the
// narrow collaborators main.go already wires (the store, the notify seam, the settings reader) and
// the custody admission limit (workersvc.CustodyHoldLimit), so a crossing at that exact limit is
// what it notifies on.
type CustodyEpisodeReconciler struct {
	store    custodyEpisodeStore
	notifier custodyEpisodeNotifier
	settings custodyEpisodeSettings
	limit    int
	logger   *slog.Logger
}

// NewCustodyEpisodeReconciler builds a CustodyEpisodeReconciler. limit is the custody admission
// cap (main.go passes workersvc.CustodyHoldLimit); a non-positive limit disables the admission
// gate, so Reconcile becomes a no-op. A nil logger defaults to slog.Default().
func NewCustodyEpisodeReconciler(store custodyEpisodeStore, notifier custodyEpisodeNotifier, settings custodyEpisodeSettings, limit int, logger *slog.Logger) *CustodyEpisodeReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &CustodyEpisodeReconciler{store: store, notifier: notifier, settings: settings, limit: limit, logger: logger}
}

// Reconcile re-arms closed episodes then notifies every owner freshly at/over the custody limit,
// once per episode. It is best-effort: every error is logged, never returned, and one owner's
// failure never aborts the rest.
//
// Order per tick:
//  1. ENABLEMENT gate — reuse the health-notification enablement (Decision D10, no new enable
//     flag). A disabled or unreadable setting suppresses the DM; the per-run nudge is suppressed
//     in health.go regardless, so an enable-off deployment sends neither custody surface.
//  2. RE-ARM — clear the episode notice for every already-notified owner who has dropped below
//     the limit, so a later re-crossing notifies afresh.
//  3. NOTIFY — for every owner at/over the limit, the atomic ClaimCustodyEpisodeNotice claim
//     (pgx.ErrNoRows ⇒ another replica/tick already claimed this episode, skip silently); only
//     the returned id proceeds, so N booting pods send EXACTLY ONE DM per episode. The mark is
//     set BEFORE Notify (at-most-once, Decision D10).
func (r *CustodyEpisodeReconciler) Reconcile(ctx context.Context) {
	enabled, err := r.settings.HealthEnabled(ctx)
	if err != nil {
		r.logger.Error("custody episode: read health_enabled", "error", err)
		return
	}
	if !enabled {
		return
	}
	// A non-positive limit DISABLES the admission gate (the claim path treats it the same), so
	// there is no episode to open or close. CustodyHoldLimit is a positive const, so this is a
	// defensive guard — and it also stops ListOwnersOverCustodyLimit from matching every owner.
	if r.limit <= 0 {
		return
	}
	limit := int32(r.limit) //nolint:gosec // G115: the custody admission limit is a small positive const (8)

	// Re-arm FIRST: clear the notice for owners who dropped below the limit. Best-effort — a
	// list/clear error is logged and the notify pass still runs.
	if cleared, cerr := r.store.ListOwnersWithClearedCustodyEpisode(ctx, limit); cerr != nil {
		r.logger.Error("custody episode: list cleared owners", "error", cerr)
	} else {
		for _, uid := range cleared {
			if err := r.store.ClearCustodyEpisodeNotice(ctx, uid); err != nil {
				r.logger.Warn("custody episode: clear notice", "user", uid.String(), "error", err)
			}
		}
	}

	owners, err := r.store.ListOwnersOverCustodyLimit(ctx, limit)
	if err != nil {
		r.logger.Error("custody episode: list owners over limit", "error", err)
		return
	}
	// Read the base URL once. Empty on error — the notice degrades gracefully by omitting the
	// deep link and still sending, matching the other notifiers.
	base, _ := r.settings.PublicBaseURL(ctx)

	for _, uid := range owners {
		// Atomic claim BEFORE the send: only the returned id proceeds. A no-row result means
		// another replica or tick already claimed this episode — skip silently.
		claimed, err := r.store.ClaimCustodyEpisodeNotice(ctx, uid)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			r.logger.Error("custody episode: claim", "user", uid.String(), "error", err)
			continue
		}
		// Read the exact facts (open holds + blocked runs) from the frozen M1 aggregate. If it
		// fails AFTER we claimed, re-arm the notice so a later tick retries — otherwise a transient
		// read error would burn this episode's one DM and it would never send.
		agg, err := r.store.GetCustodyAggregateForOwner(ctx, store.GetCustodyAggregateForOwnerParams{
			UserID:           claimed,
			CustodyHoldLimit: limit,
		})
		if err != nil {
			r.logger.Error("custody episode: read aggregate", "user", claimed.String(), "error", err)
			if cerr := r.store.ClearCustodyEpisodeNotice(ctx, claimed); cerr != nil {
				r.logger.Warn("custody episode: re-arm after aggregate error", "user", claimed.String(), "error", cerr)
			}
			continue
		}
		n := buildCustodyEpisodeNotification(base, claimed, agg.OpenHolds, agg.BlockedRuns)
		if _, err := r.notifier.Notify(ctx, n); err != nil {
			r.logger.Warn("custody episode: notify", "user", claimed.String(), "error", err)
		}
	}
}

// custodyEpisode copy is FIXED, server-authored FACTS-only text (Decision D10): no user or LLM
// free text is ever interpolated. The title/body are cause-neutral fixed strings; the Facts are
// TRUSTED, built from the closed aggregate counts and one compile-time literal command.
const (
	custodyEpisodeTitle = "Recovery custody is blocking new runs"
	custodyEpisodeBody  = "You're at the recovery custody limit, so new runs can't start until you resolve some held work. Review your held work, then export what you want to keep and discard the rest."
	// custodyDiscardCommand is the EXACT remediation command as a FIXED TRUSTED STRING with
	// literal placeholder text (owner-level, not per-hold) — copied verbatim from the CLI
	// spelling defined in M5. It is a compile-time literal, NEVER interpolated from run/hold ids
	// or any user/LLM text, so it can carry no injection.
	custodyDiscardCommand = "uzi run discard <run-id> --hold <hold-id> --yes"
)

// buildCustodyEpisodeNotification assembles the owner blocked-custody episode DM (Decision D10).
// It is PURE (no I/O) so its shape is unit-testable. The title/body are fixed cause-neutral text;
// the Facts are TRUSTED, built from the server-computed aggregate counts plus the fixed discard
// command (never user or LLM text). The deep link is server-built from the operator base URL (the
// /workers custody resolution surface, D8); an empty base yields no link. User-scoped (no
// run/review anchor) — the episode is owner-level, not per-run.
func buildCustodyEpisodeNotification(baseURL string, userID uuid.UUID, openHolds, blockedRuns int64) notifysvc.Notification {
	return notifysvc.Notification{
		UserID: userID,
		Kind:   KindCustodyEpisode,
		Payload: map[string]any{
			"title":        custodyEpisodeTitle,
			"body":         custodyEpisodeBody,
			"open_holds":   openHolds,
			"blocked_runs": blockedRuns,
			"command":      custodyDiscardCommand,
		},
		Slack: &notifysvc.SlackRender{
			Emoji: "🛑",
			Title: custodyEpisodeTitle,
			Body:  custodyEpisodeBody,
			Link:  custodyDeepLink(baseURL),
			Facts: custodyEpisodeFacts(openHolds, blockedRuns),
		},
	}
}

// custodyEpisodeFacts renders the aggregate as TRUSTED mrkdwn Facts: the open-hold count, the
// blocked-run count, and the exact discard command as a `code` chip. All three are server-built
// from closed ints and a compile-time literal, so the `*bold*`/`code` markup is intended (Facts
// are ScrubSecrets'd but NOT mrkdwn-escaped by the notifier). The count facts always render (the
// reconciler only reaches here for an owner at the limit, so open_holds >= limit > 0); a
// blocked_runs of 0 (all held runs momentarily off-queue) still renders "0 blocked runs" so the
// pressure is not understated to nothing.
func custodyEpisodeFacts(openHolds, blockedRuns int64) []string {
	return []string{
		fmt.Sprintf("*%d* held %s", openHolds, plural(openHolds, "source", "sources")),
		fmt.Sprintf("*%d* blocked %s", blockedRuns, plural(blockedRuns, "run", "runs")),
		"Discard held work: `" + custodyDiscardCommand + "`",
	}
}

// custodyDeepLink builds the Slack DM deep link to the /workers custody resolution surface (the
// durable detailed hold list, Decision D8) from the operator-set public base URL. An empty base
// (unset, or the settings lookup failed) yields "" so the notice simply carries no link — the
// same empty-base behaviour as runLink and the vault-lock notice.
func custodyDeepLink(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/workers"
}

// plural picks the singular or plural noun for n.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
