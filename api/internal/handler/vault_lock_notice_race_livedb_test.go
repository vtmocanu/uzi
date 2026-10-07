package handler

import (
	"context"
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

// All queries except the single refresh list run against Postgres, including
// unlock's clear, the manual handler's pre-ack, and reconciler eligibility/claim.
type vaultNoticeListBarrier struct {
	*store.Queries
	entered, release chan struct{}
}

func (s *vaultNoticeListBarrier) ListMasterSealedSecrets(ctx context.Context, uid uuid.UUID) ([]store.ListMasterSealedSecretsRow, error) {
	if s.entered == nil {
		return s.Queries.ListMasterSealedSecrets(ctx, uid)
	}
	close(s.entered)
	select {
	case <-s.release:
		return s.Queries.ListMasterSealedSecrets(ctx, uid)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type vaultNoticeRaceSettings struct{}

func (vaultNoticeRaceSettings) UziLabel(context.Context) (string, error) {
	return "PRD", nil
}

func (vaultNoticeRaceSettings) PublicBaseURL(context.Context) (string, error) {
	return "", nil
}

// PRD #890 D6 / success criterion 4: an older unlock finishing after a deliberate
// lock must preserve the pre-ack and suppress the real reconciler's inbox notice.
// The handler package is enumerated by e2e/run-store-it.sh's LiveDB sweep.
func TestVaultLockOlderUnlockPreservesNoticeLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h := e.handler()
	barrier := &vaultNoticeListBarrier{Queries: h.q}
	h.vault = vault.New(e.box, barrier)
	if err := h.vault.Unlock(e.ctx, e.user, recoveryUnlockPassword); err != nil {
		t.Fatal(err)
	}
	mustExecT(e.ctx, t, e.pool, `UPDATE users SET slack_notify=true,
		slack_resolved_id=$2, slack_link_confirmed_at=now() WHERE id=$1`,
		e.user, "U"+e.user.String())
	mustExecT(e.ctx, t, e.pool, `UPDATE runs SET status='queued' WHERE id=$1`, e.run)

	marker := func() store.UserVault {
		t.Helper()
		row, err := h.q.GetUserVault(e.ctx, e.user)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
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
	rows, err := h.q.ListUsersNeedingVaultLockNotice(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	eligible := false
	for _, row := range rows {
		if row.ID == e.user {
			eligible = row.PendingRuns == 1
		}
	}
	if !eligible || !h.vault.Unlocked(e.user) || marker().LockNotifiedAt.Valid {
		t.Fatal("fixture must be eligible with pending work, unlocked, and unacknowledged")
	}
	reconciler := schedsvc.NewVaultLockReconciler(h.q, h.vault,
		notifysvc.New(h.q, nil, 0, nil), vaultNoticeRaceSettings{}, nil)
	reconciler.Reconcile(e.ctx)
	if got := inbox(); got != 0 {
		t.Fatalf("initial unlocked owner received %d notices", got)
	}
	if marker().LockNotifiedAt.Valid {
		t.Fatal("unlocked suppression must not claim the notice")
	}

	// Enable the barrier only after initialization and the unlocked control.
	barrier.entered = make(chan struct{})
	barrier.release = make(chan struct{})
	ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
	done := make(chan *httptest.ResponseRecorder, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	joined := false
	t.Cleanup(func() {
		cancel()
		release()
		if !joined {
			<-done
		}
	})
	go func() { done <- vaultRecoveryUnlock(h, ctx, e.user, recoveryUnlockPassword) }()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("older unlock did not reach list barrier")
	}
	if !h.vault.Unlocked(e.user) {
		t.Fatal("older unlock must cache the DEK before reaching the barrier")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/vault/lock", nil)
	req = req.WithContext(mw.ContextWithUser(ctx, store.User{ID: e.user, IsActive: true}))
	lockResponse := httptest.NewRecorder()
	h.VaultLock(lockResponse, req)
	if lockResponse.Code != http.StatusNoContent {
		t.Fatalf("manual lock=%d body=%s", lockResponse.Code, lockResponse.Body.String())
	}
	preack := marker().LockNotifiedAt
	if h.vault.Unlocked(e.user) || !preack.Valid {
		t.Fatal("manual lock must evict the key and persist its pre-ack before releasing older unlock")
	}

	release()
	unlockResponse := <-done
	joined = true
	if unlockResponse.Code != http.StatusNoContent {
		t.Fatalf("older unlock=%d body=%s", unlockResponse.Code, unlockResponse.Body.String())
	}
	if h.vault.Unlocked(e.user) {
		t.Error("older unlock must leave the manually locked vault locked")
	}
	after := marker().LockNotifiedAt
	if !after.Valid || !after.Time.Equal(preack.Time) {
		t.Errorf("older unlock erased manual lock pre-ack: before=%v after=%v", preack, after)
	}
	reconciler.Reconcile(e.ctx)
	afterRace := inbox()
	if afterRace != 0 {
		t.Errorf("deliberately locked owner received %d unwanted vault_locked inbox notices", afterRace)
	}

	// Positive control: the same locked owner is deliverable when explicitly re-armed.
	if err := h.q.ClearVaultLockNotice(e.ctx, e.user); err != nil {
		t.Fatal(err)
	}
	reconciler.Reconcile(e.ctx)
	if got := inbox(); got != afterRace+1 {
		t.Errorf("positive control inbox=%d, want %d after re-arming locked owner", got, afterRace+1)
	}
	if !marker().LockNotifiedAt.Valid {
		t.Error("positive control did not claim the lock notice")
	}
}
