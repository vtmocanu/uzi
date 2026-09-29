package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpsertReleaseSettings publishes one release fact group in a single transaction.
// Callers include the RC keys only when the RC fetch succeeded.
func (q *Queries) UpsertReleaseSettings(ctx context.Context, facts []UpsertAppSettingParams) error {
	beginner, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("release settings: database does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := q.WithTx(tx)
	for _, fact := range facts {
		if _, err := queries.UpsertAppSetting(ctx, fact); err != nil {
			return fmt.Errorf("release settings: write %s: %w", fact.Key, err)
		}
	}
	return tx.Commit(ctx)
}
