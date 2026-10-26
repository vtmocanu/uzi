package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestVaultModeClaimAdmission(t *testing.T) {
	for _, mode := range []string{store.VaultModeUnprotected, store.VaultModeProtected, "invalid"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			uid := uuid.New()
			m := newMemVaultStore()
			m.states[uid] = store.LockVaultAuthStateRow{VaultMode: mode}
			box := newBox(t)
			v := newMemoryVault(box, m)
			if v.Unlocked(uid) {
				t.Fatal("fixture unexpectedly has a cached key")
			}
			fs := &fakeStore{claimErr: pgx.ErrNoRows, chatClaimErr: pgx.ErrNoRows}
			svc := New(fs, box, testParams())
			svc.SetVault(v)
			wkr := store.Worker{ID: uuid.New(), UserID: uid}
			if payload, err := svc.Claim(ctx, wkr, nil); err != nil || payload != nil {
				t.Fatalf("Claim: payload=%v err=%v", payload, err)
			}
			if payload, err := svc.ClaimChat(ctx, wkr); err != nil || payload != nil {
				t.Fatalf("ClaimChat: payload=%v err=%v", payload, err)
			}
			wantQuery := mode == store.VaultModeUnprotected
			if (fs.claimParams != nil) != wantQuery || (fs.chatClaimParams != nil) != wantQuery {
				t.Fatalf("claim queried=%v chat queried=%v want=%v", fs.claimParams != nil, fs.chatClaimParams != nil, wantQuery)
			}
		})
	}
}

func TestVaultModeHealthAdmission(t *testing.T) {
	for _, mode := range []string{store.VaultModeUnprotected, store.VaultModeProtected} {
		t.Run(mode, func(t *testing.T) {
			r := queuedRunPastThreshold()
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())
			m := newMemVaultStore()
			m.states[r.UserID] = store.LockVaultAuthStateRow{VaultMode: mode}
			svc.SetVault(newMemoryVault(newBox(t), m))
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID).HealthReason.String
			if (got == reasonVaultLocked) != (mode == store.VaultModeProtected) {
				t.Fatalf("mode=%s health reason=%s", mode, got)
			}
		})
	}
}

// modeParkStore records the users fence before accepting the existing park.
// It deliberately does not provide a secret advisory or vault lifecycle seam.
type modeParkStore struct {
	*fakeStore
	mode    string
	lockErr error
	locked  bool
}

func (f *modeParkStore) LockUserVaultStateForMutation(ctx context.Context, _ uuid.UUID) (store.LockUserVaultStateRow, error) {
	f.locked = true
	if f.lockErr != nil {
		return store.LockUserVaultStateRow{}, f.lockErr
	}
	return store.LockUserVaultStateRow{VaultMode: f.mode}, ctx.Err()
}
func (f *modeParkStore) SetRunRecoveryWait(ctx context.Context, p store.SetRunRecoveryWaitParams) (int64, error) {
	if !f.locked {
		return 0, errors.New("park preceded users fence")
	}
	return f.fakeStore.SetRunRecoveryWait(ctx, p)
}

func TestVaultModeLateRecoveryPark(t *testing.T) {
	for _, tc := range []struct {
		name, mode          string
		unlocked, wantTyped bool
		lockErr             error
		wantErr             bool
	}{
		{name: "off generic", mode: store.VaultModeUnprotected},
		{name: "protected locked", mode: store.VaultModeProtected, wantTyped: true},
		{name: "protected historical after unlock", mode: store.VaultModeProtected, unlocked: true, wantTyped: true},
		{name: "state read failed", lockErr: errors.New("state unavailable"), wantErr: true},
		{name: "invalid mode", mode: "invalid", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := recoveryRun(2)
			base, svc, wkr := limitParkFixture(t, run)
			base.setRecoveryWaitRows = 1
			q := &modeParkStore{fakeStore: base, mode: tc.mode, lockErr: tc.lockErr}
			svc.q = q
			if tc.unlocked {
				svc.SetVault(unlockedVault(t, run.UserID, newBox(t)))
			}
			_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
				State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseVaultLocked),
			})
			if tc.wantErr {
				if err == nil || applied || base.setRecoveryWait != nil {
					t.Fatalf("failed fence mutated park: applied=%v err=%v params=%v", applied, err, base.setRecoveryWait)
				}
				return
			}
			if err != nil || !applied || base.setRecoveryWait == nil {
				t.Fatalf("park: applied=%v err=%v", applied, err)
			}
			p := base.setRecoveryWait
			if p.RecoveryCause.Valid != tc.wantTyped || (p.RecoveryCause.Valid && p.RecoveryCause.String != recoveryCauseVaultLocked) {
				t.Fatalf("cause=%v want typed=%v", p.RecoveryCause, tc.wantTyped)
			}
			if p.ID != run.ID || p.WorkerID != pgconv.UUID(wkr.ID) || p.ClaimGeneration.Valid {
				t.Fatal("park changed run/worker or invented a generation")
			}
			lo := parkNow.Add(4*time.Minute + recoveryParkJitterMin)
			hi := parkNow.Add(4*time.Minute + recoveryParkJitterMax)
			if !p.RetryNotBefore.Valid || p.RetryNotBefore.Time.Before(lo) || p.RetryNotBefore.Time.After(hi) {
				t.Fatalf("generic backoff changed: %v", p.RetryNotBefore)
			}
		})
	}
}

var _ interface {
	LockUserVaultStateForMutation(context.Context, uuid.UUID) (store.LockUserVaultStateRow, error)
	SetRunRecoveryWait(context.Context, store.SetRunRecoveryWaitParams) (int64, error)
} = (*modeParkStore)(nil)
