package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// codexRefreshTxBinder preserves explicit fault-injection wrappers while binding
// every material write to the actual transaction. Unknown wrappers fail closed.
type codexRefreshTxBinder interface {
	bindCodexRefreshTx(*store.Queries) codexRefreshStore
}

func (s *Service) codexMaterialWriteContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if s.codexWriteContext != nil {
		return s.codexWriteContext(ctx, timeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// recordCodexMaterialLoss makes terminal retention loss durable without inventing a
// provider rejection or vault-lock cause. One detached five-second transaction
// locks account then rotating intent; no vault lifecycle/advisory work runs here.
func (s *Service) recordCodexMaterialLoss(ctx context.Context, userID, accountID, operationID uuid.UUID, fromGeneration int64) {
	if applied, err := s.quarantineCodexMaterialLoss(ctx, userID, accountID, operationID, fromGeneration); err != nil || !applied {
		slog.Warn("codex refresh: terminal material loss transition not applied", "account", accountID, "operation", operationID)
	}
}

func (s *Service) quarantineCodexMaterialLoss(ctx context.Context, userID, accountID, operationID uuid.UUID, fromGeneration int64) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if s.txBeginner == nil {
		return false, errors.New("codex refresh: no transaction beginner configured")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := q.LockCodexAccountForRejection(ctx, store.LockCodexAccountForRejectionParams{
		ID: accountID, UserID: userID, Op: operationID, FromGeneration: fromGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := q.LockRotatingCodexRefreshIntent(ctx, store.LockRotatingCodexRefreshIntentParams{
		Op: operationID, UserID: userID, ProviderAccountID: accountID, FromGeneration: fromGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	// The account lock permits a survivor's already-quarantined state. Read it
	// while locked so zero UPDATE rows is accepted only for that explicit state.
	account, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: accountID, UserID: userID})
	if err != nil {
		return false, err
	}
	n, err := q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: accountID, UserID: userID, Op: operationID})
	if err != nil {
		return false, err
	}
	if n != 1 && !(n == 0 && account.CoordState == codexCoordQuarantined) {
		return false, nil
	}
	n, err = q.MarkCodexRejectionIntentUnrecoverable(ctx, store.MarkCodexRejectionIntentUnrecoverableParams{
		Op: operationID, UserID: userID, ProviderAccountID: accountID, FromGeneration: fromGeneration,
	})
	if err != nil || n != 1 {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// withCodexVaultWrite holds secret advisory -> users -> account until commit.
// Provider I/O and vault lifecycle operations must stay outside this boundary.
func (s *Service) withCodexVaultWrite(ctx context.Context, source codexRefreshStore, userID uuid.UUID, raw []byte, purpose vault.SealPurpose, write func(context.Context, codexRefreshStore, vault.SealedMaterial) error) error {
	if s.vlt == nil {
		return vault.ErrNotConfigured
	}
	if s.txBeginner == nil {
		return errors.New("codex refresh: no transaction beginner configured")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	var bound codexRefreshStore
	switch source := source.(type) {
	case codexRefreshTxBinder:
		bound = source.bindCodexRefreshTx(q)
	case *store.Queries:
		bound = q
	default:
		return errors.New("codex refresh: store lacks transaction binding")
	}
	if bound == nil {
		return errors.New("codex refresh: empty transaction binding")
	}
	if err := q.LockSecretMutation(ctx, userID); err != nil {
		return err
	}
	state, err := q.LockUserVaultState(ctx, userID)
	if err != nil {
		return err
	}
	sealed, err := s.sealCodexLogin(vault.WithStateReader(ctx, q), userID, raw, purpose)
	if err != nil {
		return err
	}
	if sealed.Mode != state.VaultMode || sealed.Epoch != state.VaultEpoch {
		return vault.ErrInvalidState
	}
	if _, err := q.FenceVaultWrite(ctx, userID, sealed.Mode, sealed.Epoch); err != nil {
		return err
	}
	if err := write(ctx, bound, sealed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
