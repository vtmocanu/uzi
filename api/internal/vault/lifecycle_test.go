package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestOffUnlockPreservesDurableMaterialAndEvictsRetiredKey(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "trusted", true: "existing"}[existing], func(t *testing.T) {
			v, master, st := newTestVaultWithStore(t)
			uid := uuid.New()
			const password = "old-wrapping-password"
			if err := v.Unlock(context.Background(), uid, password); err != nil {
				t.Fatal(err)
			}
			// Deliberately leave a retired wrapper and cache entry in an off fixture.
			st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 7}
			legacy, err := master.Seal([]byte("legacy material"))
			if err != nil {
				t.Fatal(err)
			}
			sid := st.putSecret(uid, testKind, legacy, store.SealedWithMaster)
			before, _ := st.getSecretByID(sid)
			wrapper := st.vaults[uid]
			state := st.states[uid]
			clearCalls := len(st.clearNoticeCalls)
			retired := v.cache[uid].dek
			if existing {
				err = v.UnlockExisting(context.Background(), uid, "ignored")
			} else {
				err = v.Unlock(context.Background(), uid, "ignored")
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := st.getSecretByID(sid)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(wrapper, st.vaults[uid]) || state != st.states[uid] {
				t.Fatal("off unlock changed material, wrapper, mode, or epoch")
			}
			if v.Unlocked(uid) || !bytes.Equal(retired, make([]byte, len(retired))) {
				t.Fatal("off unlock retained retired key")
			}
			if len(st.clearNoticeCalls) != clearCalls {
				t.Fatal("off unlock cleared a lock notice")
			}
		})
	}
}

func TestEnableDisableMaterialRoundTrip(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	ctx := context.Background()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 2}
	const password = "current-login-password"
	const plaintext = "round trip material"
	off, err := v.Seal(ctx, uid, testKind, []byte(plaintext), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	sid := st.putSecret(uid, testKind, off.Ciphertext, off.SealedWith)
	if err := v.Enable(ctx, uid, password); err != nil {
		t.Fatal(err)
	}
	protected, _ := st.getSecretByID(sid)
	if protected.sealedWith != store.SealedWithDEK {
		t.Fatal("enable did not convert material to DEK")
	}
	opened, err := v.OpenAtEpoch(ctx, uid, testKind, protected.sealedWith, protected.ciphertext, 3)
	if err != nil || string(opened) != plaintext {
		t.Fatalf("enabled material: %v", err)
	}
	if _, err := v.OpenAtEpoch(ctx, uid, testKind, off.SealedWith, off.Ciphertext, off.Epoch); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatalf("old off snapshot: %v", err)
	}
	if err := v.Disable(ctx, uid); err != nil {
		t.Fatal(err)
	}
	current, _ := st.getSecretByID(sid)
	if current.sealedWith != store.SealedWithMasterAAD || v.Unlocked(uid) {
		t.Fatal("disable did not convert material and evict")
	}
	if _, ok := st.vaults[uid]; ok {
		t.Fatal("disable retained wrapper")
	}
	opened, err = v.OpenAtEpoch(ctx, uid, testKind, current.sealedWith, current.ciphertext, 4)
	if err != nil || string(opened) != plaintext {
		t.Fatalf("disabled material: %v", err)
	}
	if _, err := v.OpenAtEpoch(ctx, uid, testKind, protected.sealedWith, protected.ciphertext, 3); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatalf("old protected snapshot: %v", err)
	}
	if _, err := v.Open(uuid.New(), testKind, current.sealedWith, current.ciphertext); err == nil {
		t.Fatal("disabled material lost owner AAD")
	}
}

