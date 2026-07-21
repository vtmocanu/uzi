package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestUserSecretsRewrapLiveDB is the PRD #104 D10 regression gate at the SQL layer:
// the vault's lazy rewrap must update exactly the row it opened, never every row of
// the same kind.
//
// Before the fix, RewrapUserSecret's predicate was
// `WHERE user_id = $2 AND kind = $3 AND sealed_with = 'master'` and
// ListMasterSealedSecrets returned no id, so the rewrap loop's first iteration
// matched EVERY master-sealed row of the kind and overwrote the siblings with the
// first row's resealed bytes; the later iterations then matched nothing and the loop
// completed without an error. The failure mode is silent data loss, which is why the
// gate lives here (real SQL, real set-based UPDATE) and not only against the vault's
// fake store: a fake can be written to match one row per call and would go green
// against the broken statement.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserSecretsRewrapLiveDB(t *testing.T) {
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

	// Two users: the owner holds two master-sealed tokens of one kind; the bystander
	// holds one, to prove the rewrap is scoped to its owner as well as to its row.
	owner, bystander := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, bystander} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("secrets-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, ct []byte) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			 VALUES ($1, $2, $3, $4, $5, $6, 'master')`,
			id, user, store.KindAnthropicToken, label, label == "default", ct)
		return id
	}
	first := mkSecret(owner, "default", []byte("sealed-token-one"))
	second := mkSecret(owner, "console-key", []byte("sealed-token-two"))
	foreign := mkSecret(bystander, "default", []byte("sealed-token-bystander"))

	// The listing must carry the id — without it the loop below cannot address a row.
	rows, err := q.ListMasterSealedSecrets(ctx, owner)
	if err != nil {
		t.Fatalf("list master-sealed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListMasterSealedSecrets returned %d rows, want 2 (both of the owner's tokens)", len(rows))
	}
	listed := map[uuid.UUID][]byte{}
	for _, r := range rows {
		listed[r.ID] = r.Ciphertext
	}
	if string(listed[first]) != "sealed-token-one" || string(listed[second]) != "sealed-token-two" {
		t.Fatalf("listing did not pair each id with its own ciphertext: %v", listed)
	}

	// Rewrap each row exactly as vault.rewrapMasterSecrets does: one statement per
	// listed row, carrying that row's id.
	for id, want := range map[uuid.UUID][]byte{
		first:  []byte("rewrapped-token-one"),
		second: []byte("rewrapped-token-two"),
	} {
		n, rerr := q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
			ID: id, UserID: owner, Ciphertext: want,
		})
		if rerr != nil {
			t.Fatalf("rewrap %s: %v", id, rerr)
		}
		if n != 1 {
			t.Fatalf("rewrap %s affected %d rows, want exactly 1 — the predicate is matching siblings", id, n)
		}
	}

	readBack := func(id uuid.UUID) (ct []byte, sealedWith string) {
		if err := pool.QueryRow(ctx, `SELECT ciphertext, sealed_with FROM user_secrets WHERE id = $1`, id).
			Scan(&ct, &sealedWith); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		return ct, sealedWith
	}
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		ct   string
		sw   string
	}{
		{"first", first, "rewrapped-token-one", store.SealedWithDEK},
		{"second", second, "rewrapped-token-two", store.SealedWithDEK},
		{"bystander", foreign, "sealed-token-bystander", store.SealedWithMaster},
	} {
		ct, sw := readBack(tc.id)
		if string(ct) != tc.ct {
			t.Errorf("%s row ciphertext = %q, want %q — a sibling's rewrap overwrote it", tc.name, ct, tc.ct)
		}
		if sw != tc.sw {
			t.Errorf("%s row sealed_with = %q, want %q", tc.name, sw, tc.sw)
		}
	}

	// The sealed_with guard is the double-rewrap protection PRD #32 relies on: a
	// second unlock racing the first must not clobber a now-'dek' row with stale
	// master-era bytes.
	n, err := q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
		ID: first, UserID: owner, Ciphertext: []byte("stale-bytes"),
	})
	if err != nil {
		t.Fatalf("double rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-rewrapping a 'dek' row affected %d rows, want 0 (the sealed_with guard is gone)", n)
	}

	// Defensive scope: another user's id is a no-op, not a cross-user write.
	n, err = q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
		ID: foreign, UserID: owner, Ciphertext: []byte("cross-user-bytes"),
	})
	if err != nil {
		t.Fatalf("cross-user rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("rewrapping another user's secret affected %d rows, want 0", n)
	}
}
