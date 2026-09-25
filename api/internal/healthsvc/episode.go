package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// EpisodeReconciler opens and closes DANGER health episodes and, from M6, fans a single
// per-admin notice out for each episode (PRD #1484 M2 D14 + M6). It is a STANDALONE
// reconciler wired from main.go beside the custody-episode reconciler, running the SAME
// registry once a minute through the SHARED Service so the endpoint and the episode
// lifecycle can never disagree about the instance's health.
//
// The notice fan-out is DEBOUNCED by two evaluations, exactly like the PRD prescribes: the
// tick that OPENS the episode sends nothing, and only the NEXT still-danger tick (which
// finds the episode already open) claims and notifies. So a one-tick danger blip that
// recovers on the next tick opens an episode, closes it, and never wakes anyone.
//
// The notice reuses the SAME health-notification ENABLEMENT gate the custody-episode
// reconciler uses (settings.HealthEnabled, PRD #1484 line 186 / D10: no new enable flag) and
// the SAME persist-first per-user notifysvc.Notify seam (the row lands in the pruned,
// write-only notifications event log; the Slack DM to a linked admin is the only surface). Per-admin exactly-once is the atomic
// ClaimHealthEpisodeNotice claim on the (episode_id, user_id) PK — a claim that inserts sends
// one notice, a claim that no-ops (already notified) skips silently, so replays and a second
// replica never double-notify.
//
// Its decision logic is unit-tested with fakes (this is not a live-DB test — healthsvc is
// not in the sweep-enumerated live-DB package list; the claim query's atomicity is proven in
// the store package, the custody precedent).
type EpisodeReconciler struct {
	eval     healthEvaluator
	store    episodeStore
	notifier episodeNotifier
	settings episodeSettings
	now      func() time.Time
	logger   *slog.Logger
}

// healthEvaluator is the one method the reconciler needs from the shared Service, kept as
// an interface so a fake Doc can drive the open/close/notify decision without a database.
type healthEvaluator interface {
	Evaluate(ctx context.Context) (Doc, error)
}

// episodeStore is the episode-lifecycle slice of *store.Queries the reconciler uses: the
// open-episode read, the atomic open, the idempotent close, the admin fan-out set, and the
// atomic per-admin notice claim.
type episodeStore interface {
	GetOpenHealthEpisode(ctx context.Context) (store.GetOpenHealthEpisodeRow, error)
	OpenHealthEpisode(ctx context.Context, openedAt pgtype.Timestamptz) (uuid.UUID, error)
	CloseHealthEpisode(ctx context.Context, arg store.CloseHealthEpisodeParams) error
	ListAdmins(ctx context.Context) ([]uuid.UUID, error)
	ClaimHealthEpisodeNotice(ctx context.Context, arg store.ClaimHealthEpisodeNoticeParams) (int64, error)
}

// episodeNotifier is the notifysvc write seam (persist-first, best-effort Slack).
// *notifysvc.Service satisfies it — the same seam the custody-episode reconciler uses.
type episodeNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// episodeSettings is the settings surface the reconciler reads: the health-notification
// ENABLEMENT gate it REUSES (PRD #1484 line 186 / D10, no new enable flag) and the operator
// base URL its deep link is built from. *settings.Cache satisfies it.
type episodeSettings interface {
	HealthEnabled(ctx context.Context) (bool, error)
	PublicBaseURL(ctx context.Context) (string, error)
}

// NewEpisodeReconciler builds an EpisodeReconciler. eval is the shared Service, st is
// *store.Queries, notifier is *notifysvc.Service, settings is *settings.Cache. A nil logger
// defaults to slog.Default().
func NewEpisodeReconciler(eval healthEvaluator, st episodeStore, notifier episodeNotifier, settings episodeSettings, logger *slog.Logger) *EpisodeReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &EpisodeReconciler{eval: eval, store: st, notifier: notifier, settings: settings, now: time.Now, logger: logger}
}

