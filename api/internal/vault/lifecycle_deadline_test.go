package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type deadlineNoticeStore struct {
	*fakeVaultStore
	observe func(context.Context)
}

func (s *deadlineNoticeStore) ClaimVaultLockNotice(ctx context.Context, _ uuid.UUID) (uuid.UUID, error) {
	s.observe(ctx)
	return uuid.Nil, errors.New("fixture notice unavailable")
}

func TestManualLockBoundsNoticeDeadline(t *testing.T) {
	for _, shorter := range []bool{false, true} {
		t.Run(map[bool]string{false: "background", true: "shorter caller"}[shorter], func(t *testing.T) {
			v, _, st := newTestVaultWithStore(t)
			id := uuid.New()
			if err := v.Unlock(context.Background(), id, "deadline-fixture-password"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var caller time.Time
			if shorter {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
				caller, _ = ctx.Deadline()
			}
			called := false
			v.q = &deadlineNoticeStore{fakeVaultStore: st, observe: func(ctx context.Context) {
				called = true
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Error("manual notice claim lacks bounded deadline")
				}
				if shorter && !deadline.Equal(caller) {
					t.Error("manual notice claim replaced shorter caller deadline")
				}
				if !v.Unlocked(id) {
					t.Error("manual lock evicted before notice attempt")
				}
			}}
			if err := v.LockAndAcknowledge(ctx, id); err != nil {
				t.Fatal(err)
			}
			if !called || v.Unlocked(id) {
				t.Fatal("notice failure skipped key eviction")
			}
		})
	}
}

type deadlineStatusStore struct {
	*fakeVaultStore
	observe func(context.Context)
}

func (s *deadlineStatusStore) GetVaultPolicy(ctx context.Context) (string, error) {
	s.observe(ctx)
	return s.fakeVaultStore.GetVaultPolicy(ctx)
}

func TestStatusBoundsLifecycleReads(t *testing.T) {
	v, _, st := newTestVaultWithStore(t)
	called := false
	v.q = &deadlineStatusStore{fakeVaultStore: st, observe: func(ctx context.Context) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Minute {
			t.Error("status can indefinitely hold lifecycle boundary")
		}
	}}
	if _, err := v.Status(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("status did not reach policy lookup")
	}
}
