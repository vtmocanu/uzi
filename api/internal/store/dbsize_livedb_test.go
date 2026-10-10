package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestDatabaseSizeStatusLiveDB proves the db.size probe SQL against a real Postgres: a
// positive size and at most three non-empty relation names, largest first.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).
func TestDatabaseSizeStatusLiveDB(t *testing.T) {
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

	got, err := store.DatabaseSizeStatus(ctx, pool)
	if err != nil {
		t.Fatalf("DatabaseSizeStatus: %v", err)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", got.SizeBytes)
	}
	if n := len(got.Largest); n == 0 || n > 3 {
		t.Fatalf("len(Largest) = %d, want 1..3", n)
	}
	for i, r := range got.Largest {
		if r.Name == "" {
			t.Errorf("Largest[%d] has an empty name", i)
		}
		if i > 0 && r.SizeBytes > got.Largest[i-1].SizeBytes {
			t.Errorf("Largest not descending: [%d]=%d > [%d]=%d", i, r.SizeBytes, i-1, got.Largest[i-1].SizeBytes)
		}
	}
}
