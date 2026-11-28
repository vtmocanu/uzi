package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Issue #4784: per-user JSON documents for runtime web modules.
//
// Revisions come from one sequence and are equality-only tokens: no ordering is
// promised. They are unique under same-primary crash recovery (synchronous_commit
// on, fsync on, storage honouring flushes), and not across failover, PITR, setval
// or a migration Down.

// MaxModuleUserDocuments caps documents per user.
const MaxModuleUserDocuments = 32

// ExpectedKind selects the precondition a write is checked against.
type ExpectedKind int

const (
	// ExpectCreate requires that no document exists.
	ExpectCreate ExpectedKind = iota
	// ExpectMatch requires the stored revision to equal Value.
	ExpectMatch
	// ExpectNeverMatch can never succeed (a malformed precondition); the write
	// still validates the body and then reports the current revision.
	ExpectNeverMatch
)

// ExpectedRevision is a write precondition.
type ExpectedRevision struct {
	Kind  ExpectedKind
	Value int64
}

var (
	// ErrModuleDataInvalid means the body is not storable as jsonb (input error).
	ErrModuleDataInvalid = errors.New("store: module data is not a storable JSON document")
	// ErrModuleDataCap means the user already holds MaxModuleUserDocuments documents.
	ErrModuleDataCap = errors.New("store: module data document cap reached")
	// ErrModuleDataNoUser means the user row does not exist.
	ErrModuleDataNoUser = errors.New("store: module data user not found")
)

// ModuleDataConflict reports a failed precondition. Current is the stored
// revision observed while building the conflict, nil when no document exists.
// After a failed conditional write it comes from a read that follows the check,
// so a concurrent writer can make it newer than the revision the check saw;
// clients refetch on a conflict either way.
type ModuleDataConflict struct{ Current *int64 }

func (e *ModuleDataConflict) Error() string { return "store: module data revision conflict" }

// ModuleDataCommitError marks an uncertain commit acknowledgment; it makes no
// promise that durable state is unchanged.
type ModuleDataCommitError struct{ Err error }

func (e *ModuleDataCommitError) Error() string {
	return fmt.Sprintf("store: module data commit outcome uncertain: %v", e.Err)
}
func (e *ModuleDataCommitError) Unwrap() error { return e.Err }

// validateErrIsInput reports whether a jsonb validation error is the client's
// fault: invalid text, untranslatable character, bad unicode escape, numeric
// range or a statement/value too complex. Anything else is a server error.
func validateErrIsInput(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "22P02", "22P05", "22021", "22003", "54001":
		return true
	}
	return false
}

type moduleDataTxBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// Test hooks; nil in production.
var (
	moduleDataAfterBeginHook atomic.Pointer[func()]
	moduleDataAfterCountHook atomic.Pointer[func()]
)

func runModuleDataHook(h *atomic.Pointer[func()]) {
	if f := h.Load(); f != nil {
		(*f)()
	}
}

// GetModuleUserDataDoc returns the document and revision; (nil, nil, nil) when absent.
func GetModuleUserDataDoc(ctx context.Context, q *Queries, userID uuid.UUID, moduleID string) ([]byte, *int64, error) {
	row, err := q.GetModuleUserData(ctx, GetModuleUserDataParams{UserID: userID, ModuleID: moduleID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rev := row.Revision
	return row.Data, &rev, nil
}

func beginModuleDataTx(ctx context.Context, db moduleDataTxBeginner) (pgx.Tx, func(), error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}
	return tx, cleanup, nil
}

func moduleDataConflict(ctx context.Context, q *Queries, userID uuid.UUID, moduleID string) error {
	_, cur, err := GetModuleUserDataDoc(ctx, q, userID, moduleID)
	if err != nil {
		return err
	}
	return &ModuleDataConflict{Current: cur}
}

// PutModuleUserData writes a document under exp and returns the new revision,
// only after the commit succeeded. Validation runs for every kind so an invalid
// body is never reported as a conflict or a cap.
func PutModuleUserData(ctx context.Context, db moduleDataTxBeginner, userID uuid.UUID, moduleID string, data []byte, exp ExpectedRevision) (int64, error) {
	tx, cleanup, err := beginModuleDataTx(ctx, db)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	q := New(tx)
	runModuleDataHook(&moduleDataAfterBeginHook)

	typ, err := q.ValidateModuleUserDataJSON(ctx, data)
	if err != nil {
		if validateErrIsInput(err) {
			return 0, ErrModuleDataInvalid
		}
		return 0, fmt.Errorf("store: validate module data: %w", err)
	}
	// The query maps a nil body (SQL NULL) to "null" too; neither is a document.
	if typ == "null" {
		return 0, ErrModuleDataInvalid
	}

	var rev int64
	switch exp.Kind {
	case ExpectNeverMatch:
		return 0, moduleDataConflict(ctx, q, userID, moduleID)
	case ExpectCreate:
		if _, err := q.LockUserForModuleData(ctx, userID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrModuleDataNoUser
			}
			return 0, err
		}
		if _, cur, err := GetModuleUserDataDoc(ctx, q, userID, moduleID); err != nil {
			return 0, err
		} else if cur != nil {
			return 0, &ModuleDataConflict{Current: cur}
		}
		n, err := q.CountModuleUserData(ctx, userID)
		if err != nil {
			return 0, err
		}
		runModuleDataHook(&moduleDataAfterCountHook)
		if n >= MaxModuleUserDocuments {
			return 0, ErrModuleDataCap
		}
		rev, err = q.InsertModuleUserData(ctx, InsertModuleUserDataParams{UserID: userID, ModuleID: moduleID, Column3: data})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, moduleDataConflict(ctx, q, userID, moduleID)
		}
		if err != nil {
			return 0, err
		}
	case ExpectMatch:
		rev, err = q.UpdateModuleUserData(ctx, UpdateModuleUserDataParams{UserID: userID, ModuleID: moduleID, Column3: data, Revision: exp.Value})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, moduleDataConflict(ctx, q, userID, moduleID)
		}
		if err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("store: unknown expected-revision kind %d", exp.Kind)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, &ModuleDataCommitError{Err: err}
	}
	return rev, nil
}

// DeleteModuleUserData deletes the document only when its revision equals
// exp.Value. ExpectCreate is treated as ExpectNeverMatch.
func DeleteModuleUserData(ctx context.Context, db moduleDataTxBeginner, userID uuid.UUID, moduleID string, exp ExpectedRevision) error {
	tx, cleanup, err := beginModuleDataTx(ctx, db)
	if err != nil {
		return err
	}
	defer cleanup()
	q := New(tx)

	if exp.Kind != ExpectMatch {
		return moduleDataConflict(ctx, q, userID, moduleID)
	}
	if _, err := q.DeleteModuleUserData(ctx, DeleteModuleUserDataParams{UserID: userID, ModuleID: moduleID, Revision: exp.Value}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return moduleDataConflict(ctx, q, userID, moduleID)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return &ModuleDataCommitError{Err: err}
	}
	return nil
}
