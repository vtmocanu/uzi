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

// TestUserJudgeBindingLiveDB pins the schema half of PRD #104 M4, and one property
// in particular that would otherwise be discovered by a user rather than by a test.
//
// 00079's foreign key is composite — (users.id, judge_anthropic_secret_id) →
// user_secrets (user_id, id) — and a bare ON DELETE SET NULL on a composite key
// nulls EVERY referencing column. Here that means users.id, the PRIMARY KEY. Postgres
// would reject the delete, so deleting a judge-bound token would FAIL rather than
// unbind, and only for users who had actually set a binding. The migration uses the
// Postgres 15+ column list, SET NULL (judge_anthropic_secret_id); this test asserts
// both halves of the result — that the delete succeeds AND that users.id survives it
// — because asserting only the first would pass against a FK that nulled the wrong
// column but happened not to be exercised.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserJudgeBindingLiveDB(t *testing.T) {
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
			u, fmt.Sprintf("judgebind-%s@e2e", u))
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
	ownerDefault := mkSecret(owner, "default", true)
	ownerCheap := mkSecret(owner, "cheap-console", false)
	strangerToken := mkSecret(stranger, "default", true)

	// A user starts unbound: their retrospectives spend their default.
	if bound, berr := q.GetUserJudgeAnthropicSecret(ctx, owner); berr != nil || bound.Valid {
		t.Fatalf("fresh user judge binding = (%+v,%v), want NULL", bound, berr)
	}

	// --- Cross-user binding refused by the SCHEMA ---
	if _, err := q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID: owner, JudgeAnthropicSecretID: pgtype.UUID{Bytes: strangerToken, Valid: true},
	}); err == nil {
		t.Fatal("binding the judge lane to ANOTHER user's secret succeeded — the composite FK is not enforcing, " +
			"and a crafted request would spend that user's Anthropic account on retrospectives")
	} else if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("cross-user judge binding failed for the wrong reason: %v (want a foreign key violation)", err)
	}

	// --- The user's own secret binds, and reads back ---
	if _, err := q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID: owner, JudgeAnthropicSecretID: pgtype.UUID{Bytes: ownerCheap, Valid: true},
	}); err != nil {
		t.Fatalf("bind judge to own secret: %v", err)
	}
	bound, err := q.GetUserJudgeAnthropicSecret(ctx, owner)
	if err != nil {
		t.Fatalf("read judge binding: %v", err)
	}
	if !bound.Valid || uuid.UUID(bound.Bytes) != ownerCheap {
		t.Fatalf("judge binding = %+v, want %v", bound, ownerCheap)
	}

	// --- THE TRAP: deleting the bound token must unbind, not fail, and must not
	// touch users.id ---
	if _, err := q.DeleteUserSecret(ctx, store.DeleteUserSecretParams{ID: ownerCheap, UserID: owner}); err != nil {
		t.Fatalf("deleting a JUDGE-BOUND token must succeed and unbind (D5); it failed: %v — "+
			"a bare ON DELETE SET NULL on this composite FK would try to null users.id, the PRIMARY KEY", err)
	}
	after, err := q.GetUserJudgeAnthropicSecret(ctx, owner)
	if err != nil {
		t.Fatalf("read judge binding after delete: %v", err)
	}
	if after.Valid {
		t.Fatal("judge binding survived its token's deletion — ON DELETE SET NULL did not fire")
	}
	// The user must still exist, with the same id. If SET NULL had targeted users.id
	// the delete would have errored above; this catches the subtler case where the
	// row is somehow reachable but re-keyed.
	var stillThere uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE id = $1`, owner).Scan(&stillThere); err != nil {
		t.Fatalf("the user vanished or was re-keyed when their judge token was deleted: %v", err)
	}
	if stillThere != owner {
		t.Fatalf("users.id = %v, want %v — SET NULL nulled the wrong column", stillThere, owner)
	}

	// --- Clearing the binding explicitly is legal (MATCH SIMPLE) ---
	if _, err := q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID: owner, JudgeAnthropicSecretID: pgtype.UUID{Bytes: ownerDefault, Valid: true},
	}); err != nil {
		t.Fatalf("re-bind to the default token: %v", err)
	}
	if _, err := q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID: owner, JudgeAnthropicSecretID: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("clearing the judge binding must be legal: %v", err)
	}
	if cleared, _ := q.GetUserJudgeAnthropicSecret(ctx, owner); cleared.Valid {
		t.Fatal("judge binding not cleared")
	}

	// --- A user can bind their judge lane and a worker independently ---
	//
	// The two bindings are separate columns on separate tables by design (D1): the
	// point of the feature is that retrospectives can bill a different account from
	// the runs they review, so this asserts they do not collide.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: owner, Name: "alpha", TokenHash: tokenHash(),
		AnthropicSecretID: pgtype.UUID{Bytes: ownerDefault, Valid: true},
	})
	if err != nil {
		t.Fatalf("create bound worker: %v", err)
	}
	second := mkSecret(owner, "judge-key", false)
	if _, err := q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID: owner, JudgeAnthropicSecretID: pgtype.UUID{Bytes: second, Valid: true},
	}); err != nil {
		t.Fatalf("bind judge while a worker is bound elsewhere: %v", err)
	}
	reread, err := q.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		t.Fatalf("reload worker: %v", err)
	}
	if uuid.UUID(reread.AnthropicSecretID.Bytes) != ownerDefault {
		t.Fatalf("the worker's binding changed when the judge binding was set: %+v", reread.AnthropicSecretID)
	}
	judgeNow, _ := q.GetUserJudgeAnthropicSecret(ctx, owner)
	if uuid.UUID(judgeNow.Bytes) != second {
		t.Fatalf("judge binding = %+v, want %v", judgeNow, second)
	}
}
