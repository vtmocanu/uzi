package store_test

import (
	"os"
	"strings"
	"testing"
)

// A partial index still scans the existing secret table. It must not extend the
// ACCESS EXCLUSIVE lock held by the transaction that adds provider_id.
func TestProviderKeyIndexHasOnlineMigration(t *testing.T) {
	columns, err := os.ReadFile("migrations/00477_user_secrets_provider_key.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(columns), "CREATE UNIQUE INDEX user_secrets_one_provider_key") {
		t.Fatal("provider key table-alter migration builds the index; move its scan to a concurrent migration")
	}
	b, err := os.ReadFile("migrations/00490_user_secrets_provider_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "-- +goose NO TRANSACTION\n") || strings.Contains(s, "ALTER TABLE") {
		t.Fatal("the provider key index must build outside a table-alter transaction")
	}
	up, down, ok := strings.Cut(s, "-- +goose Down")
	drop := "DROP INDEX CONCURRENTLY IF EXISTS user_secrets_one_provider_key;"
	create := "CREATE UNIQUE INDEX CONCURRENTLY user_secrets_one_provider_key"
	if !ok || !strings.Contains(up, drop) || !strings.Contains(up, create) ||
		strings.Index(up, drop) >= strings.Index(up, create) || strings.Contains(down, "DROP INDEX") || !strings.Contains(down, "SELECT 1;") {
		t.Fatal("online index must drop/rebuild on retry and keep uniqueness until the guarded column Down")
	}
}

func TestProviderKeyIndexCatalogAndUniquenessLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	var valid, unique bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid, indisunique FROM pg_index
        WHERE indexrelid = 'user_secrets_one_provider_key'::regclass`).Scan(&valid, &unique); err != nil || !valid || !unique {
		t.Fatalf("provider key index valid=%t unique=%t: %v", valid, unique, err)
	}
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "index-owner")
	if _, err := insertProviderKey(ctx, q, owner, p.ID); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("migrations/00490_user_secrets_provider_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(b), "-- +goose Down")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	// A different label bypasses the generic (user, kind, label) unique index:
	// only the provider index can reject this second key for the same provider.
	_, err = pool.Exec(ctx, `INSERT INTO user_secrets
        (user_id, kind, label, ciphertext, sealed_with, is_default, auto_eligible, provider_id, provider_scope_id)
        VALUES ($1, 'anthropic_compatible_key', 'different-label', 'ct', 'master', false, false, $2, $1)`, owner, p.ID)
	if code, constraint := pgCodeC(err); code != "23505" || constraint != "user_secrets_one_provider_key" {
		t.Fatalf("second provider key = %v, want the provider index's unique violation", err)
	}
}