func TestEnableVerifiesCurrentPassword(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	const password = "current-login-password"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	initial := store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 4, PasswordHash: pgtype.Text{String: hash, Valid: true}}
	st.states[uid] = initial
	if err := v.Enable(context.Background(), uid, "wrong-login-password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, ok := st.vaults[uid]; ok {
		t.Fatal("wrong enable password created wrapper")
	}
	if v.Unlocked(uid) || st.states[uid] != initial {
		t.Fatal("wrong enable password changed cache or mode")
	}
	if err := v.Enable(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	if !v.Unlocked(uid) || st.states[uid].VaultMode != store.VaultModeProtected || st.states[uid].VaultEpoch != 5 {
		t.Fatal("valid password did not enable")
	}
	v.Lock(uid)
	if err := v.UnlockExisting(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
}

func TestCreationPasswordPolicy(t *testing.T) {
	for _, hasPassword := range []bool{false, true} {
		for _, n := range []int{auth.MinPasswordLen - 1, auth.MinPasswordLen, auth.MaxPasswordLen, auth.MaxPasswordLen + 1} {
			t.Run(fmt.Sprintf("password=%v/length=%d", hasPassword, n), func(t *testing.T) {
				v, _, st := newTestVaultWithStore(t)
				uid := uuid.New()
				password := strings.Repeat("p", n)
				state := store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
				if hasPassword {
					hash, err := auth.HashPassword(password)
					if err != nil {
						t.Fatal(err)
					}
					state.PasswordHash = pgtype.Text{String: hash, Valid: true}
				}
				st.states[uid] = state
				err := v.Enable(context.Background(), uid, password)
				// Existing password users may retain a historically shorter login password.
				invalid := n > auth.MaxPasswordLen || (!hasPassword && n < auth.MinPasswordLen)
				if invalid {
					if !errors.Is(err, ErrPassphraseLength) {
						t.Fatalf("length %d: %v", n, err)
					}
					if _, ok := st.vaults[uid]; ok || v.Unlocked(uid) {
						t.Fatal("invalid length created key material")
					}
				} else if err != nil {
					t.Fatalf("length %d: %v", n, err)
				}
			})
		}
	}
}

func TestRequiredPolicyRefusesDisable(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	if err := v.Unlock(context.Background(), uid, "protected-password"); err != nil {
		t.Fatal(err)
	}
	st.policy = "required"
	wrapper := st.vaults[uid]
	state := st.states[uid]
	if err := v.Disable(context.Background(), uid); !errors.Is(err, ErrRequiredPolicy) {
		t.Fatalf("Disable: %v", err)
	}
	if !v.Unlocked(uid) || state != st.states[uid] || !reflect.DeepEqual(wrapper, st.vaults[uid]) {
		t.Fatal("policy rejection changed protected vault")
	}
}

type failingMaterialStore struct {
	*fakeVaultStore
	err error
}

func (s *failingMaterialStore) GetVaultMaterialTotals(context.Context, uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	return store.GetVaultMaterialTotalsRow{}, s.err
}

func TestLifecycleCallbackFailurePreservesCachedKey(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	if err := v.Unlock(context.Background(), uid, "protected-password"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal(context.Background(), uid, testKind, []byte("still readable"), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("deterministic totals failure")
	failing := &failingMaterialStore{fakeVaultStore: st, err: wantErr}
	v.q = failing
	v.transact = st.transaction(failing)
	wrapper, state := st.vaults[uid], st.states[uid]
	if err := v.Disable(context.Background(), uid); !errors.Is(err, wantErr) {
		t.Fatalf("Disable: %v", err)
	}
	if !reflect.DeepEqual(wrapper, st.vaults[uid]) || state != st.states[uid] {
		t.Fatal("pre-write failure changed fixture state")
	}
	plain, err := v.Open(uid, testKind, sealed.SealedWith, sealed.Ciphertext)
	if err != nil || string(plain) != "still readable" {
		t.Fatalf("cached key lost: %v", err)
	}
}

func TestExistingUnlockLazyFailureIsBestEffort(t *testing.T) {
	v, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	const password = "protected-password"
	if err := v.Unlock(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal(context.Background(), uid, testKind, []byte("current material"), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	v.Lock(uid)
	legacy, err := master.Seal([]byte("legacy material"))
	if err != nil {
		t.Fatal(err)
	}
	sid := st.putSecret(uid, testKind, legacy, store.SealedWithMaster)
	failing := &failingMaterialStore{fakeVaultStore: st, err: errors.New("conversion refused")}
	v.q = failing
	tx := st.transaction(failing)
	calls := 0
	v.transact = func(ctx context.Context, fn func(context.Context, Store) error) error {
		calls++
		if calls == 1 && v.Unlocked(uid) {
			t.Fatal("key published before validation transaction committed")
		}
		if calls == 2 && !v.Unlocked(uid) {
			t.Fatal("key not published before lazy transaction")
		}
		return tx(ctx, fn)
	}
	if err := v.UnlockExisting(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("transactions = %d, want validation and lazy", calls)
	}
	plain, err := v.Open(uid, testKind, sealed.SealedWith, sealed.Ciphertext)
	if err != nil || string(plain) != "current material" {
		t.Fatalf("successful unlock lost key: %v", err)
	}
	row, _ := st.getSecretByID(sid)
	if row.sealedWith != store.SealedWithMaster || !bytes.Equal(row.ciphertext, legacy) {
		t.Fatal("pre-write lazy failure changed fixture material")
	}
}

func TestLazyCommitUncertaintyEvictsAndReconciles(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	const password = "protected-password"
	if err := v.Unlock(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	tx := st.transaction(st)
	calls := 0
	v.transact = func(ctx context.Context, fn func(context.Context, Store) error) error {
		calls++
		if err := tx(ctx, fn); err != nil {
			return err
		}
		if calls == 2 {
			return &CommitError{Err: errors.New("ack lost")}
		}
		if calls == 3 && v.Unlocked(uid) {
			t.Fatal("reconciliation retained key")
		}
		return nil
	}
	if err := v.UnlockExisting(context.Background(), uid, password); !errors.Is(err, ErrUncertainOutcome) {
		t.Fatalf("uncertain lazy commit reported success: %v", err)
	}
	if calls != 3 || v.Unlocked(uid) {
		t.Fatalf("reconciliation calls=%d unlocked=%v", calls, v.Unlocked(uid))
	}
}

func TestLazyConversionRefusesChangedEpoch(t *testing.T) {
	v, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	const password = "protected-password"
	if err := v.Unlock(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	legacy, err := master.Seal([]byte("unchanged legacy material"))
	if err != nil {
		t.Fatal(err)
	}
	sid := st.putSecret(uid, testKind, legacy, store.SealedWithMaster)
	tx := st.transaction(st)
	calls := 0
	v.transact = func(ctx context.Context, fn func(context.Context, Store) error) error {
		calls++
		if calls == 2 {
			// Simulate a different process advancing the durable epoch between phases.
			state := st.states[uid]
			state.VaultEpoch++
			st.states[uid] = state
		}
		return tx(ctx, fn)
	}
	if err := v.UnlockExisting(context.Background(), uid, password); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changed epoch: %v", err)
	}
	row, _ := st.getSecretByID(sid)
	if v.Unlocked(uid) || row.sealedWith != store.SealedWithMaster || !bytes.Equal(row.ciphertext, legacy) {
		t.Fatal("stale lazy phase used retired key or changed material")
	}
}

func TestLifecycleTransactionDeadlines(t *testing.T) {
	for _, shorter := range []bool{false, true} {
		t.Run(map[bool]string{false: "maximum", true: "shorter caller"}[shorter], func(t *testing.T) {
			v, _, st := newTestVaultWithStore(t)
			uid := uuid.New()
			ctx := context.Background()
			var callerDeadline time.Time
			if shorter {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				callerDeadline, _ = ctx.Deadline()
			}
			tx := st.transaction(st)
			calls := 0
			v.transact = func(ctx context.Context, fn func(context.Context, Store) error) error {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Minute {
					t.Fatal("transaction deadline exceeds lifecycle maximum")
				}
				if shorter && !deadline.Equal(callerDeadline) {
					t.Fatal("shorter caller deadline was replaced")
				}
				return tx(ctx, fn)
			}
			const password = "protected-password"
			if err := v.Unlock(ctx, uid, password); err != nil {
				t.Fatal(err)
			}
			if err := v.UnlockExisting(ctx, uid, password); err != nil {
				t.Fatal(err)
			}
			if err := v.Disable(ctx, uid); err != nil {
				t.Fatal(err)
			}
			if err := v.Enable(ctx, uid, password); err != nil {
				t.Fatal(err)
			}
			if calls != 5 {
				t.Fatalf("transactions=%d, want create, validation, lazy, disable, enable", calls)
			}
		})
	}
}

type claimObservationStore struct {
	*fakeVaultStore
	observe func()
	err     error
}

func (s *claimObservationStore) ClaimVaultLockNotice(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	s.observe()
	return id, s.err
}

func TestLockClaimsNoticeBeforeEvictionEvenOnFailure(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "claim failure"}[fails], func(t *testing.T) {
			v, _, st := newTestVaultWithStore(t)
			uid := uuid.New()
			if err := v.Unlock(context.Background(), uid, "protected-password"); err != nil {
				t.Fatal(err)
			}
			key := v.cache[uid].dek
			called := false
			claim := &claimObservationStore{fakeVaultStore: st, observe: func() {
				called = true
				// Unlocked takes cache mu: this also guards against I/O under that mutex.
				if !v.Unlocked(uid) {
					t.Error("notice claim observed evicted key")
				}
				if v.lifecycleMutex(uid).TryLock() {
					v.lifecycleMutex(uid).Unlock()
					t.Error("claim ran outside lifecycle")
				}
			}}
			if fails {
				claim.err = errors.New("claim failed")
			}
			v.q = claim
			v.transact = st.transaction(claim)
			if err := v.LockAndAcknowledge(context.Background(), uid); err != nil {
				t.Fatal(err)
			}
			if !called || v.Unlocked(uid) || !bytes.Equal(key, make([]byte, len(key))) {
				t.Fatal("claim or eviction was skipped")
			}
		})
	}
}

func TestNilOpenAtEpochFailsClosedWithBoundReader(t *testing.T) {
	var v *Vault
	ctx := WithStateReader(context.Background(), newFakeVaultStore())
	if _, err := v.OpenAtEpoch(ctx, uuid.New(), testKind, store.SealedWithMaster, nil, 0); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil OpenAtEpoch: %v", err)
	}
}
