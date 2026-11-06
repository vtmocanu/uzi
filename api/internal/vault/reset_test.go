package vault

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const resetFixturePassword = "reset-fixture-password"

// resetFixture returns a vault whose owner has a created wrapper, one sealed
// secret, one Codex account, and an unlocked cache entry.
func resetFixture(t *testing.T) (*Vault, *fakeVaultStore, uuid.UUID, context.Context) {
	t.Helper()
	v, _, st := newTestVaultWithStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	uid := uuid.New()
	if err := v.Unlock(ctx, uid, resetFixturePassword); err != nil {
		t.Fatal(err)
	}
	material, err := v.Seal(ctx, uid, testKind, []byte("to be deleted"), SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	st.putSecret(uid, testKind, material.Ciphertext, material.SealedWith)
	st.codexAccounts = map[uuid.UUID]int{uid: 1}
	if !v.Unlocked(uid) {
		t.Fatal("fixture owner should be unlocked")
	}
	return v, st, uid, ctx
}

func noopReset(context.Context, BrowserStore) error { return nil }

func TestResetOrdersCallbackBeforeDeletesBeforeEpochAdvance(t *testing.T) {
	v, st, uid, ctx := resetFixture(t)
	before, err := v.State(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	st.resetCalls()
	err = v.Reset(ctx, uid, func(context.Context, BrowserStore) error {
		st.record("fn")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"LockSecretMutation", "LockOwnerWorkersForReset", "LockOwnerRunsForReset", "LockVaultAuthState",
		"fn",
		"DeleteAllUserSecretsForOwner", "DeleteAllCodexProviderAccountsForOwner", "DeleteUserVault", "ClearSidebarCodexAccounts",
		"AdvanceUserVaultMode",
	}
	if got := st.callLog(); !slices.Equal(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	after, err := v.State(ctx, uid)
	if err != nil || after.VaultMode != store.VaultModeProtected || after.VaultEpoch != before.VaultEpoch+1 {
		t.Fatalf("state after reset = %+v, %v; want protected epoch %d", after, err, before.VaultEpoch+1)
	}
	if exists, err := v.Exists(ctx, uid); err != nil || exists {
		t.Fatalf("wrapper still exists after reset: %v, %v", exists, err)
	}
	if _, ok := st.getSecret(uid, testKind); ok {
		t.Fatal("secret survived reset")
	}
	if st.codexAccounts[uid] != 0 {
		t.Fatal("codex account survived reset")
	}
}

func TestResetCallbackErrorDeletesAndAdvancesNothing(t *testing.T) {
	v, st, uid, ctx := resetFixture(t)
	before, _ := v.State(ctx, uid)
	boom := errors.New("callback refused")
	st.resetCalls()
	err := v.Reset(ctx, uid, func(context.Context, BrowserStore) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Reset error = %v, want the callback error", err)
	}
	for _, c := range st.callLog() {
		if c == "DeleteAllUserSecretsForOwner" || c == "DeleteAllCodexProviderAccountsForOwner" ||
			c == "DeleteUserVault" || c == "ClearSidebarCodexAccounts" || c == "AdvanceUserVaultMode" {
			t.Fatalf("%s ran after a failed callback: %v", c, st.callLog())
		}
	}
	if _, ok := st.getSecret(uid, testKind); !ok || st.codexAccounts[uid] != 1 {
		t.Fatal("material deleted despite callback failure")
	}
	after, _ := v.State(ctx, uid)
	if after != before {
		t.Fatalf("state changed on callback failure: %+v -> %+v", before, after)
	}
}

func TestResetEvictsCachedKeyOnSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		v, _, uid, ctx := resetFixture(t)
		if err := v.Reset(ctx, uid, noopReset); err != nil {
			t.Fatal(err)
		}
		if v.Unlocked(uid) {
			t.Fatal("cached DEK survived a successful reset")
		}
	})
	t.Run("callback failure", func(t *testing.T) {
		v, _, uid, ctx := resetFixture(t)
		if err := v.Reset(ctx, uid, func(context.Context, BrowserStore) error { return errors.New("no") }); err == nil {
			t.Fatal("expected error")
		}
		if v.Unlocked(uid) {
			t.Fatal("cached DEK survived a failed reset attempt")
		}
	})
}

func TestResetIsIdempotentOnEmptyOwner(t *testing.T) {
	v, _, _, ctx := resetFixture(t)
	empty := uuid.New() // no wrapper, no secrets, no accounts
	for i := 1; i <= 2; i++ {
		if err := v.Reset(ctx, empty, noopReset); err != nil {
			t.Fatalf("reset #%d: %v", i, err)
		}
		state, err := v.State(ctx, empty)
		if err != nil || state.VaultMode != store.VaultModeProtected || state.VaultEpoch != int64(i) {
			t.Fatalf("after reset #%d: %+v, %v", i, state, err)
		}
	}
}

func TestResetRefusesAtEpochExhaustion(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	uid := uuid.New()
	st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: math.MaxInt64}
	err := v.Reset(context.Background(), uid, noopReset)
	if !errors.Is(err, ErrEpochExhausted) {
		t.Fatalf("Reset error = %v, want ErrEpochExhausted", err)
	}
	for _, c := range st.callLog() {
		if c == "AdvanceUserVaultMode" {
			t.Fatal("epoch advanced past MaxInt64")
		}
	}
}

func TestResetRequiresResetStore(t *testing.T) {
	_, master, st := newTestVaultWithStore(t)
	// A Store that is not a resetStore (no embedded concrete type exposes the methods).
	narrow := struct{ Store }{st}
	v := New(master, st, func(ctx context.Context, fn func(context.Context, Store) error) error {
		return fn(ctx, narrow)
	})
	if err := v.Reset(context.Background(), uuid.New(), noopReset); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Reset error = %v, want ErrNotConfigured", err)
	}
}

