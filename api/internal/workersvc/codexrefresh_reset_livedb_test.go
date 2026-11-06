package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// TestCodexRecoverySlotWriteAfterVaultResetFailsClosedLiveDB (PRD #3989): a refresh
// that held a lease before an admin account reset must not resurrect the deleted
// account. vault.Reset deletes the codex_provider_account row, so the recovery-slot
// write finds nothing to update: it reports errCodexRecoveryFenceLost, writes no
// bytes, and recreates no row. Skipped without UZI_TEST_DATABASE_URL; run via
// ./e2e/run-store-it.sh.
func TestCodexRecoverySlotWriteAfterVaultResetFailsClosedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("access-new")}}
	f := newRefreshFixture(t, env, fake)
	enrollCodexProtectedVault(t, env, f.svc.vlt, f.userID, codexVaultLockedTestPassword)

	// The in-flight refresh owns the live lease when the reset lands.
	op := uuid.New()
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: f.accountID, UserID: f.userID, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}

	if err := f.svc.vlt.Reset(env.ctx, f.userID, noopResetCallback); err != nil {
		t.Fatalf("vault.Reset: %v", err)
	}
	if n := env.countProviderAccounts(t, f.userID); n != 0 {
		t.Fatalf("accounts after reset = %d, want 0", n)
	}

	params := store.SetCodexRecoverySlotParams{Gen: 0, ID: f.accountID, UserID: f.userID, Op: op}
	err := f.svc.writeCodexRecoverySlotOnce(env.ctx, env.q, params, []byte("rotated login that must not persist"), 5*time.Second)
	if !errors.Is(err, errCodexRecoveryFenceLost) {
		t.Fatalf("recovery write after reset = %v, want errCodexRecoveryFenceLost", err)
	}
	if n := env.countProviderAccounts(t, f.userID); n != 0 {
		t.Fatalf("accounts after the refused write = %d, want 0 (no row recreated)", n)
	}
	if _, err := env.q.GetCodexProviderAccountByID(env.ctx, store.GetCodexProviderAccountByIDParams{UserID: f.userID, ID: f.accountID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted account lookup = %v, want pgx.ErrNoRows", err)
	}
}

func noopResetCallback(_ context.Context, _ vault.BrowserStore) error { return nil }
