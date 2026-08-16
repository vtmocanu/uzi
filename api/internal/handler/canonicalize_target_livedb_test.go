package handler

import (
	"context"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCanonicalizeTargetMatchesBackfillSQLLiveDB pins the ONE property the whole issue-#232
// dedup rests on: the Go ingest folder (handler.canonicalizeTarget) and the 00097 backfill's
// SQL expression produce the SAME canonical coordinate for the SAME input. If they ever
// diverged, a row ingested today and the historical row the migration folded would land on
// DIFFERENT (category, target) keys and the dedup would silently split them again — exactly
// the bug this issue closes, reintroduced invisibly.
//
// It lives in the handler package because canonicalizeTarget is unexported here; it needs a
// real Postgres only to evaluate lower(btrim(regexp_replace(... COLLATE "C", ...))), so it
// opens a pool and compares, fixture by fixture, rather than persisting anything.
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

	// Both sides are ASCII-only and byte-deterministic: Go via asciiLowerTarget + RE2's
	// ASCII-only POSIX classes + an ASCII-space trim; SQL via COLLATE "C" on the
	// regexp_replace input, which forces the POSIX classes, btrim and lower to ASCII-only.
	// So the two must agree byte-for-byte on ASCII AND on non-ASCII (which both leave
	// untouched), regardless of the production DB's UTF-8 locale.
	fixtures := []string{
		// ASCII fixtures — the coordinate identifiers targets are in practice.
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
		// Non-ASCII fixtures — the whole point of the COLLATE "C" fix (issue #232 review). In
		// the production UTF-8 locale a default lower()/POSIX-class fold would fold these
		// Unicode code points (curly apostrophe, em-dash, NBSP, a decomposed accent, Unicode
		// case) while Go leaves them intact, DE-LINKING a re-judged recurrence from its
		// backfilled history. With COLLATE "C" (SQL) and asciiLowerTarget (Go), BOTH sides
		// leave every non-ASCII byte untouched and fold only the ASCII around it — so Go==SQL.
		// The decomposed accent below is a literal "e" followed by U+0301 (NOT a precomposed
		// U+00E9); gofmt does not NFC-normalize string contents, so it stays decomposed.
		"worker’s git identity", // U+2019 curly apostrophe — kept, NOT folded to a space
		"api—internal—forge",    // U+2014 EM DASH between words — kept, NOT folded
		" worker git ",          // U+00A0 NBSP at both edges AND interior — kept, NOT trimmed/folded
		"éclair review",        // decomposed "é" (e + U+0301) — NOT NFC-composed
		"WÜRK identity",         // uppercase U+00DC "Ü" — ASCII W/R/K lowered, Ü kept as-is
	}
	for _, in := range fixtures {
		goOut := canonicalizeTarget(in, workersvc.ReviewTargetMaxBytes)
		var sqlOut string
		if err := pool.QueryRow(ctx,
			`SELECT lower(btrim(regexp_replace($1 COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g')))`, in).Scan(&sqlOut); err != nil {
			t.Fatalf("SQL canonical for %q: %v", in, err)
		}
		if goOut != sqlOut {
			t.Errorf("Go/SQL disagree on %q: go=%q sql=%q", in, goOut, sqlOut)
		}
	}
}
