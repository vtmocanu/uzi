package handler

import (
	"context"
	"os"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCanonicalizeTargetMatchesBackfillSQLLiveDB pins the ONE property the whole issue-#232
// dedup rests on: the Go ingest folder (handler.canonicalizeTarget) and the 00097 backfill's
// SQL expression produce the SAME canonical coordinate for the SAME input. If they ever
// diverged, a row ingested today and the historical row the migration folded would land on
// DIFFERENT (category, target) keys and the dedup would silently split them again — exactly
// the bug this issue closes, reintroduced invisibly.
//
// It lives in the handler package because canonicalizeTarget is unexported here; it needs a
// real Postgres only to evaluate lower(btrim(regexp_replace(...))), so it opens a pool and
// compares, fixture by fixture, rather than persisting anything.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestCanonicalizeTargetMatchesBackfillSQLLiveDB(t *testing.T) {
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

	// ASCII fixtures — the coordinate identifiers targets are in practice. The Go func adds a
	// leading NFC pass the SQL omits; on ASCII that is a no-op, so the two must agree exactly.
	fixtures := []string{
		"Worker Git-Identity Setup ",
		"worker  git-identity   setup",
		"worker clone setup (git identity)",
		"worker runner clone setup",
		"api/internal/forge/gitlab.go",
		"UPPER.snake_case--dashes",
		"ShellCheck",
		"  --foo bar--  ",
		"trailing punctuation!!!",
		"",
		"   ",
		"multi   space\tand\ttabs",
	}
	for _, in := range fixtures {
		goOut := canonicalizeTarget(in, workersvc.ReviewTargetMaxBytes)
		var sqlOut string
		if err := pool.QueryRow(ctx,
			`SELECT lower(btrim(regexp_replace($1, '[[:space:][:punct:]]+', ' ', 'g')))`, in).Scan(&sqlOut); err != nil {
			t.Fatalf("SQL canonical for %q: %v", in, err)
		}
		if goOut != sqlOut {
			t.Errorf("Go/SQL disagree on %q: go=%q sql=%q", in, goOut, sqlOut)
		}
	}
}
