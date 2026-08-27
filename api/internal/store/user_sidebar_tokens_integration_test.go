package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserSidebarTokensLiveDB executes the two queries migration 00123 adds a
// column for — SetUserSidebarTokens and the widened GetUserSettings — against
// real Postgres. The reason this exists at all is the standing rule in
// .claude/rules/go.md: a green `sqlc generate` is not evidence a query runs
// (sqlc's type deduction is not Postgres's), so a new query is unverified until
// a live-DB test has executed it. The uuid[] round-trip (NULL column, empty
// array, populated array) is exactly the kind of thing that only the server
// settles.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserSidebarTokensLiveDB(t *testing.T) {
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
		user, fmt.Sprintf("sidebar-%s@e2e", user))

	// A never-written column is NULL and must read as the empty (default-only)
	// choice, not as an error — this is the no-backfill contract in 00123.
	s, err := q.GetUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("get settings (pristine row): %v", err)
	}
	if len(s.SidebarTokenIds) != 0 {
		t.Fatalf("pristine sidebar_token_ids = %v, want empty", s.SidebarTokenIds)
	}

	// Populated round-trip: the RETURNING value and a fresh read must both carry
	// the set, in order.
	a, b := uuid.New(), uuid.New()
	got, err := q.SetUserSidebarTokens(ctx, store.SetUserSidebarTokensParams{
		ID:              user,
		SidebarTokenIds: []uuid.UUID{a, b},
	})
	if err != nil {
		t.Fatalf("set sidebar tokens: %v", err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("RETURNING sidebar_token_ids = %v, want [%s %s]", got, a, b)
	}
	s, err = q.GetUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("get settings (populated): %v", err)
	}
	if len(s.SidebarTokenIds) != 2 || s.SidebarTokenIds[0] != a || s.SidebarTokenIds[1] != b {
		t.Fatalf("read-back sidebar_token_ids = %v, want [%s %s]", s.SidebarTokenIds, a, b)
	}

	// Clearing with an explicit empty array is the "back to default-only" write
	// the web's uncheck-everything path produces.
	got, err = q.SetUserSidebarTokens(ctx, store.SetUserSidebarTokensParams{
		ID:              user,
		SidebarTokenIds: []uuid.UUID{},
	})
	if err != nil {
		t.Fatalf("clear sidebar tokens: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RETURNING after clear = %v, want empty", got)
	}
	s, err = q.GetUserSettings(ctx, user)
	if err != nil {
		t.Fatalf("get settings (cleared): %v", err)
	}
	if len(s.SidebarTokenIds) != 0 {
		t.Fatalf("read-back after clear = %v, want empty", s.SidebarTokenIds)
	}
}
