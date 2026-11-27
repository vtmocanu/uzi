package vault

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: TryStatus is the non-blocking Status the admin Capacity observation uses.

func TestTryStatusMatchesStatusWhenLifecycleIsFree(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, v *Vault, st *fakeVaultStore, uid uuid.UUID)
	}{
		{"unprotected", func(_ *testing.T, _ *Vault, st *fakeVaultStore, uid uuid.UUID) {
			st.states[uid] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
		}},
		{"protected-locked", func(*testing.T, *Vault, *fakeVaultStore, uuid.UUID) {}},
		{"protected-unlocked", func(t *testing.T, v *Vault, _ *fakeVaultStore, uid uuid.UUID) {
			if err := v.Unlock(ctx, uid, "try-status-fixture-password"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _, st := newTestVaultWithStore(t)
			uid := uuid.New()
			tc.setup(t, v, st, uid)
			want, err := v.Status(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			got, ok, err := v.TryStatus(ctx, uid)
			if err != nil || !ok {
				t.Fatalf("TryStatus = (%+v, %v, %v), want ok", got, ok, err)
			}
			if got != want {
				t.Fatalf("TryStatus = %+v, Status = %+v", got, want)
			}
			if tc.name == "protected-unlocked" && !got.Unlocked {
				t.Fatal("fixture should be unlocked")
			}
			// The boundary is released again.
			m := v.lifecycleMutex(uid)
			if !m.TryLock() {
				t.Fatal("TryStatus left the lifecycle mutex held")
			}
			m.Unlock()
		})
	}
}

func TestTryStatusNeverWaitsOnHeldLifecycle(t *testing.T) {
	_, master, st := newTestVaultWithStore(t)
	uid := uuid.New()
	var waits atomic.Int32
	v := New(master, st, st.transaction(st), WithLifecycleWaitObserverForTest(func(id uuid.UUID) {
		if id == uid {
			waits.Add(1)
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := v.lifecycleMutex(uid)
	m.Lock()
	held := true
	defer func() {
		if held {
			m.Unlock()
		}
	}()

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, ok, err := v.TryStatus(ctx, uid)
		done <- result{ok, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.ok {
			t.Fatalf("TryStatus on held lifecycle = ok %v, err %v; want ok=false, nil", r.ok, r.err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("TryStatus took %v on a held lifecycle", d)
		}
	case <-time.After(time.Second):
		t.Fatal("TryStatus blocked on a held lifecycle")
	}
	if n := waits.Load(); n != 0 {
		t.Fatalf("wait observer fired %d times for TryStatus; it must never be a waiter", n)
	}

	// Control: a Status against the held lock is a waiter, proving the observer is wired.
	statusDone := make(chan error, 1)
	go func() {
		_, err := v.Status(ctx, uid)
		statusDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for waits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Status never reached the wait observer")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-statusDone:
		t.Fatalf("Status completed while the lifecycle was held: %v", err)
	default:
	}
	m.Unlock()
	held = false
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatalf("Status after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Status did not complete after unlock")
	}
	if n := waits.Load(); n != 1 {
		t.Fatalf("wait observer count = %d, want 1 (the Status control only)", n)
	}
}
