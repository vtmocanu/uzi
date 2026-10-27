package vault

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CommitError distinguishes an uncertain commit acknowledgment from a callback
// failure before commit. It makes no promise that durable state is unchanged.
type CommitError struct{ Err error }

func (e *CommitError) Error() string {
	return fmt.Sprintf("vault: commit outcome uncertain: %v", e.Err)
}
func (e *CommitError) Unwrap() error { return e.Err }

// NewPostgres installs the real transaction adapter. Admission reads require
// READ COMMITTED regardless of the pool's default transaction configuration.
func NewPostgres(master *secretbox.Box, pool *pgxpool.Pool, opts ...Option) *Vault {
	if pool == nil {
		panic("vault: database pool required")
	}
	q := store.New(pool)
	return New(master, q, func(ctx context.Context, fn func(context.Context, Store) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return fmt.Errorf("vault: begin: %w", err)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
		}()
		tq := q.WithTx(tx)
		if err := fn(WithStateReader(ctx, tq), tq); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &CommitError{Err: err}
		}
		return nil
	}, opts...)
}
