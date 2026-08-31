package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests pin issue #804 (PRD #111 D2) at the SQL layer against a REAL Postgres:
//
//   - InsertUserSecret / UpsertDefaultUserSecret born-auto-eligible ONLY for a user's
//     first/sole anthropic_token, so a single-token owner has a non-empty auto-select
//     pool and their auto-mode ephemeral workers never park in pool_wait, while token
//     #2+ (the reserved-console-key hazard) stays opt-in false.
//   - Rotation via UpsertDefaultUserSecret PRESERVES the existing opt-in/opt-out state.
//   - CreateEphemeralHostedWorker persists the caller-supplied anthropic_bind_mode.
//   - UserHasAutoEligibleAnthropicToken — the bit the ephemeral provisioner reads to
//     choose `auto` vs `default`.
//   - The 00182 backfill WHERE-clause: a user's SOLE token becomes eligible, a
//     multi-token user's tokens are never touched.
//
// A fake store cannot exhibit any of these — the first-token subquery, the CHECK, and
// the set-based backfill are all real-SQL behaviour. Every test is skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh provides
// one and sweeps this package for the LiveDB suffix).

// autoEligPool opens a migrated pool or skips when no throwaway DB is configured.
func autoEligPool(t *testing.T) (context.Context, *store.Queries, *pgxpool.Pool) {
	t.Helper()
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
	t.Cleanup(pool.Close)
	return ctx, store.New(pool), pool
}

// seedBareUser inserts a bare user and returns its id.
func seedBareUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		id, fmt.Sprintf("autoelig-%s@e2e", id))
	return id
}

// TestInsertUserSecretFirstTokenAutoEligibleLiveDB: the first anthropic_token a user
// stores is born auto_eligible=true; the second is false.
func TestInsertUserSecretFirstTokenAutoEligibleLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)
	user := seedBareUser(ctx, t, pool)

	first, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: "subscription", WantDefault: true,
		Ciphertext: []byte("ct1"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if !first.AutoEligible {
		t.Errorf("first anthropic_token auto_eligible = false, want true (a sole-token owner needs a non-empty auto-select pool)")
	}

	// With one eligible token, the provisioner's read must report a non-empty pool.
	has, err := q.UserHasAutoEligibleAnthropicToken(ctx, user)
	if err != nil {
		t.Fatalf("UserHasAutoEligibleAnthropicToken: %v", err)
	}
	if !has {
		t.Errorf("UserHasAutoEligibleAnthropicToken = false after a born-eligible first token, want true")
	}

	second, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: "console-key", WantDefault: false,
		Ciphertext: []byte("ct2"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if second.AutoEligible {
		t.Errorf("second anthropic_token auto_eligible = true, want false (only the FIRST token opts in; #2 is the reserved-key hazard)")
	}
}

// TestUpsertDefaultFirstTokenAutoEligibleLiveDB: the D14 alias's INSERT branch is
// born-eligible only for a first token, and a rotation (its DO UPDATE branch) preserves
// whatever opt-in/opt-out state the token already carried.
func TestUpsertDefaultFirstTokenAutoEligibleLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)

	// (a) First-token INSERT branch: born eligible.
	userA := seedBareUser(ctx, t, pool)
	a1, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{
		UserID: userA, Kind: store.KindAnthropicToken, Ciphertext: []byte("a1"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	if !a1.AutoEligible {
		t.Errorf("UpsertDefaultUserSecret first-token auto_eligible = false, want true")
	}

	// (b) Rotation preserves an OPTED-OUT token as false. Insert a first token (born
	// true), flip it off, then rotate via Upsert (hits DO UPDATE) — it must stay false.
	userB := seedBareUser(ctx, t, pool)
	b1, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: userB, Kind: store.KindAnthropicToken, Label: "default", WantDefault: true,
		Ciphertext: []byte("b1"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert userB token: %v", err)
	}
	if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
		ID: b1.ID, UserID: userB, Kind: store.KindAnthropicToken, AutoEligible: false,
	}); err != nil {
		t.Fatalf("opt userB token out: %v", err)
	}
	bRot, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{
		UserID: userB, Kind: store.KindAnthropicToken, Ciphertext: []byte("b1-rot"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("rotate userB: %v", err)
	}
	if bRot.ID != b1.ID {
		t.Fatalf("rotation created a new row (%s) instead of updating the default (%s)", bRot.ID, b1.ID)
	}
	if bRot.AutoEligible {
		t.Errorf("rotation flipped an opted-OUT token back to eligible; auto_eligible = true, want false (rotation must preserve state)")
	}

	// (c) Rotation preserves an OPTED-IN token as true.
	userC := seedBareUser(ctx, t, pool)
	c1, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: userC, Kind: store.KindAnthropicToken, Label: "default", WantDefault: true,
		Ciphertext: []byte("c1"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert userC token: %v", err)
	}
	if !c1.AutoEligible {
		t.Fatalf("precondition: userC first token not born eligible")
	}
	cRot, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{
		UserID: userC, Kind: store.KindAnthropicToken, Ciphertext: []byte("c1-rot"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("rotate userC: %v", err)
	}
	if !cRot.AutoEligible {
		t.Errorf("rotation cleared an opted-IN token; auto_eligible = false, want true (rotation must preserve state)")
	}
}

// TestUserHasAutoEligibleAnthropicTokenLiveDB pins the provisioner's decision input:
// false with no token / only an opted-out token, true once a token is opted in.
func TestUserHasAutoEligibleAnthropicTokenLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)

	// No token at all → false.
	empty := seedBareUser(ctx, t, pool)
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, empty); err != nil {
		t.Fatalf("query (no token): %v", err)
	} else if has {
		t.Errorf("UserHasAutoEligibleAnthropicToken = true for a token-less user, want false")
	}

	// A single opted-OUT token → false. Insert (born true), then opt out.
	user := seedBareUser(ctx, t, pool)
	tok, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: "default", WantDefault: true,
		Ciphertext: []byte("ct"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
		ID: tok.ID, UserID: user, Kind: store.KindAnthropicToken, AutoEligible: false,
	}); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, user); err != nil {
		t.Fatalf("query (opted out): %v", err)
	} else if has {
		t.Errorf("UserHasAutoEligibleAnthropicToken = true with only an opted-out token, want false")
	}

	// Opt it back in → true.
	if _, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
		ID: tok.ID, UserID: user, Kind: store.KindAnthropicToken, AutoEligible: true,
	}); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, user); err != nil {
		t.Fatalf("query (opted in): %v", err)
	} else if !has {
		t.Errorf("UserHasAutoEligibleAnthropicToken = false with an opted-in token, want true")
	}
}

