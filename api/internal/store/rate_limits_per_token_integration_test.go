package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestRateLimitsPerTokenLiveDB pins PRD #104 M5's schema repoint against real
// Postgres: the gauge is now keyed per TOKEN, its composite FK ties each row to a
// (user, token) that exists, and — the property that replaces PRD #53's app-level
// D3b cleanup — deleting a token CASCADES its gauge row. None of these are visible
// to a fake store: the FK, the cascade, and the per-token upsert are live SQL.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestRateLimitsPerTokenLiveDB(t *testing.T) {
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

	owner, stranger := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, stranger} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("rl-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, isDefault bool) uuid.UUID {
		row, serr := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if serr != nil {
			t.Fatalf("insert secret %q: %v", label, serr)
		}
		return row.ID
	}
	def := mkSecret(owner, "default", true)
	console := mkSecret(owner, "console-key", false)
	strangerTok := mkSecret(stranger, "default", true)

	upsert := func(secretID, userID uuid.UUID, pct int16) {
		if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
			UserSecretID: secretID,
			UserID:       userID,
			FiveHourPct:  pgtype.Int2{Int16: pct, Valid: true},
			SevenDayPct:  pgtype.Int2{Int16: pct, Valid: true},
			Source:       pgtype.Text{String: "usage_endpoint", Valid: true},
			SyncedAt:     pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); uerr != nil {
			t.Fatalf("upsert %s: %v", secretID, uerr)
		}
	}

	// --- Two of the owner's tokens carry independent readings ---
	upsert(def, owner, 11)
	upsert(console, owner, 22)
	upsert(strangerTok, stranger, 99)

	rows, err := q.ListRateLimitsForUser(ctx, owner)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("owner has %d meter rows, want 2 (one per token)", len(rows))
	}
	// Default first (the query orders is_default DESC).
	if rows[0].Label != "default" || !rows[0].IsDefault || rows[0].FiveHourPct.Int16 != 11 {
		t.Fatalf("first row = %+v, want the default reading 11", rows[0])
	}
	if rows[1].Label != "console-key" || rows[1].FiveHourPct.Int16 != 22 {
		t.Fatalf("second row = %+v, want console-key reading 22", rows[1])
	}

	// --- The composite FK refuses a gauge row whose (user, token) pair is wrong ---
	//
	// A token with NO existing gauge row is required to exercise this: the FK is
	// checked on the INSERT path, and ON CONFLICT (user_secret_id) DO UPDATE
	// deliberately does not touch user_id, so upserting over an EXISTING row updates
	// only the reading and never re-checks ownership (which is itself correct — the
	// poller always passes the matching pair from one listing row). `spare` is
	// owner's, never given a reading, and here mis-claimed as stranger's.
	spare := mkSecret(owner, "spare", false)
	if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: spare, UserID: stranger, // wrong owner: no (stranger, spare) pair exists
		FiveHourPct: pgtype.Int2{Int16: 1, Valid: true}, SevenDayPct: pgtype.Int2{Int16: 1, Valid: true},
		Source: pgtype.Text{String: "usage_endpoint", Valid: true}, SyncedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); uerr == nil {
		t.Fatal("a gauge row with a mismatched (user_id, user_secret_id) pair was accepted — the composite FK is not enforcing")
	}

	// --- Deleting a token CASCADES its gauge row (replaces PRD #53 D3b cleanup) ---
	if _, derr := q.DeleteUserSecret(ctx, store.DeleteUserSecretParams{ID: console, UserID: owner}); derr != nil {
		t.Fatalf("delete console token: %v", derr)
	}
	var gauge int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM anthropic_rate_limits WHERE user_secret_id = $1`, console).Scan(&gauge); err != nil {
		t.Fatalf("count gauge after token delete: %v", err)
	}
	if gauge != 0 {
		t.Fatalf("the deleted token still has %d gauge rows — ON DELETE CASCADE did not fire", gauge)
	}
	// The sibling's gauge is untouched. Owner now holds `default` (reading 11) and
	// `spare` (created for the FK check above, no reading); console is gone.
	after, err := q.ListRateLimitsForUser(ctx, owner)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after deleting console, owner has %d meters, want 2 (default + spare)", len(after))
	}
	if after[0].Label != "default" || after[0].FiveHourPct.Int16 != 11 {
		t.Fatalf("default meter changed: %+v, want reading 11 intact", after[0])
	}
	if after[1].Label != "spare" || after[1].SyncedAt.Valid {
		t.Fatalf("spare meter = %+v, want present with no reading (unavailable)", after[1])
	}

	// --- The admin listing folds correctly: one row per (user, token), plus a row
	// per token-less user ---
	third := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		third, "rl-notoken@e2e")

	adminRows, err := q.ListRateLimits(ctx)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	tokenlessSeen := false
	ownerTokens := 0
	for _, r := range adminRows {
		switch r.UserID {
		case third:
			if r.UserSecretID.Valid {
				t.Fatal("a token-less user must appear with a NULL user_secret_id")
			}
			tokenlessSeen = true
		case owner:
			if r.UserSecretID.Valid {
				ownerTokens++
			}
		}
	}
	if !tokenlessSeen {
		t.Fatal("the token-less user is missing from the admin listing")
	}
	// owner holds `default` (with a reading) and `spare` (no reading) after console
	// was deleted — both appear, since the listing is driven from user_secrets and
	// a reading-less token is `unavailable`, not absent.
	if ownerTokens != 2 {
		t.Fatalf("owner appears with %d token rows in the admin listing, want 2 (default + spare; console deleted)", ownerTokens)
	}
}