// Reconcile runs one evaluation and moves the episode lifecycle by exactly one step:
//   - overall danger AND no episode open  -> open one and RETURN (the opener sends NO
//     notice; a 23505 unique violation means another replica opened first, which is
//     already-open, not an error). This is the first half of the two-evaluation debounce.
//   - overall danger AND an episode ALREADY open (opened by a PRIOR tick) -> the debounce is
//     satisfied: fan the one-per-admin notice out (gated on HealthEnabled).
//   - overall not-danger AND an episode open -> close it (the re-arm; no recovery notice, D11).
//
// warn/unknown never notify — only danger opens an episode and fires the fan-out. Everything
// else is a no-op. Best-effort: every error is logged, never returned.
func (r *EpisodeReconciler) Reconcile(ctx context.Context) {
	doc, err := r.eval.Evaluate(ctx)
	if err != nil {
		r.logger.Error("health episode: evaluate", "error", err)
		return
	}
	danger := doc.Status == sevDanger

	open, err := r.store.GetOpenHealthEpisode(ctx)
	hasOpen := true
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			hasOpen = false
		} else {
			r.logger.Error("health episode: read open episode", "error", err)
			return
		}
	}

	switch {
	case danger && !hasOpen:
		if _, err := r.store.OpenHealthEpisode(ctx, pgconv.Time(r.now())); err != nil {
			if isUniqueViolation(err) {
				// The partial unique index rejected a second concurrent open: another
				// replica opened the episode first. That is exactly the desired outcome —
				// there is one open episode — so treat it as success, not an error.
				return
			}
			r.logger.Error("health episode: open", "error", err)
		}
		// The OPENER tick sends NO notice: the debounce fires the fan-out on the NEXT
		// still-danger tick, which finds the episode already open (the case below).
	case danger && hasOpen:
		// The episode was opened by a PRIOR tick, so the two-evaluation debounce is
		// satisfied: notify every admin exactly once for THIS episode.
		r.notifyAdmins(ctx, open.ID, doc)
	case !danger && hasOpen:
		if err := r.store.CloseHealthEpisode(ctx, store.CloseHealthEpisodeParams{
			ID:       open.ID,
			ClosedAt: pgconv.Time(r.now()),
		}); err != nil {
			r.logger.Error("health episode: close", "episode", open.ID.String(), "error", err)
		}
	}
}

// notifyAdmins fans the one-per-admin Danger notice out for the already-open episode. It is
// GATED on the health-notification enablement (D10, no new enable flag) — a disabled or
// unreadable setting suppresses the notice entirely — and it dedups per admin with the atomic
// ClaimHealthEpisodeNotice claim: only a claim that INSERTED (rows-affected 1) proceeds to
// Notify, so a replay, a sibling replica or a later still-danger tick never double-notifies.
// Best-effort throughout: one admin's claim/notify error is logged and never aborts the rest.
func (r *EpisodeReconciler) notifyAdmins(ctx context.Context, episodeID uuid.UUID, doc Doc) {
	enabled, err := r.settings.HealthEnabled(ctx)
	if err != nil {
		r.logger.Error("health episode: read health_enabled", "error", err)
		return
	}
	if !enabled {
		return
	}

	admins, err := r.store.ListAdmins(ctx)
	if err != nil {
		r.logger.Error("health episode: list admins", "error", err)
		return
	}
	if len(admins) == 0 {
		return
	}

	// The notice body is composed ONCE from the danger checks at this moment (their
	// already-sanitized, server-authored titles/summaries from Evaluate — D7, no raw
	// untrusted text). Only the per-admin UserID differs.
	danger := dangerChecks(doc)
	base, _ := r.settings.PublicBaseURL(ctx) // empty on error ⇒ the notice simply omits the deep link

	for _, uid := range admins {
		// Atomic claim BEFORE the send: rows-affected 1 means THIS caller claimed the
		// (episode, admin) slot and must send; 0 means a prior tick/replica already claimed
		// it, so skip silently (this is the 23505/no-op-claim "already notified" case).
		claimed, err := r.store.ClaimHealthEpisodeNotice(ctx, store.ClaimHealthEpisodeNoticeParams{
			EpisodeID: episodeID,
			UserID:    uid,
		})
		if err != nil {
			r.logger.Error("health episode: claim notice", "user", uid.String(), "episode", episodeID.String(), "error", err)
			continue
		}
		if claimed == 0 {
			continue
		}
		n := buildHealthEpisodeNotification(base, uid, episodeID, danger)
		if _, err := r.notifier.Notify(ctx, n); err != nil {
			r.logger.Warn("health episode: notify", "user", uid.String(), "error", err)
		}
	}
}

// KindHealthEpisode is the notifications.kind for the per-admin Danger health-episode notice
// (PRD #1484 M6). kind is a free-text column with no CHECK, so this needs no migration (like
// notifysvc's other kinds and slacksvc.KindCustodyEpisode). The row is a write-only event-log
// entry that nothing renders as an inbox (PRD #1650 D1); the Slack DM is the delivery.
const KindHealthEpisode = "health_episode"

// healthEpisode copy is FIXED, server-authored text (D7). The title/body are cause-neutral
// fixed strings; the per-check BODY lines are built from the danger checks' ALREADY-SANITIZED
// titles/summaries (composed by Evaluate from fixed templates + closed enums + sanitized
// identifiers) — never raw free text from kube/forge/run/worker. Those sanitized identifiers
// are still not Slack-mrkdwn-escaped, so they ride in the notice BODY (which the notifier
// escapes via SlackMrkdwn), never in the Slack Facts (which it does not).
const (
	healthEpisodeTitle = "Instance health needs attention"
	healthEpisodeBody  = "One or more health checks are at danger, so uzi may not be able to run work. Open Admin > Health for the full picture and what to do."
)