// TestCreateEphemeralHostedWorkerPersistsBindModeLiveDB proves the caller-supplied
// anthropic_bind_mode is written (issue #804): create with 'auto', read the column back.
func TestCreateEphemeralHostedWorkerPersistsBindModeLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	runID := fx.queuedRun()

	wkr, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID:            fx.userID,
		Name:              "ephemeral-" + runID.String(),
		TokenHash:         tokenHash(),
		TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
		HostedSize:        pgtype.Text{String: "m", Valid: true},
		DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
		EphemeralRunID:    runID,
		AnthropicBindMode: "auto",
	})
	if err != nil {
		t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}
	if wkr.AnthropicBindMode != "auto" {
		t.Errorf("returned worker anthropic_bind_mode = %q, want \"auto\"", wkr.AnthropicBindMode)
	}
	var got string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT anthropic_bind_mode FROM workers WHERE id = $1`, wkr.ID).Scan(&got); err != nil {
		t.Fatalf("read back anthropic_bind_mode: %v", err)
	}
	if got != "auto" {
		t.Errorf("persisted anthropic_bind_mode = %q, want \"auto\" (the caller-supplied mode must be stored)", got)
	}
}

// TestBackfillSoleTokenAutoEligibleWhereClauseLiveDB reproduces migration 00182's exact
// backfill WHERE-clause on seeded pre-migration-state rows (auto_eligible=false inserted
// directly), which is the accepted alternative to asserting the migration itself — by the
// time a test runs, 00182 has already applied to an empty table. A user's SOLE token is
// made eligible; neither token of a two-token user is touched.
func TestBackfillSoleTokenAutoEligibleWhereClauseLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)

	sole := seedBareUser(ctx, t, pool)
	multi := seedBareUser(ctx, t, pool)

	// Seed rows in the PRE-backfill state (auto_eligible=false explicitly), bypassing
	// InsertUserSecret's born-eligible logic so the backfill has something to do.
	seedRaw := func(user uuid.UUID, label string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO user_secrets (id, user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with)
			 VALUES ($1, $2, $3, $4, $5, false, $6, 'master')`,
			id, user, store.KindAnthropicToken, label, label == "default", []byte("ct-"+label))
		return id
	}
	soleTok := seedRaw(sole, "default")
	multiA := seedRaw(multi, "default")
	multiB := seedRaw(multi, "console-key")

	// The 00182 Up statement, verbatim.
	mustExec(ctx, t, pool, `
UPDATE user_secrets s
SET auto_eligible = true
WHERE s.kind = 'anthropic_token'
  AND NOT s.auto_eligible
  AND NOT EXISTS (
      SELECT 1 FROM user_secrets o
      WHERE o.user_id = s.user_id AND o.kind = 'anthropic_token' AND o.id <> s.id
  )`)

	// The provisioner-facing read is the cleanest assertion of the outcome.
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, sole); err != nil {
		t.Fatalf("read sole: %v", err)
	} else if !has {
		t.Errorf("sole-token user has no eligible token after backfill, want their token pooled")
	}
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, multi); err != nil {
		t.Fatalf("read multi: %v", err)
	} else if has {
		t.Errorf("a multi-token user's token was pooled by the backfill, want none touched (reserved-key hazard)")
	}

	// Column-level check too, so the assertion does not rest solely on the EXISTS query.
	assertElig := func(id uuid.UUID, want bool) {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT auto_eligible FROM user_secrets WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read auto_eligible: %v", err)
		}
		if got != want {
			t.Errorf("secret %s auto_eligible = %v, want %v", id, got, want)
		}
	}
	assertElig(soleTok, true)
	assertElig(multiA, false)
	assertElig(multiB, false)
}
