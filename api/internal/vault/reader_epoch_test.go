package vault

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The first read returns the captured epoch, then retires its cache before
// OpenAtEpoch can look up the DEK. No lifecycle timing or sleeps are needed.
type retiringReader struct {
	v            *Vault
	owner        uuid.UUID
	calls        int
	next         store.GetUserVaultStateRow
	err          error
	hadCachedKey bool
}

func (r *retiringReader) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	r.calls++
	if r.calls == 1 {
		r.hadCachedKey = r.v.Unlocked(r.owner)
		r.v.Lock(r.owner)
		return store.GetUserVaultStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 7}, nil
	}
	return r.next, r.err
}

func TestOpenAtEpochRetiredCache(t *testing.T) {
	infra := errors.New("state lookup failed")
	for _, tc := range []struct {
		name      string
		state     store.GetUserVaultStateRow
		err, want error
	}{
		{"disable", store.GetUserVaultStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 8}, nil, store.ErrVaultSealStale},
		{"disable enable ABA", store.GetUserVaultStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 9}, nil, store.ErrVaultSealStale},
		{"genuine lock", store.GetUserVaultStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 7}, nil, ErrLocked},
		{"lookup failure", store.GetUserVaultStateRow{}, infra, infra},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, f := newTestVaultWithStore(t)
			uid := uuid.New()
			f.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 6}
			if err := v.Unlock(context.Background(), uid, "cache-reader-fixture-passphrase"); err != nil {
				t.Fatal(err)
			}
			sealed, err := v.Seal(context.Background(), uid, testKind, []byte("reader fixture material"), SealForUse)
			if err != nil || sealed.Epoch != 7 || !v.Unlocked(uid) {
				t.Fatalf("cached epoch-seven fixture: error=%v epoch=%d cached=%v", err, sealed.Epoch, v.Unlocked(uid))
			}
			r := &retiringReader{v: v, owner: uid, next: tc.state, err: tc.err}
			ctx := WithStateReader(context.Background(), r)
			plain, err := v.OpenAtEpoch(ctx, uid, testKind, sealed.SealedWith, sealed.Ciphertext, sealed.Epoch)
			if err != tc.want || plain != nil || r.calls != 2 || !r.hadCachedKey || v.Unlocked(uid) {
				t.Fatalf("open: error=%v want=%v plaintext-present=%v reads=%d had-key=%v still-cached=%v", err, tc.want, plain != nil, r.calls, r.hadCachedKey, v.Unlocked(uid))
			}
		})
	}
}
