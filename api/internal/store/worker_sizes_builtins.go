package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// workerSizeReconcilerQueries is the query subset the builtin worker-size reconcile
// needs; narrowing keeps the shadow-warning path unit-testable without a database.
type workerSizeReconcilerQueries interface {
	InsertBuiltinWorkerSize(ctx context.Context, arg InsertBuiltinWorkerSizeParams) (int64, error)
	// RefreshPristineBuiltinWorkerSize re-applies shipped values to a pristine builtin
	// row (builtin AND NOT customized) when they changed.
	RefreshPristineBuiltinWorkerSize(ctx context.Context, arg RefreshPristineBuiltinWorkerSizeParams) (int64, error)
	GetWorkerSize(ctx context.Context, name string) (WorkerSize, error)
}

// ReconcileBuiltinWorkerSizes seeds the shipped hosted-worker sizes (PRD #4213), the
// builtin-agent-template pattern: a missing builtin is inserted with the shipped values;
// an existing pristine builtin row (customized = false) is refreshed to them when they
// changed; an admin-edited row is never overwritten. It runs once at boot, after
// migrations and before the hosted poll can serve, in one transaction under the table's
// advisory lock (WorkerSizesLockClass), so it cannot interleave with an admin mutation
// on another api replica.
func ReconcileBuiltinWorkerSizes(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit
	if err := ReconcileBuiltinWorkerSizesTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReconcileBuiltinWorkerSizesTx is ReconcileBuiltinWorkerSizes inside a caller-owned
// transaction (which it neither commits nor rolls back), so a test can reconcile and roll
// the shared table back.
func ReconcileBuiltinWorkerSizesTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", WorkerSizesLockClass); err != nil {
		return fmt.Errorf("lock worker sizes: %w", err)
	}
	return reconcileBuiltinWorkerSizes(ctx, New(tx))
}

func reconcileBuiltinWorkerSizes(ctx context.Context, q workerSizeReconcilerQueries) error {
	var inserted int
	for _, s := range builtin.Sizes() {
		n, err := q.InsertBuiltinWorkerSize(ctx, InsertBuiltinWorkerSizeParams{
			Name:                 s.Name,
			CpuRequestMillicores: s.CPURequestMillicores,
			CpuLimitMillicores:   s.CPULimitMillicores,
			MemoryRequestMib:     s.MemoryRequestMiB,
			MemoryLimitMib:       s.MemoryLimitMiB,
			DataGib:              s.DataGiB,
			DindDataGib:          s.DinDDataGiB,
			DisplayOrder:         s.DisplayOrder,
		})
		if err != nil {
			return fmt.Errorf("insert builtin worker size %q: %w", s.Name, err)
		}
		if n > 0 {
			inserted++
			continue
		}
		// The row already exists. Deliver shipped values to it IF it is still pristine;
		// the query's WHERE leaves an edited row and a same-name admin size alone.
		if _, err := q.RefreshPristineBuiltinWorkerSize(ctx, RefreshPristineBuiltinWorkerSizeParams{
			Name:                 s.Name,
			CpuRequestMillicores: s.CPURequestMillicores,
			CpuLimitMillicores:   s.CPULimitMillicores,
			MemoryRequestMib:     s.MemoryRequestMiB,
			MemoryLimitMib:       s.MemoryLimitMiB,
			DataGib:              s.DataGiB,
			DindDataGib:          s.DinDDataGiB,
			DisplayOrder:         s.DisplayOrder,
		}); err != nil {
			return fmt.Errorf("refresh pristine builtin worker size %q: %w", s.Name, err)
		}
		existing, gErr := q.GetWorkerSize(ctx, s.Name)
		switch {
		case gErr != nil:
			slog.Debug("reconcile: could not read back the existing worker size", "name", s.Name, "error", gErr)
		case !existing.Builtin:
			slog.Warn("builtin worker size shadowed by an admin size of the same name; skipping seed", "name", s.Name)
		}
	}
	slog.Info("reconciled builtin worker sizes", "builtins", len(builtin.Sizes()), "inserted", inserted)
	return nil
}
