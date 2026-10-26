package handler

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"testing"
	"time"
)

func setVaultFixturePassword(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(context.Background(), t, pool, "UPDATE users SET password_hash=$2 WHERE id=$1", id, hash)
}

// Every lifecycle callback receives the interception wrapper over its own real
// transaction. Out-of-transaction notice calls use the supplied root wrapper.
func newWrappedPostgresVault(box *secretbox.Box, pool *pgxpool.Pool, root vault.Store, wrap func(*store.Queries) vault.Store) *vault.Vault {
	return vault.New(box, root, func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
		}()
		q := wrap(store.New(tx))
		if err := fn(vault.WithStateReader(ctx, q), q); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &vault.CommitError{Err: err}
		}
		return nil
	})
}
