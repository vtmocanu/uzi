package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for the PRD #589 M2 default-schedule queries. A default-origin row
// stores only its editable fields plus catalog_slug; its prompt/labels/guidance live in
// the builtin catalog and are resolved in Go at fire time, so they persist NULL. Idempotent
// enable rides the partial unique index uq_run_schedules_default_per_repo, whose behaviour
// only a real Postgres answers — hence a live-DB test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

func tsFuture() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
}

func TestCreateDefaultScheduleLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	s, err := q.CreateDefaultSchedule(ctx, store.CreateDefaultScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		CatalogSlug: pgtype.Text{String: "docs-hygiene", Valid: true},
		CronExpr:    pgtype.Text{String: "0 8 * * 1", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: true,
		WaitOnLimit: true,
	})
	if err != nil {
		t.Fatalf("CreateDefaultSchedule: %v", err)
	}
	if s.Origin != "default" {
		t.Fatalf("origin = %q, want default", s.Origin)
	}
	if s.Prompt.Valid {
		t.Fatalf("prompt = %q, want NULL (catalog-owned, resolved at fire time)", s.Prompt.String)
	}
	if s.Labels != nil {
		t.Fatalf("labels = %v, want NULL", s.Labels)
	}
	if s.Customized {
		t.Fatal("customized = true on first enable, want false")
	}
	if !s.Enabled {
		t.Fatal("enabled = false, want true on enable")
	}
	if s.CatalogSlug.String != "docs-hygiene" {
		t.Fatalf("catalog_slug = %q, want docs-hygiene", s.CatalogSlug.String)
	}
}

func TestCreateDefaultScheduleIdempotentLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	params := store.CreateDefaultScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "sweep",
		CatalogSlug: pgtype.Text{String: "bug-triage", Valid: true},
		CronExpr:    pgtype.Text{String: "0 2 * * *", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: true,
		WaitOnLimit: true,
		MaxIssues:   pgtype.Int4{Int32: 3, Valid: true},
	}
	first, err := q.CreateDefaultSchedule(ctx, params)
	if err != nil {
		t.Fatalf("first enable: %v", err)
	}

	// A second enable of the same (user, repo, slug) inserts nothing: ON CONFLICT DO NOTHING
	// returns no row (pgx.ErrNoRows on a :one), and the existing row is fetched instead.
	_, err = q.CreateDefaultSchedule(ctx, params)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second enable error = %v, want pgx.ErrNoRows (DO NOTHING)", err)
	}
	existing, err := q.GetDefaultScheduleForRepoSlug(ctx, store.GetDefaultScheduleForRepoSlugParams{
		UserID:      userID,
		RepoID:      repoID,
		CatalogSlug: pgtype.Text{String: "bug-triage", Valid: true},
	})
	if err != nil {
		t.Fatalf("GetDefaultScheduleForRepoSlug: %v", err)
	}
	if existing.ID != first.ID {
		t.Fatalf("idempotent enable returned a different row: %s vs %s", existing.ID, first.ID)
	}

	// Exactly one default row exists for this (user, repo, slug).
	defaults, err := q.ListEnabledDefaultsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListEnabledDefaultsForUser: %v", err)
	}
	if len(defaults) != 1 {
		t.Fatalf("default rows for user = %d, want 1 (idempotent)", len(defaults))
	}
}

func TestResetDefaultScheduleLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	s, err := q.CreateDefaultSchedule(ctx, store.CreateDefaultScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		CatalogSlug: pgtype.Text{String: "docs-hygiene", Valid: true},
		CronExpr:    pgtype.Text{String: "0 8 * * 1", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: true,
		WaitOnLimit: true,
	})
	if err != nil {
		t.Fatalf("CreateDefaultSchedule: %v", err)
	}

	// Diverge the editable fields and mark it customized via the edit path.
	edited, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
		Target:      "prompt",
		RepoID:      repoID,
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "30 9 * * 2", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: false,
		WaitOnLimit: false,
		Model:       pgtype.Text{String: "fable", Valid: true},
		Customized:  true,
		ID:          s.ID,
		UserID:      userID,
	})
	if err != nil {
		t.Fatalf("UpdateRunSchedule: %v", err)
	}
	if !edited.Customized {
		t.Fatal("edited row customized = false, want true")
	}

	reset, err := q.ResetDefaultSchedule(ctx, store.ResetDefaultScheduleParams{
		CronExpr:    pgtype.Text{String: "0 8 * * 1", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		WaitOnLimit: true,
		NextFireAt:  tsFuture(),
		ID:          s.ID,
		UserID:      userID,
	})
	if err != nil {
		t.Fatalf("ResetDefaultSchedule: %v", err)
	}
	if reset.Customized {
		t.Fatal("reset row customized = true, want false")
	}
	if reset.CronExpr.String != "0 8 * * 1" {
		t.Fatalf("reset cron = %q, want the catalog default", reset.CronExpr.String)
	}
	if !reset.AutoApprove || !reset.WaitOnLimit {
		t.Fatalf("reset flags = auto:%v wait:%v, want both true", reset.AutoApprove, reset.WaitOnLimit)
	}
	if reset.Model.Valid {
		t.Fatalf("reset model = %q, want NULL (catalog leaves it blank)", reset.Model.String)
	}

	// ResetDefaultSchedule is gated on origin='default': a user row is never matched.
	userRow, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		Prompt:      pgtype.Text{String: "a user's own prompt", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 8 * * 1", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: true,
		WaitOnLimit: true,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("CreateRunSchedule (user row): %v", err)
	}
	_, err = q.ResetDefaultSchedule(ctx, store.ResetDefaultScheduleParams{
		CronExpr:    pgtype.Text{String: "0 8 * * 1", Valid: true},
		Timezone:    "UTC",
		AutoApprove: true,
		WaitOnLimit: true,
		NextFireAt:  tsFuture(),
		ID:          userRow.ID,
		UserID:      userID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resetting a user-origin row error = %v, want pgx.ErrNoRows (origin gate)", err)
	}
}
