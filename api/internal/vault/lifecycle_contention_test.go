package vault

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// postCommitBarrier waits after the fixture transaction has returned, releasing
// txMu before the ACK barrier. Only the first successful transaction is delayed.
func postCommitBarrier(tx Transact, entered, release chan struct{}) Transact {
	var calls atomic.Int32
	return func(ctx context.Context, fn func(context.Context, Store) error) error {
		err := tx(ctx, fn)
		if err == nil && calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return &CommitError{Err: ctx.Err()}
			}
		}
		return err
	}
}

func contentionJoin(t *testing.T, done <-chan error) {
	t.Helper()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrUncertainOutcome) {
		t.Errorf("join lifecycle operation during cleanup: %v", err)
	}
}

func contentionObserver(uid uuid.UUID, seen chan struct{}) Option {
	return WithLifecycleWaitObserverForTest(func(id uuid.UUID) {
		if id == uid {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
	})
}

func TestDelayedUnlockPublicationSerializesDisableEnableABA(t *testing.T) {
	_, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	seen := make(chan struct{}, 1)
	v := New(master, st, st.transaction(st), contentionObserver(uid, seen))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const password = "contention-fixture-password"
	if err := v.Unlock(ctx, uid, password); err != nil {
		t.Fatal(err)
	}
	material, err := v.Seal(ctx, uid, testKind, []byte("independent ABA material"), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	sid := st.putSecret(uid, testKind, material.Ciphertext, material.SealedWith)
	v.Lock(uid)
	before, err := v.State(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := st.GetUserVault(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := unwrapKey(uid, password, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	defer wipe(keyA)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	v.transact = postCommitBarrier(st.transaction(st), entered, release)
	unlockDone, cycleDone := make(chan error, 1), make(chan error, 1)
	unlockJoined, cycleStarted, cycleJoined := false, false, false
	defer func() {
		unblock()
		cancel()
		if !unlockJoined {
			contentionJoin(t, unlockDone)
		}
		if cycleStarted && !cycleJoined {
			contentionJoin(t, cycleDone)
		}
	}()
	go func() { unlockDone <- v.UnlockExisting(ctx, uid, password) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("unlock never reached post-commit barrier")
	}
	if v.lifecycleMutex(uid).TryLock() {
		v.lifecycleMutex(uid).Unlock()
		t.Fatal("unlock released lifecycle before private key publication")
	}
	cycleStarted = true
	go func() {
		err := v.Disable(ctx, uid)
		if err == nil {
			err = v.Enable(ctx, uid, password)
		}
		cycleDone <- err
	}()
	select {
	case <-seen:
	case err := <-cycleDone:
		cycleJoined = true
		t.Fatalf("disable/enable completed before observed contention: %v", err)
	case <-ctx.Done():
		t.Fatal("disable did not contend on delayed unlock")
	}
	current, err := v.State(ctx, uid)
	if err != nil || current != before || v.Unlocked(uid) {
		t.Fatal("delayed unlock allowed premature mode/epoch/cache change")
	}
	unblock()
	err = <-unlockDone
	unlockJoined = true
	if err != nil {
		t.Fatal(err)
	}
	err = <-cycleDone
	cycleJoined = true
	if err != nil {
		t.Fatal(err)
	}
	current, err = v.State(ctx, uid)
	if err != nil || current.VaultMode != store.VaultModeProtected || current.VaultEpoch != before.VaultEpoch+2 {
		t.Fatal("ABA did not advance current epoch twice")
	}
	v.mu.RLock()
	entry := v.cache[uid]
	currentEpoch, sameKey := entry.epoch, bytes.Equal(entry.dek, keyA)
	v.mu.RUnlock()
	if currentEpoch != current.VaultEpoch || sameKey {
		t.Fatal("retired key A published over current key B")
	}
	row, _ := st.getSecretByID(sid)
	plain, err := v.OpenAtEpoch(ctx, uid, testKind, row.sealedWith, row.ciphertext, current.VaultEpoch)
	if err != nil || string(plain) != "independent ABA material" {
		t.Fatal("current key B cannot open material")
	}
	if _, err := v.OpenAtEpoch(ctx, uid, testKind, material.SealedWith, material.Ciphertext, material.Epoch); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatal("retired epoch still admitted")
	}
}

func TestExplicitLockContendsWithLifecyclePublication(t *testing.T) {
	for _, operation := range []string{"enable", "disable"} {
		for _, lock := range []string{"raw", "acknowledge"} {
			t.Run(operation+"/"+lock, func(t *testing.T) {
				_, master, st := newTestVaultWithStore(t)
				uid := uuid.New()
				seen := make(chan struct{}, 1)
				v := New(master, st, st.transaction(st), contentionObserver(uid, seen))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				const password = "publication-fixture-password"
				st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
				if operation == "disable" {
					if err := v.Enable(ctx, uid, password); err != nil {
						t.Fatal(err)
					}
				}
				material, err := v.Seal(ctx, uid, testKind, []byte("publication material"), SealForUse)
				if err != nil {
					t.Fatal(err)
				}
				sid := st.putSecret(uid, testKind, material.Ciphertext, material.SealedWith)
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				v.transact = postCommitBarrier(st.transaction(st), entered, release)
				lifecycleDone, lockDone := make(chan error, 1), make(chan error, 1)
				lifecycleJoined, lockStarted, lockJoined := false, false, false
				defer func() {
					unblock()
					cancel()
					if !lifecycleJoined {
						contentionJoin(t, lifecycleDone)
					}
					if lockStarted && !lockJoined {
						contentionJoin(t, lockDone)
					}
				}()
				go func() {
					if operation == "enable" {
						lifecycleDone <- v.Enable(ctx, uid, password)
					} else {
						lifecycleDone <- v.Disable(ctx, uid)
					}
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("lifecycle never reached post-commit barrier")
				}
				if operation == "enable" && v.Unlocked(uid) {
					t.Fatal("enable published private key before commit ACK")
				}
				if v.lifecycleMutex(uid).TryLock() {
					v.lifecycleMutex(uid).Unlock()
					t.Fatal("lifecycle ownership missing at private publication barrier")
				}
				lockStarted = true
				go func() {
					if lock == "raw" {
						v.Lock(uid)
						lockDone <- nil
					} else {
						lockDone <- v.LockAndAcknowledge(ctx, uid)
					}
				}()
				select {
				case <-seen:
				case err := <-lockDone:
					lockJoined = true
					t.Fatalf("explicit lock completed before observed contention: %v", err)
				case <-ctx.Done():
					t.Fatal("explicit lock did not contend on publication")
				}
				unblock()
				err = <-lifecycleDone
				lifecycleJoined = true
				if err != nil {
					t.Fatal(err)
				}
				err = <-lockDone
				lockJoined = true
				if err != nil {
					t.Fatal(err)
				}
				if v.Unlocked(uid) {
					t.Fatal("explicit lock failed to evict published key")
				}
				cold := New(master, st, st.transaction(st))
				if err := cold.UnlockExisting(ctx, uid, password); err != nil {
					t.Fatal(err)
				}
				row, _ := st.getSecretByID(sid)
				plain, err := cold.Open(uid, testKind, row.sealedWith, row.ciphertext)
				if err != nil || string(plain) != "publication material" {
					t.Fatal("restart cannot read committed material")
				}
				state, err := cold.State(ctx, uid)
				wantMode, wantEpoch, wantWrapper := store.VaultModeProtected, int64(1), true
				if operation == "disable" {
					wantMode, wantEpoch, wantWrapper = store.VaultModeUnprotected, 2, false
				}
				exists, existsErr := cold.Exists(ctx, uid)
				if err != nil || existsErr != nil || state.VaultMode != wantMode || state.VaultEpoch != wantEpoch || exists != wantWrapper {
					t.Fatal("inconsistent committed mode/epoch/wrapper")
				}
			})
		}
	}
}

func TestUncertainReconciliationHasFreshDeadlineAndTimeoutIsNotRollback(t *testing.T) {
	_, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	cause := errors.New("lost commit ACK")
	calls := 0
	v := New(master, st, func(ctx context.Context, fn func(context.Context, Store) error) error {
		calls++
		if ctx.Err() != nil {
			t.Fatal("reconciliation inherited canceled request")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second || time.Until(deadline) < 14*time.Second {
			t.Fatal("reconciliation did not use fresh 15 second deadline")
		}
		return context.DeadlineExceeded
	})
	v.put(uid, []byte("private-key"), 1)
	err := v.finishLifecycle(uid, &CommitError{Err: cause})
	var uncertain *UncertainOutcomeError
	if !errors.As(err, &uncertain) || !errors.Is(err, ErrUncertainOutcome) ||
		!errors.Is(uncertain.Reconciliation, context.DeadlineExceeded) || !errors.Is(err, cause) || calls != 1 || v.Unlocked(uid) {
		t.Fatal("reconciliation timeout was treated as rollback or retained a key")
	}
}
