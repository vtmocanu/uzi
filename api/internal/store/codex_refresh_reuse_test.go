package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

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

type reuseClockTx struct {
	pgx.Tx
	steps          []string
	at, consumedAt pgtype.Timestamptz
}

func (tx *reuseClockTx) Exec(_ context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	if sql == "SELECT pg_advisory_xact_lock($1, $2)" {
		tx.steps = append(tx.steps, "user_lock")
		return pgconn.CommandTag{}, nil
	}
	if strings.Contains(sql, "-- name: ConsumeCodexRefreshReuse") {
		tx.steps = append(tx.steps, "consume")
		for _, arg := range args {
			if stamp, ok := arg.(pgtype.Timestamptz); ok {
				tx.consumedAt = stamp
			}
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	panic("unexpected admission write")
}

type reuseClockRow struct {
	at *pgtype.Timestamptz
}

func (r reuseClockRow) Scan(dest ...interface{}) error {
	if r.at != nil {
		*dest[0].(*pgtype.Timestamptz) = *r.at
	}
	return nil
}
func (tx *reuseClockTx) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	switch {
	case strings.Contains(sql, "-- name: LockCodexRefreshReuseAccount"):
		tx.steps = append(tx.steps, "account_lock")
	case strings.Contains(sql, "-- name: LockRotatingCodexRefreshIntent"):
		tx.steps = append(tx.steps, "intent_lock")
	case strings.Contains(sql, "-- name: LeaseClockNow"):
		tx.steps = append(tx.steps, "fresh_clock")
		return reuseClockRow{at: &tx.at}
	default:
		panic("unexpected admission read")
	}
	return reuseClockRow{}
}
func (tx *reuseClockTx) Commit(context.Context) error {
	tx.steps = append(tx.steps, "commit")
	return nil
}
func (tx *reuseClockTx) Rollback(context.Context) error { return nil }

type reuseClockBeginner struct{ tx *reuseClockTx }

func (b reuseClockBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

func TestCodexReuseAdmissionClockAfterLocks(t *testing.T) {
	tx := &reuseClockTx{at: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	ok, err := store.AdmitCodexRefreshReuse(context.Background(), reuseClockBeginner{tx}, store.CodexRefreshReuseParams{
		UserID: uuid.New(), AccountID: uuid.New(), OperationID: uuid.New(), FromGeneration: 7,
	})
	if err != nil || !ok {
		t.Fatalf("admission=%v,%v", ok, err)
	}
	want := []string{"user_lock", "account_lock", "intent_lock", "fresh_clock", "consume", "commit"}
	if !reflect.DeepEqual(tx.steps, want) {
		t.Fatalf("admission order=%v, want %v", tx.steps, want)
	}
	if !tx.consumedAt.Valid || !tx.consumedAt.Time.Equal(tx.at.Time) {
		t.Fatal("consumption did not receive the post-lock database timestamp")
	}
}

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
