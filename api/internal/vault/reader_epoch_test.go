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
	v     *Vault
	owner uuid.UUID
	calls int
	next  store.GetUserVaultStateRow
	err   error
}

func (r *retiringReader) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	r.calls++
	if r.calls == 1 {
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
			v, _ := newTestVault(t)
			uid := uuid.New()
			r := &retiringReader{v: v, owner: uid, next: tc.state, err: tc.err}
			ctx := WithStateReader(context.Background(), r)
			plain, err := v.OpenAtEpoch(ctx, uid, testKind, store.SealedWithDEK, []byte("unavailable"), 7)
			if err != tc.want || plain != nil || r.calls != 2 {
				t.Fatalf("open: error=%v want=%v plaintext-present=%v reads=%d", err, tc.want, plain != nil, r.calls)
			}
		})
	}
}
