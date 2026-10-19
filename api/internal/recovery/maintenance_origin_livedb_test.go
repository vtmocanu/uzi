package recovery

import (
	"testing"

	"github.com/google/uuid"
)

// The producer's local origin is an opaque fallback idempotency key, not the
// server capture UUID. Exercise the real existing Reserve path without changing
// the server grammar, custody binding or schema.
func TestMaintenanceOriginFallbackReserveLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	key := "maintenance-v1-" + uuid.NewString()
	if len(key) != 51 {
		t.Fatalf("local origin length = %d, want 51", len(key))
	}
	first := e.reserve(key)
	second := e.reserve(key)
	if first != second {
		t.Fatalf("retry changed server capture identity: %s != %s", first, second)
	}
	var storedKey, state string
	var hold uuid.UUID
	var count int
	if err := e.pool.QueryRow(e.ctx,
		"SELECT idempotency_key, hold_id, state FROM recovery_captures WHERE id = $1", first).Scan(&storedKey, &hold, &state); err != nil {
		t.Fatal(err)
	}
	if storedKey != key || hold != e.hold || state != "preparing" {
		t.Fatalf("reserve binding = key %q hold %s state %q", storedKey, hold, state)
	}
	if err := e.pool.QueryRow(e.ctx,
		"SELECT count(*) FROM recovery_captures WHERE hold_id = $1", e.hold).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retry created %d captures, want one", count)
	}
}
