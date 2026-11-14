package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Pause at the private admission seam, after initial authorization but before
// worker/run locks or intent creation. Both schedules use channel barriers.
func TestCodexRefreshLateAdmissionLiveDB(t *testing.T) {
	for _, mutation := range []string{"revoke", "switch"} {
		t.Run(mutation, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
			f := newRefreshFixture(t, env, fake)
			capability := env.mintCap(t, f.runID, f.workerID)
			auth, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, capability, ScopeStartRefresh)
			if err != nil {
				t.Fatal(err)
			}
			reached, resume := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			op := uuid.New()
			check := codexWorkerRefreshAdmission(f.wkr, f.runID, capability, auth)
			ctx, cancel := context.WithTimeout(env.ctx, 5*time.Second)
			defer cancel()
			go func() {
				_, err := f.svc.coordinatedRefresh(ctx, f.userID, f.accountID, op, 0, time.Now().Add(time.Minute), time.Now().Add(time.Minute),
					func(ctx context.Context, q *store.Queries) error {
						close(reached)
						select {
						case <-resume:
						case <-ctx.Done():
							return ctx.Err()
						}
						return check(ctx, q)
					})
				done <- err
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if mutation == "revoke" {
				env.exec("UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1, codex_cap_hash=NULL WHERE id=$1", f.runID)
			} else {
				env.exec("UPDATE runs SET codex_secret_id=NULL WHERE id=$1", f.runID)
			}
			close(resume)
			err = <-done
			want := ErrCodexCapabilityEpoch
			if mutation == "switch" {
				want = ErrCodexRunNotBound
			}
			if !errors.Is(err, want) {
				t.Fatalf("late admission err=%v, want %v", err, want)
			}
			if fake.calls != 0 {
				t.Fatalf("provider calls=%d", fake.calls)
			}
			if _, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: f.userID}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("intent err=%v", err)
			}
			if acct := env.mustAccount(t, f.userID, f.accountID); acct.Generation != 0 || acct.CoordOperationID.Valid {
				t.Fatal("late request acquired a lease")
			}
		})
	}
}

func TestCodexRefreshAdmissionBeforeRevokeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
	f := newRefreshFixture(t, env, fake)
	capability := env.mintCap(t, f.runID, f.workerID)
	op := uuid.New()
	fake.onRefresh = func() {
		// Provider entry is the barrier: admission commit must already be visible
		// from a separate connection, and the account must hold this operation.
		it := mustIntent(t, env, op, f.userID)
		acct := env.mustAccount(t, f.userID, f.accountID)
		if it.State != codexIntentRotating || !acct.CoordOperationID.Valid || uuid.UUID(acct.CoordOperationID.Bytes) != op {
			t.Fatal("provider preceded durable admission")
		}
		env.exec("UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1, codex_cap_hash=NULL WHERE id=$1", f.runID)
	}
	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capability, op, 0)
	if !errors.Is(err, ErrCodexCapabilityEpoch) || res.AccessToken != "" {
		t.Fatalf("release err=%v token present=%v", err, res.AccessToken != "")
	}
	if fake.calls != 1 || env.mustAccount(t, f.userID, f.accountID).Generation != 1 {
		t.Fatal("admitted rotation did not persist")
	}
}

func TestCodexRefreshAdmissionContentionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &fakeRefreshClient{}
	f := newRefreshFixture(t, env, fake)
	capability := env.mintCap(t, f.runID, f.workerID)
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err := store.New(tx).LockCodexAccountForUpdateNowait(env.ctx, store.LockCodexAccountForUpdateNowaitParams{
		ID: f.accountID, UserID: f.userID,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	op := uuid.New()
	res, err := f.svc.CoordinatedCodexRefresh(ctx, f.wkr, f.runID, capability, op, 0)
	if !errors.Is(err, ErrCodexRefreshContended) || res.Outcome != CodexRefreshContended || res.AccessToken != "" {
		t.Fatalf("account row contention: outcome=%v token_present=%t err=%v", res.Outcome, res.AccessToken != "", err)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls=%d", fake.calls)
	}
	if _, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{
		OperationID: op, UserID: f.userID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("contended admission left intent: %v", err)
	}
}
