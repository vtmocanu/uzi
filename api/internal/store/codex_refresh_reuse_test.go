package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type reuseEarlyTx struct {
	pgx.Tx
	first      string
	lockErr    error
	rolledBack bool
	queried    bool
}

func (tx *reuseEarlyTx) Exec(_ context.Context, sql string, _ ...interface{}) (pgconn.CommandTag, error) {
	if tx.first != "" {
		panic("unexpected second exec")
	}
	tx.first = sql
	return pgconn.CommandTag{}, tx.lockErr
}

type reuseMissingRow struct{}

func (reuseMissingRow) Scan(...interface{}) error { return pgx.ErrNoRows }
func (tx *reuseEarlyTx) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	tx.queried = true
	return reuseMissingRow{}
}
func (tx *reuseEarlyTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}

type reuseEarlyBeginner struct {
	tx  *reuseEarlyTx
	err error
}

func (b reuseEarlyBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, b.err }

func TestCodexReuseAdmissionTransactionFailures(t *testing.T) {
	for _, kind := range []string{"begin", "user_lock", "account_absent"} {
		t.Run(kind, func(t *testing.T) {
			failure := errors.New("db unavailable")
			tx := &reuseEarlyTx{}
			b := reuseEarlyBeginner{tx: tx}
			if kind == "begin" {
				b.err = failure
			}
			if kind == "user_lock" {
				tx.lockErr = failure
			}
			ok, err := store.AdmitCodexRefreshReuse(context.Background(), b, store.CodexRefreshReuseParams{UserID: uuid.New()})
			if ok {
				t.Fatal("failure admitted retry")
			}
			if kind == "account_absent" {
				if err != nil || !tx.queried {
					t.Fatalf("missing account result=%v queried=%v", err, tx.queried)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("lost database error: %v", err)
			}
			if kind != "begin" {
				if tx.first != "SELECT pg_advisory_xact_lock($1, $2)" || !tx.rolledBack {
					t.Fatal("user lock must be first, transaction must roll back")
				}
			}
			if kind == "user_lock" && tx.queried {
				t.Fatal("account read after user lock failure")
			}
		})
	}
}
