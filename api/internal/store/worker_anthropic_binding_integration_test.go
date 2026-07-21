package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestWorkerAnthropicBindingLiveDB pins the schema half of PRD #104 M3 — the part
// that has to hold when every handler check is bypassed (D11).
//
// Three properties, none of which a fake store can exhibit:
//
//  1. The composite FK REFUSES a cross-user binding. `workers.anthropic_secret_id`
//     alone would only prove the secret row exists; the FK is on
//     (user_id, anthropic_secret_id) → user_secrets (user_id, id), so a secret
//     belonging to someone else has no matching pair and the write is rejected by
//     Postgres. This is the layer the acceptance criterion means by "still refused
//     with the handler check stubbed out" — the statement here IS the handler check
//     stubbed out, since SetWorkerAnthropicSecret validates nothing itself.
//
//  2. Deleting a bound token UNBINDS its workers rather than deleting them (D5) —
//     and, critically, leaves workers.user_id intact. A bare ON DELETE SET NULL on
//     a composite FK nulls EVERY referencing column, which would null the NOT NULL
//     user_id and make the delete fail outright. The migration's
//     SET NULL (anthropic_secret_id) column list is what this asserts.
//
//  3. A NULL binding is legal (MATCH SIMPLE), because "no binding" is the state
//     every worker is in until someone changes it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestWorkerAnthropicBindingLiveDB(t *testing.T) {
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
			u, fmt.Sprintf("bind-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, isDefault bool) uuid.UUID {
		row, serr := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: user, Kind: store.KindAnthropicToken, Label: label, IsDefault: isDefault,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if serr != nil {
			t.Fatalf("insert secret %q: %v", label, serr)
		}
		return row.ID
	}
	ownerDefault := mkSecret(owner, "default", true)
	ownerConsole := mkSecret(owner, "console-key", false)
	strangerToken := mkSecret(stranger, "default", true)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "alpha", TokenHash: []byte{0xaa, 0x01},
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if wkr.AnthropicSecretID.Valid {
		t.Fatalf("a fresh worker must be unbound (NULL), got %+v", wkr.AnthropicSecretID)
	}

	// --- 1. Cross-user binding is refused by the SCHEMA, not by a handler ---
	_, err = q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: owner,
		AnthropicSecretID: pgtype.UUID{Bytes: strangerToken, Valid: true},
	})
	if err == nil {
		t.Fatal("binding a worker to ANOTHER user's secret succeeded — the composite FK is not doing its job, " +
			"and a crafted PATCH would spend that user's Anthropic account")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("cross-user binding failed for the wrong reason: %v (want a foreign key violation)", err)
	}
	// It must also still be unbound — a rejected write must not have partially applied.
	after, err := q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		t.Fatalf("reload worker: %v", err)
	}
	if after.AnthropicSecretID.Valid {
		t.Fatal("worker was left bound after a rejected cross-user binding")
	}

	// --- The owner's own secrets bind fine, and clear again ---
	bound, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: owner,
		AnthropicSecretID: pgtype.UUID{Bytes: ownerConsole, Valid: true},
	})
	if err != nil {
		t.Fatalf("bind to own secret: %v", err)
	}
	if uuid.UUID(bound.AnthropicSecretID.Bytes) != ownerConsole {
		t.Fatalf("bound to %v, want %v", bound.AnthropicSecretID, ownerConsole)
	}

	// --- A foreign WORKER id is a no-op, not a cross-tenant write ---
	if _, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: stranger,
		AnthropicSecretID: pgtype.UUID{Bytes: strangerToken, Valid: true},
	}); err == nil {
		t.Fatal("a stranger rebinding someone else's worker must affect no rows (pgx.ErrNoRows), got success")
	}

	// --- 2. Deleting the bound token unbinds the worker and keeps its owner ---
	if _, err := q.DeleteUserSecret(ctx, store.DeleteUserSecretParams{ID: ownerConsole, UserID: owner}); err != nil {
		t.Fatalf("deleting a BOUND token must succeed and unbind its workers (D5); it failed: %v — "+
			"a bare ON DELETE SET NULL on this composite FK would try to null workers.user_id, which is NOT NULL", err)
	}
	unbound, err := q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		t.Fatalf("reload after token delete: %v", err)
	}
	if unbound.AnthropicSecretID.Valid {
		t.Fatal("worker still bound after its token was deleted — ON DELETE SET NULL did not fire")
	}
	if unbound.UserID != owner {
		t.Fatalf("worker owner = %v, want %v — SET NULL nulled the wrong column", unbound.UserID, owner)
	}

	// --- 3. An explicit NULL binding is legal (MATCH SIMPLE) ---
	if _, err := q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID: wkr.ID, UserID: owner, AnthropicSecretID: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("clearing a binding to NULL must be legal: %v", err)
	}

	// --- The mint-time binding takes the same FK path ---
	if _, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "beta", TokenHash: []byte{0xaa, 0x02},
		AnthropicSecretID: pgtype.UUID{Bytes: strangerToken, Valid: true},
	}); err == nil {
		t.Fatal("minting a worker already bound to another user's secret must be refused by the FK")
	}
	minted, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "gamma", TokenHash: []byte{0xaa, 0x03},
		AnthropicSecretID: pgtype.UUID{Bytes: ownerDefault, Valid: true},
	})
	if err != nil {
		t.Fatalf("minting a worker bound to the owner's own secret: %v", err)
	}
	if uuid.UUID(minted.AnthropicSecretID.Bytes) != ownerDefault {
		t.Fatalf("minted binding = %v, want %v", minted.AnthropicSecretID, ownerDefault)
	}

	// --- The list query surfaces the label, and NULL for an unbound worker ---
	rows, err := q.ListWorkersByUser(ctx, owner)
	if err != nil {
		t.Fatalf("ListWorkersByUser: %v", err)
	}
	labels := map[string]pgtype.Text{}
	for _, r := range rows {
		labels[r.Name] = r.AnthropicSecretLabel
	}
	if got := labels["gamma"]; !got.Valid || got.String != "default" {
		t.Fatalf("bound worker's label = %+v, want \"default\"", got)
	}
	if got := labels["alpha"]; got.Valid {
		t.Fatalf("unbound worker must have a NULL label, got %+v", got)
	}
}
