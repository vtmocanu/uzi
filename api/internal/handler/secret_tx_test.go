package handler

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// handlerSecretTx explicitly forwards DBTX. It is a crypto/HTTP fixture, not
// evidence of rollback or PostgreSQL lock semantics; LiveDB covers those.
type handlerSecretTx struct {
	db                    store.DBTX
	committed, rolledBack bool
}

func (t *handlerSecretTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *handlerSecretTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *handlerSecretTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *handlerSecretTx) Commit(context.Context) error   { t.committed = true; return nil }
func (t *handlerSecretTx) Rollback(context.Context) error { t.rolledBack = true; return nil }
func (*handlerSecretTx) Begin(context.Context) (pgx.Tx, error) {
	return nil, fmt.Errorf("unexpected Begin")
}
func (*handlerSecretTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, fmt.Errorf("unexpected CopyFrom")
}
func (*handlerSecretTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (*handlerSecretTx) LargeObjects() pgx.LargeObjects { panic("unexpected LargeObjects") }
func (*handlerSecretTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, fmt.Errorf("unexpected Prepare")
}
func (*handlerSecretTx) Conn() *pgx.Conn { panic("unexpected Conn") }

func newMaterialTestHandler(box *secretbox.Box, db store.DBTX) *Handler {
	m := newMemVaultQ()
	m.defaultMode = store.VaultModeUnprotected
	return &Handler{q: store.New(db), box: box, vault: newHandlerMemoryVault(box, m),
		secretBeginTx: func(context.Context) (pgx.Tx, error) { return &handlerSecretTx{db: db}, nil }}
}
