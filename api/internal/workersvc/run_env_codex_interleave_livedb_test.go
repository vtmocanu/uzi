package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A Codex refresh commit takes the owner's locks in the order advisory -> users row -> account
// row (withCodexVaultWrite). The claim finish takes run -> account (FOR SHARE NOWAIT) -> users
// (FOR KEY SHARE NOWAIT, last, just before the recompute). Neither ever waits behind the other while holding what the other needs, so
// the two interleavings below both finish. Each runs the refresh writer's real statement
// sequence (the secret-mutation advisory lock, FenceVaultWrite's users lock, AcquireCodexRefreshLease
// and CommitCodexRefresh) against a Codex run that receives run environment variables.

// refreshCommit runs the refresh writer's lock sequence in one transaction and commits.
func (fx *codexClaimFix) refreshCommit(ctx context.Context, hold func()) error {
	tx, err := fx.env.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := store.New(tx)
	if err := q.LockSecretMutation(ctx, fx.userID); err != nil {
		return err
	}
	if _, err := q.LockUserVaultState(ctx, fx.userID); err != nil {
		return err
	}
	if hold != nil {
		hold()
	}
	var gen int64
	var sealed []byte
	var sealedWith string
	if err := tx.QueryRow(ctx, `SELECT generation, sealed_login, sealed_with FROM codex_provider_account WHERE id=$1`, fx.accountID).Scan(&gen, &sealed, &sealedWith); err != nil {
		return err
	}
	op := uuid.New()
	n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}, ID: fx.accountID, UserID: fx.userID, FromGeneration: gen,
	})
	if err != nil || n != 1 {
		return errors.Join(err, errors.New("refresh lease not acquired"))
	}
	if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: sealed, SealedWith: sealedWith, Op: op, ID: fx.accountID, UserID: fx.userID, FromGeneration: gen,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func newCodexRunEnvFix(t *testing.T) *codexClaimFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	fx.svc.SetBackground(func(fn func()) { fn() })
	// A repo variable, sealed through the owner's vault, and a worker that advertises run_env_v1.
	re := &reFix{&cdFix{env: env, svc: fx.svc, userID: fx.userID, repoID: fx.repoID}}
	re.put(t, "API_BASE_URL", false, "https://sandbox.example.test")
	env.exec(`UPDATE workers SET protocol_capabilities = protocol_capabilities || $2::text[] WHERE id = $1`, fx.workerA, []string{capability.RunEnvV1})
	return fx
}

func (fx *codexClaimFix) envClaimant(t *testing.T) store.Worker {
	t.Helper()
	w := fx.claimant(t, false)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.RunEnvV1)
	return w
}

// The refresh writer holds the users and account rows when the finish needs them: the finish
// retries on lock-not-available and never waits, so both end.
func TestRunEnvCodexFinishAgainstRefreshHoldingLocksLiveDB(t *testing.T) {
	fx := newCodexRunEnvFix(t)
	old := finishRunClaimRetryDelay
	finishRunClaimRetryDelay = 20 * time.Millisecond
	defer func() { finishRunClaimRetryDelay = old }()

	writerErr := make(chan error, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(ctx context.Context, _ store.Run, _ *ClaimPayload, _ error) {
		go func() {
			writerErr <- fx.refreshCommit(ctx, func() { close(started); <-release })
		}()
		<-started
		// Release the writer after the finish has had time to hit the held row at least once.
		time.AfterFunc(60*time.Millisecond, func() { close(release) })
	}}
	done := make(chan struct{})
	var claimErr error
	go func() {
		defer close(done)
		_, claimErr = fx.svc.Claim(fx.env.ctx, fx.envClaimant(t), nil)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("claim finish did not return: lock-order deadlock or wait")
	}
	if claimErr != nil && !isLockNotAvailable(claimErr) {
		t.Fatalf("claim error = %v, want success or a bounded lock-not-available", claimErr)
	}
	select {
	case err := <-writerErr:
		if err != nil {
			t.Fatalf("refresh writer: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("refresh writer did not finish")
	}
}

// The finish holds the users row FOR KEY SHARE when the refresh writer arrives: the writer waits for
// the claim's commit, then completes; the claim is not disturbed.
func TestRunEnvCodexRefreshWaitsBehindFinishShareLockLiveDB(t *testing.T) {
	fx := newCodexRunEnvFix(t)
	writerErr := make(chan error, 1)
	fx.svc.claimHooks = &claimTestHooks{afterRunEnvLock: func(ctx context.Context, _ store.Run) {
		go func() { writerErr <- fx.refreshCommit(ctx, nil) }()
		select {
		case err := <-writerErr:
			t.Errorf("refresh writer finished while the finish held the users row (err=%v)", err)
			writerErr <- err
		case <-time.After(300 * time.Millisecond):
		}
	}}
	done := make(chan struct{})
	var claimErr error
	go func() {
		defer close(done)
		_, claimErr = fx.svc.Claim(fx.env.ctx, fx.envClaimant(t), nil)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("claim finish did not return: lock-order deadlock")
	}
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	select {
	case err := <-writerErr:
		if err != nil {
			t.Fatalf("refresh writer: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("refresh writer still blocked after the claim committed")
	}
}
