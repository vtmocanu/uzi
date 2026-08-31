// This file is the vault-lock Slack notice reconciler (PRD #890 M2). It notifies
// each user whose vault is locked while they have work the lock actually blocks —
// a run that is queued, awaiting_approval, or awaiting_input (all of which need
// an unlocked vault to proceed), or a schedule that will fire — so they unlock
// before their queued or scheduled work silently stalls after a deploy.
//
// It is a STANDALONE type, deliberately NOT a method on *Scheduler (Decision D1):
// the scheduler goroutine is gated on cfg.SchedulerCheckInterval > 0 and runs an
// immediate boot tick, so folding the reconciler into it would (a) silently die
// when scheduled runs are disabled and (b) fire un-delayed. M3 invokes this from
// main.go's boot path independently of that gate. It reuses the schedsvc Store,
// VaultGate, Notifier and SettingsReader interfaces the scheduler already defines
// (widened by M1 + M2 step 1) so main.go can build it from the same collaborators.
package schedsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
)

// KindVaultLocked is the notifications.kind for the vault-lock notice (PRD #890 M2).
// kind is a generic text column with no CHECK, so this needs no migration — it is a
// free-text discriminator, defined here for symmetry with notifysvc.KindIncidentalFinding.
const KindVaultLocked = "vault_locked"

// VaultLockReconciler sends the one-per-episode vault-lock Slack notice. It holds the
// same collaborators the scheduler does (a narrow Store, the local vault gate, the
// notify seam, and the settings reader for the deep-link base URL), so main.go builds
// it from what it already wires for the scheduler.
type VaultLockReconciler struct {
	store    Store
	vault    VaultGate
	notifier Notifier
	settings SettingsReader
	logger   *slog.Logger
}

// NewVaultLockReconciler builds a VaultLockReconciler. A nil logger defaults to
// slog.Default(). vault may be nil (a deployment without the vault) — a nil gate is
// treated as "locked" (do not skip), so the DB atomic claim remains the dedup of record.
func NewVaultLockReconciler(store Store, vault VaultGate, notifier Notifier, settings SettingsReader, logger *slog.Logger) *VaultLockReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &VaultLockReconciler{store: store, vault: vault, notifier: notifier, settings: settings, logger: logger}
}

// Reconcile notifies every eligible locked user once. It is best-effort: every error
// is logged, never returned, and one user's failure never aborts the rest.
//
// Per user: a local vlt.Unlocked skip (fail-safe — can only SUPPRESS a send, covering
// the boot-unlocked seed admin and anyone unlocked on this pod), then the atomic
// ClaimVaultLockNotice claim (pgx.ErrNoRows ⇒ another replica/tick already claimed it,
// skip silently) — only the returned id proceeds. The mark is set BEFORE Notify, so the
// send is at-most-once (Decision D2) and atomic across replicas (Decision D1).
func (r *VaultLockReconciler) Reconcile(ctx context.Context) {
	rows, err := r.store.ListUsersNeedingVaultLockNotice(ctx)
	if err != nil {
		r.logger.Error("vault-lock notice: list eligible users", "error", err)
		return
	}
	// Read the base URL once. Empty on error — the notice degrades gracefully by
	// omitting the deep link and still sending (matching the other notifiers).
	base, _ := r.settings.PublicBaseURL(ctx)

	for _, row := range rows {
		// Local fail-safe skip: the user is unlocked in THIS process. Covers the
		// boot-unlocked seed admin and anyone unlocked on this pod. A nil gate is
		// treated as locked (do not skip); the nil is guarded so it cannot panic.
		if r.vault != nil && r.vault.Unlocked(row.ID) {
			continue
		}
		// Atomic claim: only the returned id proceeds. A no-row result means another
		// replica or tick already claimed this user's slot — skip silently.
		claimed, err := r.store.ClaimVaultLockNotice(ctx, row.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			r.logger.Error("vault-lock notice: claim", "user", row.ID.String(), "error", err)
			continue
		}
		n := buildVaultLockNotification(base, claimed, row.PendingRuns, row.PendingSchedules)
		if _, err := r.notifier.Notify(ctx, n); err != nil {
			r.logger.Warn("vault-lock notice: notify", "user", claimed.String(), "error", err)
		}
	}
}

// vaultLockTitle / vaultLockBody are the fixed, cause-neutral copy (Decision D6): the
// reconciler observes any locked vault — restart-locked or user-locked — so the notice
// NEVER asserts a restart caused the lock. No user or LLM text is interpolated.
const (
	vaultLockTitle = "Vault locked"
	vaultLockBody  = "Your vault is locked — unlock it so your queued and scheduled runs can proceed."
)

// buildVaultLockNotification assembles the vault-lock notice (PRD #890 M2). It is PURE
// (no I/O) so its shape is unit-testable. The title and body are fixed cause-neutral
// text; the Facts are TRUSTED, built from the closed pending-work counts (never user or
// LLM text). The deep link is server-built from the operator-set base URL; an empty base
// yields no link. The notice is user-scoped (no run/review anchor).
func buildVaultLockNotification(baseURL string, userID uuid.UUID, pendingRuns, pendingSchedules int64) notifysvc.Notification {
	return notifysvc.Notification{
		UserID: userID,
		Kind:   KindVaultLocked,
		Payload: map[string]any{
			"title":             vaultLockTitle,
			"body":              vaultLockBody,
			"pending_runs":      pendingRuns,
			"pending_schedules": pendingSchedules,
		},
		Slack: &notifysvc.SlackRender{
			Emoji: "🔒",
			Title: vaultLockTitle,
			Body:  vaultLockBody,
			Link:  vaultDeepLink(baseURL),
			Facts: vaultLockFacts(pendingRuns, pendingSchedules),
		},
	}
}

// vaultLockFacts summarizes the blocked work as TRUSTED mrkdwn Facts, one per count that
// is > 0. A user reaches the eligibility list via the queued-run OR firing-schedule
// predicate, so at least one count is > 0. Counts of 0 are omitted.
func vaultLockFacts(pendingRuns, pendingSchedules int64) []string {
	var facts []string
	if pendingRuns > 0 {
		facts = append(facts, fmt.Sprintf("*%d* queued %s", pendingRuns, plural(pendingRuns, "run", "runs")))
	}
	if pendingSchedules > 0 {
		facts = append(facts, fmt.Sprintf("*%d* scheduled %s", pendingSchedules, plural(pendingSchedules, "job", "jobs")))
	}
	return facts
}

// plural picks the singular or plural noun for n.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// vaultDeepLink builds the Slack DM deep link to the app home from the operator-set
// public base URL. The VaultLockedBanner / VaultControls are global there, so the base
// URL root is the right landing. An empty base (unset, or the settings lookup failed)
// yields "" so the notice simply carries no link — matching reviewDeepLink/runDeepLink
// and TestBuildReviewNotificationNoBaseURL.
func vaultDeepLink(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}
