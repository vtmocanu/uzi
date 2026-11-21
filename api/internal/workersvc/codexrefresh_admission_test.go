package workersvc

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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This fixture models only transaction publication, not PostgreSQL locking.
// In particular, Rollback cannot undo a commit whose reply was lost.
type refreshAdmissionTx struct {
	pgx.Tx
	t                  *testing.T
	events             []string
	userID, accountID  uuid.UUID
	lockErr, commitErr error
	commitBeforeError  bool
	pending, committed []string
	closed             bool
}

type refreshAdmissionBeginner struct{ tx *refreshAdmissionTx }

func (b refreshAdmissionBeginner) Begin(context.Context) (pgx.Tx, error) {
	b.tx.events = append(b.tx.events, "begin")
	return b.tx, nil
}

func (tx *refreshAdmissionTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx.t.Helper()
	if sql != "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" || len(args) != 0 {
		tx.t.Fatalf("unexpected admission Exec: %q args=%v", sql, args)
	}
	tx.events = append(tx.events, "read committed")
	return pgconn.NewCommandTag("SET"), nil
}

func (tx *refreshAdmissionTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.t.Helper()
	// Ignore the sqlc name comment, but accept no other query or lock mode.
	_, query, ok := strings.Cut(sql, "\n")
	if !ok || strings.Join(strings.Fields(query), " ") != "SELECT id FROM codex_provider_account WHERE id = $1 AND user_id = $2 FOR UPDATE NOWAIT" {
		tx.t.Fatalf("unexpected admission QueryRow: %q", sql)
	}
	if !reflect.DeepEqual(args, []any{tx.accountID, tx.userID}) {
		tx.t.Fatalf("account lock args = %v, want account %s user %s", args, tx.accountID, tx.userID)
	}
	tx.events = append(tx.events, "account lock")
	return refreshAdmissionRow{tx: tx}
}

type refreshAdmissionRow struct{ tx *refreshAdmissionTx }

func (row refreshAdmissionRow) Scan(dest ...any) error {
	row.tx.t.Helper()
	if len(dest) != 1 {
		row.tx.t.Fatalf("account lock Scan columns = %d, want 1", len(dest))
	}
	id, ok := dest[0].(*uuid.UUID)
	if !ok {
		row.tx.t.Fatalf("account lock Scan destination = %T, want *uuid.UUID", dest[0])
	}
	if row.tx.lockErr != nil {
		return row.tx.lockErr
	}
	*id = row.tx.accountID
	return nil
}

func (tx *refreshAdmissionTx) Commit(context.Context) error {
	tx.t.Helper()
	if tx.closed {
		tx.t.Fatal("commit on closed admission transaction")
	}
	tx.events = append(tx.events, "commit")
	if tx.commitErr == nil || tx.commitBeforeError {
		tx.committed = append(tx.committed, tx.pending...)
		tx.pending = nil
		tx.closed = true
	}
	return tx.commitErr
}

func (tx *refreshAdmissionTx) Rollback(context.Context) error {
	tx.events = append(tx.events, "rollback")
	if tx.closed {
		return pgx.ErrTxClosed
	}
	tx.pending = nil
	tx.closed = true
	return nil
}

// The unbound source has nil Queries: accidentally using it instead of the
// binder's result fails loudly, rather than silently writing outside the tx.
type refreshAdmissionStore struct {
	*store.Queries
	tx                    *refreshAdmissionTx
	bound                 bool
	boundQueries          *store.Queries
	intentErr, acquireErr error
	acquireRows           int64
	intent                store.InsertCodexRefreshIntentParams
	lease                 store.AcquireCodexRefreshLeaseParams
}

func (f *refreshAdmissionStore) bindCodexRefreshTx(q *store.Queries) codexRefreshStore {
	f.tx.t.Helper()
	if q == nil {
		f.tx.t.Fatal("nil admission transaction binding")
	}
	f.tx.events = append(f.tx.events, "bind")
	f.boundQueries = q
	bound := *f
	bound.bound = true
	bound.boundQueries = q
	return &bound
}

func (f *refreshAdmissionStore) InsertCodexRefreshIntent(_ context.Context, arg store.InsertCodexRefreshIntentParams) (store.CodexRefreshIntent, error) {
	f.tx.t.Helper()
	if !f.bound || f.tx.closed {
		f.tx.t.Fatal("intent write outside open bound transaction")
	}
	if arg != f.intent {
		f.tx.t.Fatalf("intent args = %+v, want %+v", arg, f.intent)
	}
	f.tx.events = append(f.tx.events, "intent")
	if f.intentErr != nil {
		return store.CodexRefreshIntent{}, f.intentErr
	}
	f.tx.pending = append(f.tx.pending, "intent")
	return store.CodexRefreshIntent{OperationID: arg.OperationID}, nil
}

