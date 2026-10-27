package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// vaultNoticeClearBarrier blocks the unlock-time ClearVaultLockNotice once armed.
type vaultNoticeClearBarrier struct {
	*store.Queries
	entered, release chan struct{}
}

func (s *vaultNoticeClearBarrier) ClearVaultLockNotice(ctx context.Context, id uuid.UUID) error {
	if s.entered != nil {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Queries.ClearVaultLockNotice(ctx, id)
}

// A manual lock racing an unlock whose notice clear is still in SQL must not let
// the real reconciler send a vault_locked notice: neither while the lock waits for
// that clear, nor after the lock's pre-ack lands.
func TestVaultManualLockWaitingOnNoticeClearSendsNoNoticeLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h := e.handler()
	contended := make(chan struct{}, 1)
	observer := vault.WithLifecycleWaitObserverForTest(func(id uuid.UUID) {
		if id == e.user {
			select {
			case contended <- struct{}{}:
			default:
			}
		}
	})
	barrier := &vaultNoticeClearBarrier{Queries: h.q}
	h.vault = newWrappedPostgresVault(e.box, e.pool, barrier, func(q *store.Queries) vault.Store {
		return &vaultNoticeClearBarrier{Queries: q, entered: barrier.entered, release: barrier.release}
	}, observer)
	setVaultFixturePassword(t, e.pool, e.user, recoveryUnlockPassword)
	if err := h.vault.Unlock(e.ctx, e.user, recoveryUnlockPassword); err != nil {
		t.Fatal(err)
	}
	mustExecT(e.ctx, t, e.pool, `UPDATE users SET slack_notify=true,
		slack_resolved_id=$2, slack_link_confirmed_at=now() WHERE id=$1`,
		e.user, "U"+e.user.String())
	mustExecT(e.ctx, t, e.pool, `UPDATE runs SET status='queued' WHERE id=$1`, e.run)
	inbox := func() int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(e.ctx,
			`SELECT count(*) FROM notifications WHERE user_id=$1 AND kind='vault_locked'`,
			e.user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reconciler := schedsvc.NewVaultLockReconciler(h.q, h.vault,
		notifysvc.New(h.q, nil, 0, nil), vaultNoticeRaceSettings{}, nil)

	barrier.entered, barrier.release = make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	defer cancel()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	unlockJoined, lockStarted, lockJoined := false, false, false
	unlockDone := make(chan error, 1)
	lockDone := make(chan int, 1)
	defer func() {
		release()
		cancel()
		if !unlockJoined {
			if err := <-unlockDone; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("join unlock after releasing notice barrier: %v", err)
			}
		}
		if lockStarted && !lockJoined {
			<-lockDone
		}
	}()
	go func() { unlockDone <- h.vault.UnlockExisting(ctx, e.user, recoveryUnlockPassword) }()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("notice clear did not start")
	}
	lockStarted = true
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/vault/lock", nil)
		req = req.WithContext(mw.ContextWithUser(ctx, store.User{ID: e.user, IsActive: true}))
		rec := httptest.NewRecorder()
		h.VaultLock(rec, req)
		lockDone <- rec.Code
	}()
	select {
	case <-contended:
	case <-lockDone:
		lockJoined = true
		t.Fatal("manual lock completed before observed lifecycle contention")
	case <-ctx.Done():
		t.Fatal("manual lock did not contend on older unlock")
	}
	// Reconcile while unlock holds its notice-clear barrier.
	reconciler.Reconcile(e.ctx)
	if n := inbox(); n != 0 {
		t.Errorf("reconciler sent %d vault_locked notices while the manual lock waited", n)
	}

	release()
	unlockErr := <-unlockDone
	unlockJoined = true
	if err := unlockErr; err != nil {
		t.Fatal(err)
	}
	code := <-lockDone
	lockJoined = true
	if code != http.StatusNoContent {
		t.Fatalf("manual lock=%d", code)
	}
	if h.vault.Unlocked(e.user) {
		t.Fatal("manual lock left the vault unlocked")
	}
	reconciler.Reconcile(e.ctx)
	if n := inbox(); n != 0 {
		t.Errorf("reconciler sent %d vault_locked notices after the manual lock's pre-ack", n)
	}
}
