package vault

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// UsableAtState answers from a state the caller already read: unprotected is always usable,
// protected only with the DEK cached for that exact epoch, and a nil vault never holds one.
func TestUsableAtState(t *testing.T) {
	v, _ := newTestVault(t)
	uid := uuid.New()
	protected := func(epoch int64) store.GetUserVaultStateRow {
		return store.GetUserVaultStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: epoch}
	}
	unprotected := store.GetUserVaultStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 3}

	if !v.UsableAtState(uid, unprotected) {
		t.Fatal("unprotected state must be usable without a cached key")
	}
	if v.UsableAtState(uid, protected(3)) {
		t.Fatal("protected state without a cached key must not be usable")
	}

	v.put(uid, make([]byte, 32), 3)
	if !v.UsableAtState(uid, protected(3)) {
		t.Fatal("protected state with the key cached at its epoch must be usable")
	}
	if v.UsableAtState(uid, protected(4)) {
		t.Fatal("a key cached for another epoch must not make the state usable")
	}
	if v.UsableAtState(uuid.New(), protected(3)) {
		t.Fatal("another user's cached key must not make the state usable")
	}

	var nilVault *Vault
	if nilVault.UsableAtState(uid, protected(3)) {
		t.Fatal("a nil vault must report protected state as not usable")
	}
	if !nilVault.UsableAtState(uid, unprotected) {
		t.Fatal("a nil vault must still report unprotected state as usable")
	}
}
