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

// TestUserSummaryModelLiveDB executes the queries migrations 00132 and 00406 add columns
// for — GetUserSummaryModels, SetUserSummaryModel (PRD #362 M2) and SetUserCodexSummaryModel (migration 00406, PRD #3248) — against real
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
	both, err := q.GetUserSummaryModels(ctx, user)
	if err != nil {
		t.Fatalf("get summary models (pristine row): %v", err)
	}
	if both.SummaryModel.Valid || both.CodexSummaryModel.Valid {
		t.Fatalf("pristine summary models = %+v, want both NULL/invalid (inherit)", both)
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
	both, err = q.GetUserSummaryModels(ctx, user)
	if err != nil {
		t.Fatalf("get summary models (populated): %v", err)
	}
	if !both.SummaryModel.Valid || both.SummaryModel.String != "opus" || both.CodexSummaryModel.Valid {
		t.Fatalf("read-back summary models = %+v, want claude=opus, codex NULL", both)
	}

	// The Codex lane round-trips independently of the Claude lane.
	codexRet, err := q.SetUserCodexSummaryModel(ctx, store.SetUserCodexSummaryModelParams{
		ID:                user,
		CodexSummaryModel: pgtype.Text{String: "gpt-6-astra", Valid: true},
	})
	if err != nil {
		t.Fatalf("set codex summary model: %v", err)
	}
	if !codexRet.Valid || codexRet.String != "gpt-6-astra" {
		t.Fatalf("RETURNING codex_summary_model = %+v, want gpt-6-astra", codexRet)
	}
	both, err = q.GetUserSummaryModels(ctx, user)
	if err != nil {
		t.Fatalf("get summary models (both set): %v", err)
	}
	if both.SummaryModel.String != "opus" || both.CodexSummaryModel.String != "gpt-6-astra" {
		t.Fatalf("read-back summary models = %+v, want opus / gpt-6-astra", both)
	}
	codexRet, err = q.SetUserCodexSummaryModel(ctx, store.SetUserCodexSummaryModelParams{
		ID:                user,
		CodexSummaryModel: pgtype.Text{Valid: false},
	})
	if err != nil {
		t.Fatalf("clear codex summary model: %v", err)
	}
	if codexRet.Valid {
		t.Fatalf("RETURNING after codex clear = %+v, want NULL/invalid", codexRet)
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
	both, err = q.GetUserSummaryModels(ctx, user)
	if err != nil {
		t.Fatalf("get summary models (cleared): %v", err)
	}
	if both.SummaryModel.Valid || both.CodexSummaryModel.Valid {
		t.Fatalf("read-back after clear = %+v, want both NULL/invalid", both)
	}
}
