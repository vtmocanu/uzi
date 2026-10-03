package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexRefreshRecoveryClearersLiveDB(t *testing.T) {
	for _, clearer := range []string{"commit", "promote", "relogin", "mismatch"} {
		t.Run(clearer, func(t *testing.T) {
			ctx, pool, q, user := codexLiveDB(t)
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", user); err != nil {
					t.Errorf("delete recovery fixture user %s: %v", user, err)
				}
			})
			acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{UserID: user, ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString(), SealedLogin: []byte("login"), SealedWith: store.SealedWithMaster})
			if err != nil {
				t.Fatal(err)
			}
			op := uuid.New()
			if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{ID: acc.ID, UserID: user, Op: op, Deadline: pgconv.Time(time.Now().Add(time.Minute))}); err != nil || n != 1 {
				t.Fatalf("lease=(%d,%v)", n, err)
			}
			params := store.SetCodexRecoverySlotParams{ID: acc.ID, UserID: user, Op: op, Sealed: []byte("opaque recovery"), RecoverySealedWith: pgconv.Text(store.SealedWithMaster), RecoveryCause: pgconv.Text("vault_locked")}
			if n, err := q.SetCodexRecoverySlot(ctx, params); err != nil || n != 1 {
				t.Fatalf("slot=(%d,%v)", n, err)
			}
			switch clearer {
			case "commit":
				// Commit requires an in-progress lease. Cause metadata belongs only to quarantine.
				// Retain the slot with NULL cause while restoring that valid lease fixture.
				mustExec(ctx, t, pool, "UPDATE codex_provider_account SET recovery_cause=NULL, coord_state='in_progress' WHERE id=$1", acc.ID)
				_, err = q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{ID: acc.ID, UserID: user, Op: op, Sealed: []byte("new"), SealedWith: store.SealedWithMaster})
			case "promote":
				_, err = q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{ID: acc.ID, UserID: user, Sealed: []byte("new"), SealedWith: store.SealedWithMaster})
			case "relogin":
				var n int64
				n, err = q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{ID: acc.ID, UserID: user, Sealed: []byte("new"), SealedWith: store.SealedWithMaster})
				if err == nil && n != 1 {
					t.Fatalf("relogin rows=%d", n)
				}
			case "mismatch":
				var row store.ClearMismatchedCodexRecoveryAndMarkIntentsRow
				row, err = q.ClearMismatchedCodexRecoveryAndMarkIntents(ctx, store.ClearMismatchedCodexRecoveryAndMarkIntentsParams{ID: acc.ID, UserID: user})
				if err == nil && row.Cleared != 1 {
					t.Fatalf("mismatch rows=%+v", row)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: acc.ID, UserID: user})
			if err != nil {
				t.Fatal(err)
			}
			if got.RecoveryCause.Valid || got.RecoverySealed != nil || got.RecoverySealedWith.Valid || got.RecoveryGeneration.Valid {
				t.Fatalf("clearer %s left recovery metadata", clearer)
			}
			// A delayed background writer uses its original generation and operation fence.
			// Mismatch clearing leaves both unchanged, so supersede its operation first.
			if clearer == "mismatch" {
				mustExec(ctx, t, pool, "UPDATE codex_provider_account SET coord_operation_id=$1 WHERE id=$2", uuid.New(), acc.ID)
			}
			if n, err := q.SetCodexRecoverySlot(ctx, params); err != nil || n != 0 {
				t.Fatalf("delayed writer=(%d,%v)", n, err)
			}
			got, err = q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: acc.ID, UserID: user})
			if err != nil || got.RecoveryCause.Valid || got.RecoverySealed != nil {
				t.Fatalf("delayed writer restored cleared cause: %v", err)
			}
		})
	}
}

func TestCodexRefreshRecoveryEvidenceLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", user); err != nil {
			t.Errorf("delete recovery fixture user %s: %v", user, err)
		}
	})
	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{UserID: user, ProviderUserID: uuid.NewString(), WorkspaceAccountID: uuid.NewString(), SealedLogin: []byte("unreadable"), SealedWith: store.SealedWithMaster})
	if err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	if _, err = q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{OperationID: op, UserID: user, ProviderAccountID: acc.ID}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{ID: acc.ID, UserID: user, Op: op, Deadline: pgconv.Time(time.Now().Add(time.Minute))}); err != nil || n != 1 {
		t.Fatalf("lease=(%d,%v)", n, err)
	}
	slot := store.SetCodexRecoverySlotParams{ID: acc.ID, UserID: user, Op: op, Sealed: []byte("unreadable recovery"), RecoverySealedWith: pgconv.Text(store.SealedWithMaster)}
	if n, err := q.SetCodexRecoverySlot(ctx, slot); err != nil || n != 1 {
		t.Fatalf("slot=(%d,%v)", n, err)
	}
	params := store.HasCodexVaultLockRecoveryEvidenceParams{UserID: user, AccountID: acc.ID, OperationID: op}
	assertEvidence := func(want bool) {
		t.Helper()
		got, err := q.HasCodexVaultLockRecoveryEvidence(ctx, params)
		if err != nil || got != want {
			t.Fatalf("evidence=(%v,%v), want %v", got, err, want)
		}
	}
	assertEvidence(false) // Neither sealed material nor its key discriminator implies cause.
	slot.RecoveryCause = pgconv.Text("vault_locked")
	if n, err := q.SetCodexRecoverySlot(ctx, slot); err != nil || n != 1 {
		t.Fatalf("cause=(%d,%v)", n, err)
	}
	assertEvidence(true) // Opaque bytes are sufficient; this query never opens them.
	for _, state := range []string{"unrecoverable", "committed", "rotating", "reconciled"} {
		mustExec(ctx, t, pool, "UPDATE codex_refresh_intent SET state=$1 WHERE operation_id=$2", state, op)
		assertEvidence(state == "rotating" || state == "reconciled")
	}
	params.OperationID = uuid.New()
	assertEvidence(false)
	params.OperationID = op
	params.ObservedGeneration = 1
	assertEvidence(false)
	params.ObservedGeneration = 0
	params.UserID = uuid.New()
	assertEvidence(false)
	params.UserID = user
	assertEvidence(true)
	for _, sql := range []string{
		"UPDATE codex_provider_account SET recovery_cause='unsupported' WHERE id=$1",
		"UPDATE codex_provider_account SET recovery_generation=NULL WHERE id=$1",
		"UPDATE codex_provider_account SET recovery_sealed=NULL,recovery_sealed_with=NULL WHERE id=$1",
		"UPDATE codex_provider_account SET coord_state='idle' WHERE id=$1",
	} {
		if _, err := pool.Exec(ctx, sql, acc.ID); pgCode(err) != "23514" {
			t.Fatalf("incoherent metadata accepted: %v", err)
		}
		assertEvidence(true)
	}
	mustExec(ctx, t, pool, "UPDATE codex_provider_account SET recovery_cause=NULL WHERE id=$1", acc.ID)
	assertEvidence(false)
	// Closed-set NULL remains legal with a populated legacy recovery slot.
	slot.RecoveryCause = pgtype.Text{}
	if n, err := q.SetCodexRecoverySlot(ctx, slot); err != nil || n != 1 {
		t.Fatalf("legacy slot=(%d,%v)", n, err)
	}
	assertEvidence(false)
}
