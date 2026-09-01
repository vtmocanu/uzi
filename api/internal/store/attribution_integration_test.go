package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAttributionEnabledLiveDB exercises the issue #916 per-user AI-attribution
// opt-out against a REAL Postgres: a freshly created user defaults to
// attribution_enabled=true (opt-out, so today's behavior is preserved), and
// SetUserAttributionEnabled flips it off and back on, persisting and returning the
// updated row each time.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAttributionEnabledLiveDB(t *testing.T) {
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

	uid := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		uid, fmt.Sprintf("attr-%s@e2e", uid))

	// Default: a user who never touched the setting has attribution ON.
	fresh, err := q.GetUserByID(ctx, uid)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !fresh.AttributionEnabled {
		t.Fatalf("a fresh user must default attribution_enabled=true, got false")
	}

	// Opt out → false persists and is returned.
	off, err := q.SetUserAttributionEnabled(ctx, store.SetUserAttributionEnabledParams{ID: uid, AttributionEnabled: false})
	if err != nil {
		t.Fatalf("SetUserAttributionEnabled(false): %v", err)
	}
	if off.AttributionEnabled {
		t.Fatalf("returned row must carry attribution_enabled=false, got true")
	}
	if reread, err := q.GetUserByID(ctx, uid); err != nil || reread.AttributionEnabled {
		t.Fatalf("opt-out did not persist: got enabled=%v err=%v", reread.AttributionEnabled, err)
	}

	// Opt back in → true persists and is returned.
	on, err := q.SetUserAttributionEnabled(ctx, store.SetUserAttributionEnabledParams{ID: uid, AttributionEnabled: true})
	if err != nil {
		t.Fatalf("SetUserAttributionEnabled(true): %v", err)
	}
	if !on.AttributionEnabled {
		t.Fatalf("returned row must carry attribution_enabled=true, got false")
	}
	if reread, err := q.GetUserByID(ctx, uid); err != nil || !reread.AttributionEnabled {
		t.Fatalf("opt-in did not persist: got enabled=%v err=%v", reread.AttributionEnabled, err)
	}
}
