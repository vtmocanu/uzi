package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrTerminalConsumerGateBusy: the shared gate could not be acquired within the wait
	// (lock_timeout) or the context ended before or while waiting. The transaction is aborted,
	// or (context already ended) nothing was sent on it; roll it back either way.
	ErrTerminalConsumerGateBusy = errors.New("store: terminal consumer gate busy")
	// ErrTerminalConsumerGateNoTx: the helper was called on a pool-bound Queries. An XACT lock
	// taken in autocommit mode releases at once and serializes nothing, so it is refused.
	ErrTerminalConsumerGateNoTx = errors.New("store: terminal consumer gate requires a transaction-bound Queries")
	// ErrTerminalConsumerGateIsolation: the transaction is not READ COMMITTED.
	ErrTerminalConsumerGateIsolation = errors.New("store: terminal consumer gate requires READ COMMITTED isolation")
	// ErrTerminalConsumerGateNestedTx: BeginTx was called on an already transaction-bound Queries.
	ErrTerminalConsumerGateNestedTx = errors.New("store: BeginTx on a transaction-bound Queries")
)

const (
	sqlStateLockNotAvailable = "55P03"
	sqlStateQueryCanceled    = "57014"
)

// TerminalConsumerGateObjID derives the objid half of the TerminalConsumerGateLockClass
// advisory lock from a repository id. Computed in Go (fnv32a over the raw uuid bytes) and
// passed as an int4 param. A hash collision only serializes two unrelated repositories for a
// moment. Exported so live-DB tests can find a waiter on exactly this key in pg_locks.
func TerminalConsumerGateObjID(repoID uuid.UUID) int32 {
	h := fnv.New32a()
	_, _ = h.Write(repoID[:])
	return int32(h.Sum32()) //nolint:gosec // wraparound is fine: a lock key, not a number
}

// InTx reports whether this Queries is bound to a pgx.Tx.
func (q *Queries) InTx() bool {
	_, ok := q.db.(pgx.Tx)
	return ok
}

// LockTerminalConsumerGateShared takes the repository's consumer gate in shared mode for the
// rest of the transaction, waiting at most wait. Call it as the first lock of the transaction
// on a tx-bound Queries.
//
// It refuses (ErrTerminalConsumerGateNoTx) on a pool-bound Queries and
// (ErrTerminalConsumerGateIsolation) on any isolation other than READ COMMITTED. The wait is
// bounded by a transaction-local lock_timeout set via set_config(..., true), and the previous
// value is restored with set_config afterwards (never SET/RESET). The acquire is a statement
// of its own, so the caller's next statement takes a fresh READ COMMITTED snapshot and sees
// rows an exclusive holder committed before releasing.
//
// If the acquire fails the transaction is aborted and nothing more is sent on it (roll it
// back; a context that had already ended before the call returns busy with nothing sent): a lock
// timeout (55P03), a cancelled query (57014) or an ended context return an error satisfying
// errors.Is(err, ErrTerminalConsumerGateBusy), joined with the driver error; any other error
// is returned as is. An already-ended context is reported as busy too.
//
// Take it on the TOP-LEVEL transaction only: a pgx nested Tx is a savepoint, and ROLLBACK TO
// SAVEPOINT releases an advisory xact lock taken inside it while the outer transaction goes on.
// With pgx's default context watcher a cancelled wait closes the socket without a cancel
// request, so the server backend can stay queued as a waiter until its lock_timeout expires;
// keep wait short. That orphan never weakens the gate (if granted it only makes a retirement
// try back off).
func (q *Queries) LockTerminalConsumerGateShared(ctx context.Context, repoID uuid.UUID, wait time.Duration) error {
	if !q.InTx() {
		return ErrTerminalConsumerGateNoTx
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrTerminalConsumerGateBusy, err)
	}
	var isolation, prevTimeout string
	if err := q.db.QueryRow(ctx,
		"SELECT current_setting('transaction_isolation'), current_setting('lock_timeout')",
	).Scan(&isolation, &prevTimeout); err != nil {
		if ctx.Err() != nil {
			return errors.Join(ErrTerminalConsumerGateBusy, err)
		}
		return fmt.Errorf("terminal consumer gate: read session settings: %w", err)
	}
	if isolation != "read committed" {
		return ErrTerminalConsumerGateIsolation
	}
	ms := wait.Milliseconds()
	if ms < 1 {
		ms = 1 // lock_timeout 0 means wait forever
	}
	if ms > math.MaxInt32 {
		ms = math.MaxInt32 // lock_timeout is an int4 of milliseconds
	}
	if _, err := q.db.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(ms, 10)+"ms"); err != nil {
		if ctx.Err() != nil {
			return errors.Join(ErrTerminalConsumerGateBusy, err)
		}
		return fmt.Errorf("terminal consumer gate: set lock_timeout: %w", err)
	}
	if _, err := q.db.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1, $2)",
		TerminalConsumerGateLockClass, TerminalConsumerGateObjID(repoID)); err != nil {
		// The transaction is aborted: no restore statement can run on it.
		var pgErr *pgconn.PgError
		if ctx.Err() != nil || (errors.As(err, &pgErr) &&
			(pgErr.Code == sqlStateLockNotAvailable || pgErr.Code == sqlStateQueryCanceled)) {
			return errors.Join(ErrTerminalConsumerGateBusy, err)
		}
		return err
	}
	if _, err := q.db.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)", prevTimeout); err != nil {
		if ctx.Err() != nil {
			// The context ended after the grant: the transaction cannot continue, nothing was
			// created, and the caller may retry, like a cancellation during the wait.
			return errors.Join(ErrTerminalConsumerGateBusy, err)
		}
		return fmt.Errorf("terminal consumer gate: restore lock_timeout: %w", err)
	}
	return nil
}

// TryLockTerminalConsumerGate try-takes the repository's consumer gate exclusively for the
// rest of the transaction and reports whether it was acquired; it never waits. False means an
// admission (shared holder) or another retirement holds it. Refuses
// ErrTerminalConsumerGateNoTx on a pool-bound Queries.
func (q *Queries) TryLockTerminalConsumerGate(ctx context.Context, repoID uuid.UUID) (bool, error) {
	if !q.InTx() {
		return false, ErrTerminalConsumerGateNoTx
	}
	var ok bool
	err := q.db.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, $2)",
		TerminalConsumerGateLockClass, TerminalConsumerGateObjID(repoID)).Scan(&ok)
	return ok, err
}

// RunRepoForConsumerGate returns the repo_id of a run, so a caller can take the repository's
// gate before it reads anything else about the run. pgx.ErrNoRows passes through for a
// missing run.
func (q *Queries) RunRepoForConsumerGate(ctx context.Context, runID uuid.UUID) (pgtype.UUID, error) {
	var repo pgtype.UUID
	err := q.db.QueryRow(ctx, "SELECT repo_id FROM runs WHERE id = $1", runID).Scan(&repo)
	return repo, err
}

// BeginTx starts a transaction on a pool-bound Queries. It exists only for the test-only
// nil-txBeginner create fallback; production wires a transaction beginner. It refuses
// ErrTerminalConsumerGateNestedTx on a tx-bound Queries and returns an error when the
// underlying handle cannot begin a transaction.
func (q *Queries) BeginTx(ctx context.Context) (pgx.Tx, error) {
	if q.InTx() {
		return nil, ErrTerminalConsumerGateNestedTx
	}
	b, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("store: BeginTx: underlying handle cannot begin a transaction")
	}
	return b.Begin(ctx)
}
