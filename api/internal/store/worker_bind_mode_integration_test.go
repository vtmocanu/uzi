package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestWorkerBindModeLiveDB pins migration 00088 — PRD #111 M3's third bind mode —
// against a REAL Postgres. Three properties, none of which a fake can exhibit:
//
//  1. The CHECK is a closed set. An unrecognised mode is refused by the database,
//     not only by the Go validator, which is what still holds when a future write
//     path forgets to call ValidBindMode.
//
//  2. 🔴 D9's interaction with 00078's cascade, which is the reason no CHECK
//     couples the mode to the id. Deleting a pinned worker's credential nulls its
//     anthropic_secret_id (ON DELETE SET NULL) and LEAVES the mode 'pinned' — a row
//     the database holds legitimately and which resolution must treat as default. A
//     coupling constraint would have made that DELETE fail outright, so this test is
//     equally the proof that the constraint was correctly NOT added.
//
//  3. The write is atomic over the pair: SetWorkerAnthropicSecret sets mode and id
//     in one statement, so no reader can observe 'pinned' with the previous id.
//
// The BACKFILL is asserted separately below, because it cannot be observed here:
// by the time a test runs, the migration has already applied to an empty table.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestWorkerBindModeLiveDB(t *testing.T) {
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

	owner := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("bindmode-%s@e2e", owner))
	secret, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: owner, Kind: store.KindAnthropicToken, Label: "console-key", WantDefault: true,
		Ciphertext: []byte("ct"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "alpha", TokenHash: tokenHash(),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	// A fresh worker is 'default' by the column default, which is what every worker
	// minted before this migration keeps being.
	if wkr.AnthropicBindMode != "default" {
		t.Fatalf("a fresh worker's mode = %q, want default", wkr.AnthropicBindMode)
	}

	// --- 1. The CHECK is a closed set ---------------------------------------
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET anthropic_bind_mode = 'whatever' WHERE id = $1`, wkr.ID); err == nil {
		t.Fatal("an unrecognised bind mode was accepted; 00088's CHECK is missing or wrong")
	} else if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Fatalf("illegal mode failed for the wrong reason: %v (want a check violation)", err)
	}

	// --- 2. The pair is written atomically, and both land --------------------
	pinned, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: owner,
		AnthropicSecretID: pgtype.UUID{Bytes: secret.ID, Valid: true},
		AnthropicBindMode: "pinned",
	})
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if pinned.AnthropicBindMode != "pinned" || uuid.UUID(pinned.AnthropicSecretID.Bytes) != secret.ID {
		t.Fatalf("pin wrote mode=%q id=%+v, want pinned + %v", pinned.AnthropicBindMode, pinned.AnthropicSecretID, secret.ID)
	}

	// --- 3. 🔴 D9: deleting the token unbinds and LEAVES the mode -------------
	// The DELETE succeeding is half the assertion. A CHECK coupling mode to id —
	// the constraint a reader will be tempted to add — would make this fail here,
	// which is why 00088 documents that it cannot exist.
	if _, err := q.DeleteUserSecret(ctx, store.DeleteUserSecretParams{ID: secret.ID, UserID: owner}); err != nil {
		t.Fatalf("deleting a pinned worker's credential failed: %v\n"+
			"this is what a mode↔id coupling CHECK looks like: 00078's FK nulls the id, the "+
			"constraint rejects the resulting row, and a legal delete becomes impossible", err)
	}
	after, err := q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		t.Fatalf("reload worker: %v", err)
	}
	if after.AnthropicSecretID.Valid {
		t.Fatalf("the binding survived the token delete (%+v); 00078's ON DELETE SET NULL did not fire", after.AnthropicSecretID)
	}
	if after.AnthropicBindMode != "pinned" {
		t.Fatalf("mode = %q after the token delete, want it LEFT as pinned — the row the "+
			"database legitimately holds, and the one D9 resolves as default", after.AnthropicBindMode)
	}
	// The worker itself must survive: SET NULL, never CASCADE (PRD #104 D5).
	if after.ID != wkr.ID || after.UserID != owner {
		t.Fatalf("worker row damaged by the token delete: %+v", after)
	}

	// --- The non-pinned modes never carry an id ------------------------------
	// Enforced in the service rather than the schema (a CHECK cannot, per D9), so
	// this asserts the QUERY accepts the shape the service writes: mode changed, id
	// left NULL, in one statement.
	auto, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: owner,
		AnthropicSecretID: pgtype.UUID{},
		AnthropicBindMode: "auto",
	})
	if err != nil {
		t.Fatalf("set auto: %v", err)
	}
	if auto.AnthropicBindMode != "auto" || auto.AnthropicSecretID.Valid {
		t.Fatalf("auto wrote mode=%q id=%+v, want auto + NULL", auto.AnthropicBindMode, auto.AnthropicSecretID)
	}

	// --- Owner scoping, same as every other worker write ---------------------
	stranger := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		stranger, fmt.Sprintf("bindmode-stranger-%s@e2e", stranger))
	if _, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: stranger, AnthropicBindMode: "pinned",
	}); err == nil {
		t.Fatal("a stranger changed another user's worker bind mode")
	}
}

// TestWorkerBindModeBackfillLiveDB proves 00088's backfill, which the migration
// itself cannot demonstrate at test time: by the time any test runs, 00088 has
// already applied to a table that was empty.
//
// So it reconstructs the pre-migration state — a worker with a binding and the
// column's DEFAULT 'default', which is exactly what an existing row would have
// looked like the instant after the ADD COLUMN and before the UPDATE — and runs the
// backfill's own predicate over it. It matters because the window between those two
// statements is the one a reader might be tempted to move into application code,
// and during it every pinned worker in the fleet silently spends the owner's
// default instead.
func TestWorkerBindModeBackfillLiveDB(t *testing.T) {
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

	owner := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("backfill-%s@e2e", owner))
	secret, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: owner, Kind: store.KindAnthropicToken, Label: "console-key", WantDefault: true,
		Ciphertext: []byte("ct"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}

	// Two workers in the shape 00088 found: one bound, one not, both carrying the
	// column DEFAULT rather than a chosen mode.
	bound, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "bound", TokenHash: tokenHash(),
		AnthropicSecretID: pgtype.UUID{Bytes: secret.ID, Valid: true},
		// DEFAULT deliberately, not pinned: this reconstructs the PRE-BACKFILL state —
		// a row carrying a binding while the mode still holds the column default, which
		// is exactly what 00088 found and what its UPDATE has to repair.
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("create bound worker: %v", err)
	}
	unbound, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "unbound", TokenHash: tokenHash(),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("create unbound worker: %v", err)
	}
	// CreateWorker does not set the mode, so both are at the DEFAULT — the
	// pre-backfill state, reconstructed rather than assumed.
	if bound.AnthropicBindMode != "default" || unbound.AnthropicBindMode != "default" {
		t.Fatalf("precondition: both workers should start at the column default, got %q and %q",
			bound.AnthropicBindMode, unbound.AnthropicBindMode)
	}

	// 00088's backfill statement — and asserted to BE 00088's, not a hand-copied
	// lookalike. Without this the test would prove only that the predicate I wrote
	// here is correct, which is worth nothing if the migration stops running it: the
	// two would drift and this would stay green. Reading the shipped file couples
	// them, so deleting the backfill from 00088 reddens this.
	const backfill = `UPDATE workers SET anthropic_bind_mode = 'pinned' WHERE anthropic_secret_id IS NOT NULL`
	migration, err := os.ReadFile("migrations/00088_worker_anthropic_bind_mode.sql")
	if err != nil {
		t.Fatalf("read 00088: %v", err)
	}
	// 🔴 THE CONTROL CATCHES DELETION AND HAS TO CATCH DISABLING, WHICH IS LIKELIER.
	// A bare strings.Contains over the whole file is satisfied by a COMMENTED-OUT
	// statement — measured: with the line prefixed `-- `, Contains still returns true —
	// and equally by one moved below the `-- +goose Down` marker. In both, the
	// migration ships with no working backfill, every worker that had a binding
	// silently stops honouring its pin, and this test stays green. Commenting out is
	// what someone does when "temporarily" disabling something, so it is at least as
	// likely as deleting.
	//
	// So: split on the Down marker, read only the Up half, and require the statement
	// on a line that is not itself a comment.
	up, _, found := strings.Cut(string(migration), "-- +goose Down")
	if !found {
		t.Fatal("00088 has no `-- +goose Down` marker; cannot tell its Up half from its Down half")
	}
	var live bool
	for _, line := range strings.Split(up, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, backfill) {
			live = true
			break
		}
	}
	if !live {
		t.Fatalf("00088's Up half contains no LIVE backfill statement (commented out, moved below "+
			"the Down marker, or deleted):\n\t%s\n"+
			"Without it every worker that had a binding when the column was added silently "+
			"stops honouring its pin and spends the owner's default instead.", backfill)
	}
	mustExec(ctx, t, pool, backfill)

	got := func(id uuid.UUID) string {
		t.Helper()
		w, gerr := q.GetWorkerByID(ctx, id)
		if gerr != nil {
			t.Fatalf("reload: %v", gerr)
		}
		return w.AnthropicBindMode
	}
	if m := got(bound.ID); m != "pinned" {
		t.Fatalf("a worker WITH a binding backfilled to %q, want pinned — it would silently "+
			"stop honouring its pin and spend the owner's default", m)
	}
	if m := got(unbound.ID); m != "default" {
		t.Fatalf("a worker with NO binding backfilled to %q, want default", m)
	}
}