func (f *refreshAdmissionStore) AcquireCodexRefreshLease(_ context.Context, arg store.AcquireCodexRefreshLeaseParams) (int64, error) {
	f.tx.t.Helper()
	if !f.bound || f.tx.closed {
		f.tx.t.Fatal("lease write outside open bound transaction")
	}
	if arg != f.lease {
		f.tx.t.Fatalf("lease args = %+v, want %+v", arg, f.lease)
	}
	f.tx.events = append(f.tx.events, "lease")
	if f.acquireErr != nil {
		return 0, f.acquireErr
	}
	if f.acquireRows != 0 {
		f.tx.pending = append(f.tx.pending, "lease")
	}
	return f.acquireRows, nil
}

func TestCodexRefreshAdmissionTransaction(t *testing.T) {
	refused := errors.New("authority revoked")
	intentFailed := errors.New("intent unavailable")
	acquireFailed := errors.New("lease unavailable")
	lockFailed := &pgconn.PgError{Code: "55P03", Message: "account busy"}
	duplicate := &pgconn.PgError{Code: "23505", Message: "operation already exists"}
	commitFailed := errors.New("commit response lost")

	for _, tc := range []struct {
		name              string
		authorityErr      error
		lockErr           error
		intentErr         error
		acquireErr        error
		zeroAcquire       bool
		commitErr         error
		commitBeforeError bool
		wantErr           error
		wantAdmitted      bool
		wantCommitted     bool
		wantEvents        []string
	}{
		{
			name: "authority refusal", authorityErr: refused, wantErr: refused,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "rollback"},
		},
		{
			name: "account lock refusal", lockErr: lockFailed, wantErr: lockFailed,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "rollback"},
		},
		{
			name: "intent failure", intentErr: intentFailed, wantErr: intentFailed,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "rollback"},
		},
		{
			name: "repeated operation", intentErr: duplicate, wantErr: duplicate,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "rollback"},
		},
		{
			name: "lease failure", acquireErr: acquireFailed, wantErr: acquireFailed,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "lease", "rollback"},
		},
		{
			name: "zero lease rows", zeroAcquire: true,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "lease", "rollback"},
		},
		{
			name: "commit failure before publication", commitErr: commitFailed, wantErr: commitFailed,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "lease", "commit", "rollback"},
		},
		{
			name: "commit applied but reply lost", commitErr: commitFailed, commitBeforeError: true,
			wantErr: commitFailed, wantCommitted: true,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "lease", "commit", "rollback"},
		},
		{
			name: "confirmed commit", wantAdmitted: true, wantCommitted: true,
			wantEvents: []string{"begin", "read committed", "bind", "authority", "account lock", "intent", "lease", "commit", "rollback"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
			accountID := uuid.MustParse("20000000-0000-4000-8000-000000000002")
			operationID := uuid.MustParse("30000000-0000-4000-8000-000000000003")
			deadline := time.Date(2026, time.November, 14, 12, 0, 7, 0, time.UTC)
			const generation int64 = 17
			tx := &refreshAdmissionTx{
				t: t, userID: userID, accountID: accountID, lockErr: tc.lockErr,
				commitErr: tc.commitErr, commitBeforeError: tc.commitBeforeError,
			}
			source := &refreshAdmissionStore{
				tx: tx, intentErr: tc.intentErr, acquireErr: tc.acquireErr, acquireRows: 1,
				intent: store.InsertCodexRefreshIntentParams{
					OperationID: operationID, UserID: userID, ProviderAccountID: accountID, FromGeneration: generation,
				},
			}
			source.lease.Op = operationID
			source.lease.ID = accountID
			source.lease.UserID = userID
			source.lease.FromGeneration = generation
			source.lease.Deadline.Time = deadline
			source.lease.Deadline.Valid = true
			if tc.zeroAcquire {
				source.acquireRows = 0
			}
			svc := &Service{txBeginner: refreshAdmissionBeginner{tx: tx}}
			check := func(_ context.Context, q *store.Queries) error {
				if q == nil || q != source.boundQueries {
					t.Fatal("authority check must use the bound transaction queries")
				}
				tx.events = append(tx.events, "authority")
				return tc.authorityErr
			}
			admitted, err := svc.admitCodexRefreshAdvance(
				context.Background(), source, userID, accountID, operationID, generation, deadline, check,
			)
			if admitted != tc.wantAdmitted || !errors.Is(err, tc.wantErr) {
				t.Fatalf("admission = (%t, %v), want (%t, %v)", admitted, err, tc.wantAdmitted, tc.wantErr)
			}
			// Pre-commit failures preserve the exact error, including SQLSTATE.
			if tc.commitErr == nil && err != tc.wantErr {
				t.Fatalf("admission error = %v, want exact error %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(tx.events, tc.wantEvents) {
				t.Fatalf("transaction events = %v, want %v", tx.events, tc.wantEvents)
			}
			var wantCommitted []string
			if tc.wantCommitted {
				wantCommitted = []string{"intent", "lease"}
			}
			if !reflect.DeepEqual(tx.committed, wantCommitted) {
				t.Fatalf("committed mutations = %v, want %v", tx.committed, wantCommitted)
			}
			if len(tx.pending) != 0 || !tx.closed {
				t.Fatalf("transaction not settled: pending=%v closed=%t", tx.pending, tx.closed)
			}
		})
	}
}
