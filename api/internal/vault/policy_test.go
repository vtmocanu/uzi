package vault

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyRecoverySealBoundary(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
		t.Run(kind, func(t *testing.T) {
			v, master, st := newTestVaultWithStore(t)
			id := uuid.New()
			st.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 8}
			if v.Unlocked(id) || !v.RequiresUnlock(ctx, id) {
				t.Fatal("protected owner without cache must require unlock")
			}
			if got, err := v.Seal(ctx, id, kind, []byte("boundary"), SealForUse); !errors.Is(err, ErrLocked) || len(got.Ciphertext) != 0 {
				t.Fatalf("locked use: %+v, %v", got, err)
			}
			got, err := v.Seal(ctx, id, kind, []byte("boundary"), SealForRecovery)
			if kind != store.KindCodexAuth {
				if !errors.Is(err, ErrInvalidState) || len(got.Ciphertext) != 0 {
					t.Fatalf("non-Codex recovery: %+v, %v", got, err)
				}
				return
			}
			if err != nil || got.Mode != store.VaultModeProtected || got.Epoch != 8 || got.SealedWith != store.SealedWithMasterAAD || !got.RecoveryMaster {
				t.Fatalf("locked recovery fence: %+v, %v", got, err)
			}
			binding := append(append(append([]byte{}, id[:]...), 0), []byte(kind)...)
			plain, err := master.OpenWithAAD(got.Ciphertext, binding)
			if err != nil || string(plain) != "boundary" {
				t.Fatal("recovery lost exact owner/kind AAD")
			}
			if _, err := master.Open(got.Ciphertext); err == nil {
				t.Fatal("recovery accepted missing AAD")
			}
			for _, other := range []struct {
				id   uuid.UUID
				kind string
			}{{uuid.New(), kind}, {id, store.KindOpenAIAPIKey}} {
				if _, err := v.Open(other.id, other.kind, got.SealedWith, got.Ciphertext); err == nil {
					t.Fatal("recovery accepted wrong owner/kind")
				}
			}
			if err := v.Unlock(ctx, id, "policy-boundary-password"); err != nil {
				t.Fatal(err)
			}
			current, err := v.State(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got, err = v.Seal(ctx, id, kind, []byte("private recovery"), SealForRecovery)
			if err != nil || got.SealedWith != store.SealedWithDEK || got.RecoveryMaster || got.Mode != current.VaultMode || got.Epoch != current.VaultEpoch || v.RequiresUnlock(ctx, id) {
				t.Fatalf("unlocked recovery: %+v, %v", got, err)
			}
			plain, err = v.Open(id, kind, got.SealedWith, got.Ciphertext)
			if err != nil || string(plain) != "private recovery" {
				t.Fatal("DEK recovery unreadable")
			}
			if _, err := master.OpenWithAAD(got.Ciphertext, binding); err == nil {
				t.Fatal("master opened unlocked recovery")
			}

			// Change only the durable epoch: an old cached key must not authorize use.
			st.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: current.VaultEpoch + 1}
			if !v.RequiresUnlock(ctx, id) {
				t.Fatal("stale cache authorized protected owner")
			}
			if _, err := v.Seal(ctx, id, kind, []byte("stale"), SealForUse); !errors.Is(err, ErrLocked) {
				t.Fatalf("stale cache seal: %v", err)
			}
			got, err = v.Seal(ctx, id, kind, []byte("stale recovery"), SealForRecovery)
			if err != nil || !got.RecoveryMaster || got.SealedWith != store.SealedWithMasterAAD || got.Epoch != current.VaultEpoch+1 {
				t.Fatalf("stale recovery: %+v, %v", got, err)
			}
			v.Lock(id)
			st.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 12}
			if v.RequiresUnlock(ctx, id) {
				t.Fatal("off owner without cache requires unlock")
			}
			got, err = v.Seal(ctx, id, kind, []byte("off recovery"), SealForRecovery)
			if err != nil || got.RecoveryMaster || got.SealedWith != store.SealedWithMasterAAD || got.Mode != store.VaultModeUnprotected || got.Epoch != 12 {
				t.Fatalf("off recovery: %+v, %v", got, err)
			}
			plain, err = master.OpenWithAAD(got.Ciphertext, binding)
			if err != nil || !bytes.Equal(plain, []byte("off recovery")) {
				t.Fatal("off recovery unreadable")
			}
		})
	}
}

type policyStateReader struct {
	state store.GetUserVaultStateRow
	err   error
}

func (r policyStateReader) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	return r.state, r.err
}

func TestPolicySealFailsClosed(t *testing.T) {
	lookupErr := errors.New("state unavailable")
	for _, tc := range []struct {
		name, mode   string
		epoch        int64
		lookup, want error
	}{
		{"lookup", store.VaultModeUnprotected, 0, lookupErr, lookupErr},
		{"invalid mode", "unknown", 0, nil, ErrInvalidState},
		{"negative protected", store.VaultModeProtected, -1, nil, ErrInvalidState},
		{"negative off", store.VaultModeUnprotected, -1, nil, ErrInvalidState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := newTestVault(t)
			ctx := WithStateReader(context.Background(), policyStateReader{store.GetUserVaultStateRow{VaultMode: tc.mode, VaultEpoch: tc.epoch}, tc.lookup})
			id := uuid.New()
			if !v.RequiresUnlock(ctx, id) {
				t.Fatal("bad state did not fail closed")
			}
			for _, purpose := range []SealPurpose{SealForUse, SealForRecovery} {
				got, err := v.Seal(ctx, id, store.KindCodexAuth, []byte("blocked"), purpose)
				if !errors.Is(err, tc.want) || len(got.Ciphertext) != 0 {
					t.Fatalf("bad state seal: %+v, %v", got, err)
				}
			}
		})
	}
	for _, mode := range []string{store.VaultModeProtected, store.VaultModeUnprotected} {
		v, _, st := newTestVaultWithStore(t)
		id := uuid.New()
		st.states[id] = store.LockVaultAuthStateRow{VaultMode: mode, VaultEpoch: 3}
		for _, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
			got, err := v.Seal(context.Background(), id, kind, []byte("blocked"), SealPurpose(255))
			if !errors.Is(err, ErrInvalidState) || len(got.Ciphertext) != 0 {
				t.Fatalf("invalid purpose: %+v, %v", got, err)
			}
			if kind != store.KindCodexAuth {
				if _, err := v.Seal(context.Background(), id, kind, nil, SealForRecovery); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("non-Codex recovery in %s: %v", mode, err)
				}
			}
		}
	}
}

func TestPolicyConstructorRequiresTransaction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil transaction accepted")
		}
	}()
	New(nil, newFakeVaultStore(), nil)
}
