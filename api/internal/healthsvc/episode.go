package healthsvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// EpisodeReconciler opens and closes DANGER health episodes (PRD #1484 M2, D14). It is a
// STANDALONE reconciler wired from main.go beside the custody-episode reconciler, running
// the SAME registry once a minute through the SHARED Service so the endpoint and the
// episode lifecycle can never disagree about the instance's health.
//
// M2 is open/close ONLY — there is deliberately NO notice here. The debounced, claimed
// per-admin fan-out lands in M6. The episode row is what the banner's snooze keys on
// (episode_id), so the evaluator must exist before the snooze works, which is why it lands
// in M2 (D14).
//
// Its decision logic is unit-tested with fakes (this is not a live-DB test — healthsvc is
// not in the sweep-enumerated live-DB package list; the episode queries' atomicity is
// proven in the store package by M2-A).
type EpisodeReconciler struct {
	eval   healthEvaluator
	store  episodeStore
	now    func() time.Time
	logger *slog.Logger
}

// healthEvaluator is the one method the reconciler needs from the shared Service, kept as
// an interface so a fake Doc can drive the open/close decision without a database.
type healthEvaluator interface {
	Evaluate(ctx context.Context) (Doc, error)
}

// episodeStore is the episode-lifecycle slice of *store.Queries the reconciler writes: the
// open-episode read, the atomic open, and the idempotent close.
type episodeStore interface {
	GetOpenHealthEpisode(ctx context.Context) (store.GetOpenHealthEpisodeRow, error)
	OpenHealthEpisode(ctx context.Context, openedAt pgtype.Timestamptz) (uuid.UUID, error)
	CloseHealthEpisode(ctx context.Context, arg store.CloseHealthEpisodeParams) error
}

// NewEpisodeReconciler builds an EpisodeReconciler. eval is the shared Service, st is
// *store.Queries. A nil logger defaults to slog.Default().
func NewEpisodeReconciler(eval healthEvaluator, st episodeStore, logger *slog.Logger) *EpisodeReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &EpisodeReconciler{eval: eval, store: st, now: time.Now, logger: logger}
}

// Reconcile runs one evaluation and moves the episode lifecycle by exactly one step:
//   - overall danger AND no episode open  -> open one (a 23505 unique violation means
//     another replica opened first, which is already-open, not an error).
//   - overall not-danger AND an episode open -> close it (the re-arm).
//
// Everything else is a no-op. Best-effort: every error is logged, never returned.
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
	case !danger && hasOpen:
		if err := r.store.CloseHealthEpisode(ctx, store.CloseHealthEpisodeParams{
			ID:       open.ID,
			ClosedAt: pgconv.Time(r.now()),
		}); err != nil {
			r.logger.Error("health episode: close", "episode", open.ID.String(), "error", err)
		}
	}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure (SQLSTATE
// 23505), the signal that the health_episodes partial unique index rejected a second
// concurrent open. It mirrors the handler package's helper of the same name; healthsvc is a
// leaf that handler imports, so it cannot borrow that one without an import cycle.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