// dangerCheck is one danger check reduced to the fields the notice carries: its closed-enum
// id and its already-sanitized, server-authored title and summary.
type dangerCheck struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// dangerChecks pulls the danger checks out of the evaluated document, in the registry's
// stable order. Every field is server-authored and already sanitized by Evaluate, so the
// notice interpolates nothing raw.
func dangerChecks(doc Doc) []dangerCheck {
	var out []dangerCheck
	for _, c := range doc.Checks {
		if c.Severity == sevDanger {
			out = append(out, dangerCheck{ID: c.ID, Title: c.Title, Summary: c.Summary})
		}
	}
	return out
}

// buildHealthEpisodeNotification assembles the per-admin Danger notice (PRD #1484 M6, D7/D10).
// It is PURE (no I/O) so its shape is unit-testable. The title is fixed cause-neutral text; the
// body is that fixed copy followed by the danger checks' server-authored titles/summaries. Those
// summaries interpolate owner-supplied identifiers (worker names, forge repo paths — D7) that
// safe() sanitizes (control/bidi stripped, bounded) but does NOT Slack-mrkdwn-escape, so the
// check text rides in the notice BODY — which the notifier renders injection-safely through
// SlackMrkdwn (&<>-escaping, so a hostile <url|text> cannot become a live link) — and NEVER in
// the Slack Facts slot, which the notifier scrubs but does NOT mrkdwn-escape and which the
// notifysvc contract reserves for CLOSED server values. The Slack Facts therefore carry only a
// server-computed count, matching the custody-episode notice. The event-log payload body stores
// the same text; nothing renders it. The deep link is server-built from the operator base
// URL (the /admin/health surface); an empty base yields no link, matching the custody notice.
// User-scoped (no run/review anchor) — the episode is instance-level, not per-run.
func buildHealthEpisodeNotification(baseURL string, userID, episodeID uuid.UUID, danger []dangerCheck) notifysvc.Notification {
	// Composed ONCE and shared by the event-log payload and the Slack DM. The untrusted-carrying
	// check text lives here, in the BODY, so the notifier's SlackMrkdwn render neutralizes any
	// Slack markup an owner-supplied identifier smuggled in — the Facts slot would not.
	body := healthEpisodeBody + "\n\n" + healthEpisodeCheckLines(danger)
	return notifysvc.Notification{
		UserID: userID,
		Kind:   KindHealthEpisode,
		Payload: map[string]any{
			"title":      healthEpisodeTitle,
			"body":       body,
			"episode_id": episodeID.String(),
			"checks":     danger,
		},
		Slack: &notifysvc.SlackRender{
			Emoji: "🛑",
			Title: healthEpisodeTitle,
			Body:  body,
			Link:  healthEpisodeDeepLink(baseURL),
			Facts: healthEpisodeFacts(danger),
		},
	}
}

// healthEpisodeCheckLines renders the danger checks as one line each ("Title: summary") for the
// notice body — the SINGLE slot the check text reaches: the stored event-log payload (never
// rendered) and the Slack DM body (the notifier's SlackMrkdwn render &<>-escapes it, so
// the interpolated identifiers cannot smuggle a live <url|text> link). All text is server-authored
// and already safe()-sanitized.
func healthEpisodeCheckLines(danger []dangerCheck) string {
	lines := make([]string, 0, len(danger))
	for _, c := range danger {
		lines = append(lines, fmt.Sprintf("%s: %s", c.Title, c.Summary))
	}
	return strings.Join(lines, "\n")
}

// healthEpisodeFacts renders a single TRUSTED, CLOSED-VALUE Slack Fact: the count of danger
// checks. It deliberately carries NO check title/summary — those interpolate owner-supplied
// identifiers (worker names, forge repo paths — D7) and safe() does not escape Slack markup, so
// putting them here (Facts are ScrubSecrets'd but NOT mrkdwn-escaped by the notifier) would let a
// hostile <url|text> render as a live phishing link in an admin's DM. That text rides in the
// notice BODY instead, where the notifier's SlackMrkdwn render neutralizes it. The count is a
// server-computed int, so its `*bold*` chip is intended and injection-free — Facts must be CLOSED
// per notifysvc's contract, matching custody_episode's counts-only Facts. len(danger) >= 1 in
// practice: the reconciler reaches here only when the overall status is danger.
func healthEpisodeFacts(danger []dangerCheck) []string {
	noun := "checks"
	if len(danger) == 1 {
		noun = "check"
	}
	return []string{fmt.Sprintf("*%d* danger %s", len(danger), noun)}
}

// healthEpisodeDeepLink builds the Slack DM deep link to the Admin > Health surface from the
// operator-set public base URL. An empty base (unset, or the settings lookup failed) yields
// "" so the notice simply carries no link — the same empty-base behaviour as the custody notice.
func healthEpisodeDeepLink(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/admin/health"
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure (SQLSTATE
// 23505), the signal that the health_episodes partial unique index rejected a second
// concurrent open. It mirrors the handler package's helper of the same name; healthsvc is a
// leaf that handler imports, so it cannot borrow that one without an import cycle.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