func TestResetSerializesWithDelayedUnlock(t *testing.T) {
	_, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	seen := make(chan struct{}, 1)
	v := New(master, st, st.transaction(st), contentionObserver(uid, seen))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := v.Unlock(ctx, uid, resetFixturePassword); err != nil {
		t.Fatal(err)
	}
	v.Lock(uid)
	before, err := v.State(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	v.transact = postCommitBarrier(st.transaction(st), entered, release)
	unlockDone, resetDone := make(chan error, 1), make(chan error, 1)
	unlockJoined, resetStarted, resetJoined := false, false, false
	defer func() {
		unblock()
		cancel()
		if !unlockJoined {
			contentionJoin(t, unlockDone)
		}
		if resetStarted && !resetJoined {
			contentionJoin(t, resetDone)
		}
	}()
	go func() { unlockDone <- v.UnlockExisting(ctx, uid, resetFixturePassword) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("unlock never reached the post-commit barrier")
	}
	resetStarted = true
	go func() { resetDone <- v.Reset(ctx, uid, noopReset) }()
	select {
	case <-seen:
	case err := <-resetDone:
		resetJoined = true
		t.Fatalf("reset completed while unlock held the lifecycle mutex: %v", err)
	case <-ctx.Done():
		t.Fatal("reset did not contend on the delayed unlock")
	}
	if current, err := v.State(ctx, uid); err != nil || current != before {
		t.Fatalf("reset advanced state before unlock released: %+v, %v", current, err)
	}
	unblock()
	err = <-unlockDone
	unlockJoined = true
	if err != nil {
		t.Fatal(err)
	}
	err = <-resetDone
	resetJoined = true
	if err != nil {
		t.Fatal(err)
	}
	after, err := v.State(ctx, uid)
	if err != nil || after.VaultEpoch != before.VaultEpoch+1 {
		t.Fatalf("epoch after unlock+reset = %+v, %v; want %d", after, err, before.VaultEpoch+1)
	}
	// The key unlock published at the old epoch must not survive the reset.
	if v.Unlocked(uid) {
		t.Fatal("reset left the unlock-published key cached")
	}
}

// wrapTransact replaces v.transact with fn(calls, base) so a test controls what
// each attempt returns while the base fixture transaction stays available.
func wrapTransact(v *Vault, st *fakeVaultStore, f func(call int, base Transact) Transact) *int {
	calls := new(int)
	base := st.transaction(st)
	var mu sync.Mutex
	v.transact = func(ctx context.Context, fn func(context.Context, Store) error) error {
		mu.Lock()
		*calls++
		n := *calls
		mu.Unlock()
		return f(n, base)(ctx, fn)
	}
	return calls
}

func deadlock() error {
	return fmt.Errorf("cascade: %w", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"})
}

func TestResetCommitErrorIsUncertainEvictsAndIsNeverRetried(t *testing.T) {
	for name, cause := range map[string]error{"plain": errors.New("lost commit ack"), "deadlock-shaped": deadlock()} {
		t.Run(name, func(t *testing.T) {
			v, st, uid, ctx := resetFixture(t)
			calls := wrapTransact(v, st, func(call int, base Transact) Transact {
				return func(ctx context.Context, fn func(context.Context, Store) error) error {
					err := base(ctx, fn)
					if call == 1 && err == nil {
						return &CommitError{Err: cause}
					}
					return err
				}
			})
			err := v.Reset(ctx, uid, noopReset)
			if !errors.Is(err, ErrUncertainOutcome) {
				t.Fatalf("Reset error = %v, want ErrUncertainOutcome", err)
			}
			if *calls != 2 { // the reset itself plus one reconciliation
				t.Fatalf("transact calls = %d, want 2 (reset once, reconcile once)", *calls)
			}
			if v.Unlocked(uid) {
				t.Fatal("cached DEK retained after an uncertain reset")
			}
		})
	}
}

func TestResetRetriesDeadlockExactlyOnce(t *testing.T) {
	t.Run("recovers on the second attempt", func(t *testing.T) {
		v, st, uid, ctx := resetFixture(t)
		calls := wrapTransact(v, st, func(call int, base Transact) Transact {
			if call == 1 {
				return func(context.Context, func(context.Context, Store) error) error { return deadlock() }
			}
			return base
		})
		runs := 0
		err := v.Reset(ctx, uid, func(context.Context, BrowserStore) error { runs++; return nil })
		if err != nil {
			t.Fatalf("Reset error = %v, want success after one retry", err)
		}
		if *calls != 2 || runs != 1 {
			t.Fatalf("transact calls = %d, callback runs = %d; want 2 and 1", *calls, runs)
		}
	})
	t.Run("gives up after two deadlocks", func(t *testing.T) {
		v, st, uid, ctx := resetFixture(t)
		calls := wrapTransact(v, st, func(int, Transact) Transact {
			return func(context.Context, func(context.Context, Store) error) error { return deadlock() }
		})
		err := v.Reset(ctx, uid, noopReset)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
			t.Fatalf("Reset error = %v, want the 40P01 error", err)
		}
		if *calls != 2 {
			t.Fatalf("transact calls = %d, want exactly 2", *calls)
		}
	})
	t.Run("other errors are not retried", func(t *testing.T) {
		v, st, uid, ctx := resetFixture(t)
		other := errors.New("serialization-ish")
		calls := wrapTransact(v, st, func(int, Transact) Transact {
			return func(context.Context, func(context.Context, Store) error) error { return other }
		})
		if err := v.Reset(ctx, uid, noopReset); !errors.Is(err, other) || *calls != 1 {
			t.Fatalf("err = %v, calls = %d; want the error after 1 call", err, *calls)
		}
	})
}
