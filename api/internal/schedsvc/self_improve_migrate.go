// This file boot-migrates a legacy, engine-configured self-improvement install
// (PRD #46) onto the PRD #590 self_improve default-schedule row, then retires the
// legacy app_settings keys. It runs ONCE per boot from main.go, AFTER goose
// migrations have already applied (store.Migrate precedes it), so the schema is
// current; and it reads the legacy keys as RAW app_settings rows via ListAppSettings,
// never through the typed settings.KeySelfimprove* getters — those consts and their
// Cache accessors are deleted in PRD #590 M2, so schedsvc must not depend on them.
// The legacy key strings are therefore owned locally here.
//
// Idempotency + self-heal. The migration is safe to run on every boot:
//   - A disabled or already-migrated install has no selfimprove_enabled="true" row
//     (it was either never enabled, or the keys were deleted by a prior run), so
//     step 2 returns nil and nothing is seeded.
//   - CreateDefaultSchedule carries ON CONFLICT (user,repo,catalog_slug) DO NOTHING,
//     so a pre-existing self_improve schedule for the owner+repo is respected (its
//     enabled/disabled state wins) rather than overwritten.
//   - A crash BETWEEN the schedule insert and the key delete self-heals: the next
//     boot re-reads the still-present keys, the ON CONFLICT keeps the row already
//     created, and the delete finally lands. The two writes share ONE transaction,
//     so a committed run always has BOTH (row present, keys gone) or NEITHER.
//
// Failure tolerance. main.go treats a returned error as non-fatal (log + continue),
// but a genuine skip (disabled, unconfigured owner/repo, disconnected repo) returns
// nil so it is not logged as an error. Only a real DB/forge failure returns non-nil.
package schedsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Legacy self-improvement app_settings keys (PRD #46), owned locally so this
// migration does not depend on the settings.KeySelfimprove* consts that PRD #590 M2
// deletes. Read raw from app_settings; never through a typed getter.
const (
	legacySIEnabled   = "selfimprove_enabled"
	legacySIInterval  = "selfimprove_interval"
	legacySIRepo      = "selfimprove_repo"
	legacySIUserID    = "selfimprove_user_id"
	legacySILastRunAt = "selfimprove_last_run_at"
)

// MigrateLegacySelfImprove materializes a self_improve default-schedule row from a
// legacy enabled self-improvement install and retires the legacy keys, in one
// transaction. It is idempotent and self-healing (see the package doc). A skip
// (disabled/already-migrated, unconfigured owner/repo, or a disconnected/unowned
// repo) returns nil; only a genuine DB/forge error returns non-nil, and main.go
// treats even that as non-fatal.
func MigrateLegacySelfImprove(ctx context.Context, pool *pgxpool.Pool, now time.Time, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	q := store.New(pool)

	rows, err := q.ListAppSettings(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}

	// Disabled or already-migrated: the keys are inert now that they are out of
	// settings.Defaults, so leave them untouched and seed nothing.
	if m[legacySIEnabled] != "true" {
		return nil
	}

	repoID, rerr := uuid.Parse(strings.TrimSpace(m[legacySIRepo]))
	userID, uerr := uuid.Parse(strings.TrimSpace(m[legacySIUserID]))
	if rerr != nil || uerr != nil {
		// Enabled but never fully configured. There is no engine any more, so this is
		// best-effort — leave the keys for a possible manual cleanup, do not fail boot.
		logger.Warn("selfimprove migration: enabled but repo/owner unconfigured")
		return nil
	}

	// The repo must still be connected AND owned by the recorded owner. A disconnected
	// or reassigned repo skips (the engine is gone, so there is nothing to retry) but
	// leaves the keys so a later reconnect can be handled by hand.
	if _, err := q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("selfimprove migration: repo disconnected or not owned, skipping", "repo", repoID)
			return nil
		}
		return err
	}

	job, _ := schedtmpl.BySlug("self-improve")
	catalogCron := job.Cron
	cron := selfImproveCronFromInterval(m[legacySIInterval], catalogCron)

	// The next FUTURE occurrence — deliberately NOT derived from selfimprove_last_run_at,
	// so cutover never triggers an immediate off-cadence fire.
	nextFireAt, err := NextFire(cron, schedtmpl.DefaultTimezone, now)
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := q.WithTx(tx)

	// Model + MaxIssues left NULL: the self-improve catalog carries no model or
	// max_issues. ON CONFLICT DO NOTHING returns pgx.ErrNoRows when a self_improve
	// schedule already exists for this owner+repo — benign, that row's state wins.
	if _, err := qtx.CreateDefaultSchedule(ctx, store.CreateDefaultScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "self_improve",
		CatalogSlug: pgtype.Text{String: "self-improve", Valid: true},
		CronExpr:    pgtype.Text{String: cron, Valid: true},
		Timezone:    schedtmpl.DefaultTimezone,
		NextFireAt:  pgtype.Timestamptz{Time: nextFireAt, Valid: true},
		AutoApprove: schedtmpl.AutoApprove,
		WaitOnLimit: schedtmpl.WaitOnLimit,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if _, err := qtx.DeleteAppSettings(ctx, []string{
		legacySIEnabled, legacySIInterval, legacySIRepo, legacySIUserID, legacySILastRunAt,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	logger.Info("selfimprove migration: materialized self_improve schedule and retired legacy keys", "repo", repoID, "user", userID)
	return nil
}

// selfImproveCronFromInterval maps a legacy selfimprove_interval Go duration onto a
// cron expression: a whole-day multiple 1..31 days becomes "0 4 */N * *" (04:00 every
// N days), and anything else (a sub-day/odd interval, a value >31 days, an unparseable
// or non-positive string) falls back to the catalog default cron.
func selfImproveCronFromInterval(interval, fallback string) string {
	d, err := time.ParseDuration(strings.TrimSpace(interval))
	if err != nil || d <= 0 {
		return fallback
	}
	if d%(24*time.Hour) == 0 {
		n := int(d / (24 * time.Hour))
		if n >= 1 && n <= 31 {
			return fmt.Sprintf("0 4 */%d * *", n)
		}
	}
	return fallback
}
