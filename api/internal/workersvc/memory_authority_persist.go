package workersvc

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// persistMemoryAuthority records a run-lane claim's memory-authority report on the worker row
// (issue #4395, workers.memory_authority_lost_at), so a keeper whose memory authority is lost is
// not counted as ready by the warm-reserve policy.
//
// It runs inside the claim transaction, with locked being the worker row as read by
// GetWorkerForUpdate, after the claim snapshot was validated against it. A lost report sets the
// column when it is NULL (the first-lost time is kept) and clears memory_authority_held_at. A loss
// is sticky for its registration (issue #4492): the agent's loss is one-way until it restarts
// (agent/src/client.ts), so a later held report never clears it and only Register does. A held
// report on a worker with no recorded loss returns a receipt time (the service clock) that the
// claim passes to applyWorkerActiveSnapshot to stamp memory_authority_held_at, which pool
// readiness needs fresh for a worker with both capabilities; a held report on a worker with a
// recorded loss returns no receipt, and SetWorkerSnapshotState refuses to stamp one under a loss
// in any case. A held-to-held claim does not touch memory_authority_lost_at. An absent report
// from a worker without both capabilities is neither lost nor held and writes nothing.
//
// It persists on snapshot-carrying run and ordinary-lane claims, whether or not a run is claimed:
// the caller invokes it before the claim, so empty and overflow-refused polls persist too. It does
// not persist on claims with no active snapshot (for example UZI_ACTIVE_SNAPSHOT_DISABLED, a
// missing register nonce or a malformed body). A claim that the owner's vault gate turns idle never
// reaches this transaction; persistMemoryAuthorityVaultLocked records the same report for it,
// through the same decision. Ephemeral claims (ephemeral_lease.go) are out of scope: ephemeral
// workers are never in a pool. Divergence by design: a snapshot-carrying ephemeral claim on the
// unlocked path persists through this function, while the locked path skips ephemeral workers.
//
// The registration fence is the snapshot's register nonce, repeated in the UPDATE's WHERE. A
// claim without a snapshot carries no registration identity, so it persists nothing.
func (s *Service) persistMemoryAuthority(ctx context.Context, qtx *store.Queries, locked store.Worker, snapshot *ActiveSnapshot, lane string, report MemoryAuthorityReport) (heldAt pgtype.Timestamptz, err error) {
	if lane == "cross_check" || snapshot == nil || snapshot.RegisterNonce == "" {
		return pgtype.Timestamptz{}, nil
	}
	return s.recordMemoryAuthority(ctx, qtx, locked, snapshot.RegisterNonce, report)
}

// recordMemoryAuthority is the lost/held decision shared by the claim transaction and the
// vault-locked report transaction. It writes the loss (when new) and returns the receipt time a
// held report with no recorded loss earns; the caller stamps it.
func (s *Service) recordMemoryAuthority(ctx context.Context, qtx *store.Queries, locked store.Worker, registerNonce string, report MemoryAuthorityReport) (heldAt pgtype.Timestamptz, err error) {
	switch {
	case memoryAuthorityLost(locked.ProtocolCapabilities, report):
		if locked.MemoryAuthorityLostAt.Valid {
			return pgtype.Timestamptz{}, nil
		}
		// SetWorkerMemoryAuthorityLost also clears memory_authority_held_at.
		_, err := qtx.SetWorkerMemoryAuthorityLost(ctx, store.SetWorkerMemoryAuthorityLostParams{
			ID: locked.ID, RegisterNonce: pgconv.TextOrNull(registerNonce), LostAt: pgconv.Time(s.now()),
		})
		return pgtype.Timestamptz{}, err
	case report.held():
		if locked.MemoryAuthorityLostAt.Valid {
			return pgtype.Timestamptz{}, nil
		}
		return pgconv.Time(s.now()), nil
	}
	return pgtype.Timestamptz{}, nil
}

// persistMemoryAuthorityVaultLocked records the memory-authority report of a poll that the owner's
// vault gate (claimLane) is about to turn idle (issue #4492), so a locked protected-vault owner's
// workers still earn a held receipt or a recorded loss and the warm-reserve policy can count them.
//
// It is best effort and writes only the two memory-authority columns on the worker row: no
// snapshot apply, no snapshot_epoch write, no claim, no runs rows and no secrets. It is a no-op
// (no database access) for a cross-check claim, a missing snapshot or register nonce, an
// ephemeral worker (never in a pool) and a service without a transaction beginner. The worker row
// is locked and the snapshot validated against it exactly like the claim transaction; an invalid
// or stale snapshot (including a delayed report from a replaced registration) persists nothing.
// Any error is logged with the worker id only and swallowed, so the poll still reports idle.
func (s *Service) persistMemoryAuthorityVaultLocked(ctx context.Context, wkr store.Worker, snapshot *ActiveSnapshot, lane string, report MemoryAuthorityReport) {
	if lane == "cross_check" || snapshot == nil || snapshot.RegisterNonce == "" || wkr.Ephemeral || s.txBeginner == nil {
		return
	}
	if err := s.persistMemoryAuthorityVaultLockedTx(ctx, wkr, snapshot, report); err != nil {
		slog.Warn("vault-locked memory authority persist failed", "worker_id", wkr.ID)
	}
}

func (s *Service) persistMemoryAuthorityVaultLockedTx(ctx context.Context, wkr store.Worker, snapshot *ActiveSnapshot, report MemoryAuthorityReport) error {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return err
	}
	qtx := store.New(tx)
	locked, err := qtx.GetWorkerForUpdate(ctx, wkr.ID)
	if err != nil {
		return err
	}
	if _, valid, err := s.validatedActiveSnapshot(locked, snapshot, snapshotModeClaim); err != nil || !valid {
		return err // nothing is persisted for an invalid or stale snapshot; the deferred rollback runs
	}
	heldAt, err := s.recordMemoryAuthority(ctx, qtx, locked, snapshot.RegisterNonce, report)
	if err != nil {
		return err
	}
	if heldAt.Valid {
		if _, err := qtx.StampWorkerMemoryAuthorityHeld(ctx, store.StampWorkerMemoryAuthorityHeldParams{
			ID: locked.ID, RegisterNonce: pgconv.TextOrNull(snapshot.RegisterNonce), HeldAt: heldAt,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
