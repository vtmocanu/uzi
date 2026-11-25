package store

import (
	"context"
	"errors"
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

// analyticsSnapshotDeadline is the effective deadline; tests shrink it.
var analyticsSnapshotDeadline = AnalyticsSnapshotDeadline

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
	dctx, cancel := context.WithTimeout(ctx, analyticsSnapshotDeadline)
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
