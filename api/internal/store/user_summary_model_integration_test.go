package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserSummaryModelLiveDB executes the two queries migration 00132 adds a column
// for — GetUserSummaryModel and SetUserSummaryModel (PRD #362 M2) — against real
// Postgres. Per .claude/rules/go.md a green `sqlc generate` is not evidence a query
// runs (sqlc's type deduction is not Postgres's), so the nullable-text round-trip
// (pristine NULL, a set value, and clearing back to NULL) is only settled by the
// server.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserSummaryModelLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user, fmt.Sprintf("summary-model-%s@e2e", user))

	// A never-written column is NULL, which must read as invalid (inherit the
	// instance summary_model), not an error — the no-backfill contract in 00132.
	got, err := q.GetUserSummaryModel(ctx, user)
	if err != nil {
		t.Fatalf("get summary model (pristine row): %v", err)
	}
	if got.Valid {
		t.Fatalf("pristine summary_model = %+v, want NULL/invalid (inherit)", got)
	}

	// A set value round-trips through the RETURNING clause and a fresh read.
	ret, err := q.SetUserSummaryModel(ctx, store.SetUserSummaryModelParams{
		ID:           user,
		SummaryModel: pgtype.Text{String: "opus", Valid: true},
	})
	if err != nil {
		t.Fatalf("set summary model: %v", err)
	}
	if !ret.Valid || ret.String != "opus" {
		t.Fatalf("RETURNING summary_model = %+v, want opus", ret)
	}
	got, err = q.GetUserSummaryModel(ctx, user)
	if err != nil {
		t.Fatalf("get summary model (populated): %v", err)
	}
	if !got.Valid || got.String != "opus" {
		t.Fatalf("read-back summary_model = %+v, want opus", got)
	}

	// Clearing with a NULL is the "back to inherit the instance model" write.
	ret, err = q.SetUserSummaryModel(ctx, store.SetUserSummaryModelParams{
		ID:           user,
		SummaryModel: pgtype.Text{Valid: false},
	})
	if err != nil {
		t.Fatalf("clear summary model: %v", err)
	}
	if ret.Valid {
		t.Fatalf("RETURNING after clear = %+v, want NULL/invalid", ret)
	}
	got, err = q.GetUserSummaryModel(ctx, user)
	if err != nil {
		t.Fatalf("get summary model (cleared): %v", err)
	}
	if got.Valid {
		t.Fatalf("read-back after clear = %+v, want NULL/invalid", got)
	}
}
