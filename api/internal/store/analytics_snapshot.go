package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Issue #4684: every analytics read runs inside one REPEATABLE READ READ ONLY snapshot so the
// options, authority checks and Spend statements agree with each other.

const (
	// AnalyticsSnapshotDeadline bounds begin + SET LOCAL + every statement in fn, and is also
	// the server-side statement_timeout.
	AnalyticsSnapshotDeadline = 8 * time.Second
	// analyticsRollbackTimeout bounds the cleanup rollback, which runs on a context detached
	// from the request so a cancelled request still releases the connection.
	analyticsRollbackTimeout = 2 * time.Second
)

// ErrAnalyticsTimeout is what ClassifyAnalyticsErr returns for any timeout-shaped failure.
var ErrAnalyticsTimeout = errors.New("analytics: query timed out")

// AnalyticsBeginner is the slice of *pgxpool.Pool the snapshot helper needs.
type AnalyticsBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// AnalyticsSnapshot is the handle fn receives; every query on Q shares one snapshot, fixed by
// the first statement run on it.
type AnalyticsSnapshot struct {
	Q  *Queries
	tx pgx.Tx
}

// RunAnalyticsSnapshot runs fn inside a REPEATABLE READ READ ONLY transaction. One deadline
// (AnalyticsSnapshotDeadline) covers begin, SET LOCAL and fn; fn receives that deadline's
// context. The rollback always runs, on a context detached from ctx's cancellation.
func RunAnalyticsSnapshot(ctx context.Context, beginner AnalyticsBeginner, fn func(ctx context.Context, s *AnalyticsSnapshot) error) error {
	dctx, cancel := context.WithTimeout(ctx, AnalyticsSnapshotDeadline)
	defer cancel()

	tx, err := beginner.BeginTx(dctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), analyticsRollbackTimeout)
		defer rcancel()
		if rerr := tx.Rollback(rctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			slog.Debug("analytics snapshot rollback failed", "err", rerr)
		}
	}()

	if _, err := tx.Exec(dctx, "SET LOCAL statement_timeout = '8s'"); err != nil {
		return err
	}
	// JIT off: the custom plans of the admin-all Spend statement are costed above jit_above_cost
	// and PostgreSQL then JIT-compiles about 446 functions, 6.4-7.3s per call at 120k runs
	// (admin 30d: 8.2-8.4s with JIT, 1.3s without). Issue #4684 m5 plan and runtime evidence.
	if _, err := tx.Exec(dctx, "SET LOCAL jit = off"); err != nil {
		return err
	}
	return fn(dctx, &AnalyticsSnapshot{Q: New(tx), tx: tx})
}

// ClassifyAnalyticsErr maps statement-timeout (SQLSTATE 57014), network timeouts and context
// deadline expiry to ErrAnalyticsTimeout; any other error is returned unchanged.
func ClassifyAnalyticsErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return ErrAnalyticsTimeout
	}
	// pgconn.Timeout only matches pgconn's own timeout type; a bare net.Error timeout (for
	// example os.ErrDeadlineExceeded surfaced from the socket) is the same condition.
	var netErr net.Error
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return ErrAnalyticsTimeout
	}
	return err
}

// Savepoint runs fn, one provider gauge read, inside a SAVEPOINT of the snapshot transaction so
// a gauge that fails with an ordinary error does not poison the snapshot for the statements
// that follow (issue #4731). ctx is the snapshot context RunAnalyticsSnapshot handed to its
// callback. fn receives New(gtx) and the savepoint transaction itself, so raw statements can run
// on it too. fn must not SET LOCAL on a path that returns nil: RELEASE keeps the setting for the
// rest of the snapshot (a SET LOCAL is rolled back only when fn fails and the savepoint rolls back).
//
// Failure model, two results:
//   - gaugeErr: fn failed with an ordinary error (not a timeout) while the snapshot context was
//     live, and the savepoint rolled back cleanly. The transaction and its snapshot are intact
//     and err is nil; the caller marks that gauge unavailable and carries on.
//   - err: fatal for the whole request. Begin, Commit or Rollback of the savepoint failed (the
//     transaction state is unknown), the snapshot context ended, or fn failed with a timeout
//     (57014 or a network timeout). The caller aborts the snapshot and maps err through
//     ClassifyAnalyticsErr like any other analytics failure.
//
// When fn fails and the snapshot context is already done, ctx.Err() is returned unwrapped
// instead of fn's error: a driver error surfaced as the deadline hit would otherwise look like
// an ordinary gauge failure and answer 500 instead of the timeout the snapshot deadline really
// was. On a snapshot deadline that error is context.DeadlineExceeded, which ClassifyAnalyticsErr
// maps to ErrAnalyticsTimeout; on client cancellation it is context.Canceled, and the caller
// writes nothing.
func (s *AnalyticsSnapshot) Savepoint(ctx context.Context, fn func(ctx context.Context, q *Queries, db DBTX) error) (gaugeErr error, err error) {
	gtx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("analytics savepoint begin: %w", err)
	}
	ferr := fn(ctx, New(gtx), gtx)
	if ferr == nil {
		if err := gtx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("analytics savepoint release: %w", err)
		}
		return nil, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		slog.Debug("analytics gauge failed as the snapshot context ended", "gauge_err", ferr, "ctx_err", cerr)
		return nil, cerr
	}
	if errors.Is(ClassifyAnalyticsErr(ferr), ErrAnalyticsTimeout) {
		return nil, ferr
	}
	if rerr := gtx.Rollback(ctx); rerr != nil {
		return nil, fmt.Errorf("analytics savepoint rollback: %w", rerr)
	}
	return ferr, nil
}
