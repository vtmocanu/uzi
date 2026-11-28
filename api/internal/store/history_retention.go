package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// history_retention.go is the pinned-session half of the history_retention sweeper pass (issue
// #4695). It is a separate copy of the worker_capacity_sample_lock.go pattern: the pass holds a
// SESSION advisory lock on one pool connection that is hijacked out of the pool for good, so a
// lock or a session setting can never leak into pooled use. Unlike that file it takes TWO
// contexts: the work context bounds every statement, the cleanup context bounds the unlock and
// the close, so a work deadline that already fired does not also forbid the cleanup.

// Settings applied once per session. Server-side idle and connection-check timers end a session
// whose client vanished, so its advisory lock cannot outlive a lost process for long.
const (
	historyRetentionIdleSessionTimeoutMS     = "5000"
	historyRetentionConnCheckIntervalMS      = "250"
	historyRetentionUnlockRepliedFalseReason = "advisory lock was not held at unlock"
)

// SQLSTATEs of a statement the server aborted before it changed anything: lock_timeout
// (lock_not_available) and statement_timeout (query_canceled).
const (
	historyRetentionSQLStateLockTimeout      = "55P03"
	historyRetentionSQLStateStatementTimeout = "57014"
)

// HistoryRetentionTable names one table the retention pass trims.
type HistoryRetentionTable string

// The tables the history_retention pass trims. pr_history_heads is deliberately absent.
const (
	HistoryRetentionPRHistoryEvents      HistoryRetentionTable = "pr_history_events"
	HistoryRetentionAnthropicUsage       HistoryRetentionTable = "anthropic_usage_samples"
	HistoryRetentionCodexUsage           HistoryRetentionTable = "codex_usage_samples"
	HistoryRetentionWorkerCapacitySample HistoryRetentionTable = "worker_capacity_samples"
)

// HistoryRetentionTables returns the trimmed tables in their fixed base order.
func HistoryRetentionTables() []HistoryRetentionTable {
	return []HistoryRetentionTable{
		HistoryRetentionPRHistoryEvents,
		HistoryRetentionAnthropicUsage,
		HistoryRetentionCodexUsage,
		HistoryRetentionWorkerCapacitySample,
	}
}

// HistoryRetentionRollbackError is a KNOWN rollback: the server aborted the delete batch with
// lock_timeout (55P03) or statement_timeout (57014) and the connection is idle again, so the
// batch changed nothing (0 rows). Every other DeleteBatch error is an unknown outcome.
type HistoryRetentionRollbackError struct {
	Table HistoryRetentionTable
	Code  string
	Err   error
}

func (e *HistoryRetentionRollbackError) Error() string {
	return fmt.Sprintf("history retention: delete %s rolled back (SQLSTATE %s): %v", e.Table, e.Code, e.Err)
}

func (e *HistoryRetentionRollbackError) Unwrap() error { return e.Err }

// StatementTimeout reports whether the batch hit statement_timeout (57014), the signal to shrink
// the next batch; a lock timeout (55P03) says nothing about the batch size.
func (e *HistoryRetentionRollbackError) StatementTimeout() bool {
	return e.Code == historyRetentionSQLStateStatementTimeout
}

// HistoryRetentionSession is a hijacked connection holding the pass's advisory lock.
type HistoryRetentionSession struct {
	conn *pgx.Conn
	q    *Queries
}

// OpenHistoryRetentionSession acquires a pool connection, removes it from the pool at once,
// applies the session settings (statement and lock timeouts in milliseconds) and tries the
// session advisory lock. Acquire, Hijack, the settings and the try run under workCtx. It returns
// (nil, false, nil) when another session holds the lock, and an error when the setup or the try
// failed; in both non-acquired cases the connection is closed under cleanupCtx, so an unknown
// lock result never survives and a work deadline that already fired does not shorten the close.
// Once acquired, Close(cleanupCtx) owns the cleanup. Either timeout under one millisecond is
// refused before anything is acquired: it would render as "0", which disables the server timer.
func OpenHistoryRetentionSession(workCtx, cleanupCtx context.Context, pool *pgxpool.Pool, statementTimeout, lockTimeout time.Duration) (*HistoryRetentionSession, bool, error) {
	return openHistoryRetentionSession(workCtx, cleanupCtx, pool, statementTimeout, lockTimeout, historyRetentionIdleSessionTimeoutMS)
}

// openHistoryRetentionSession is OpenHistoryRetentionSession with the idle_session_timeout value
// as a parameter, so a test can make the settings statement fail on the real server (an invalid
// value) without a test-only branch in the production path.
func openHistoryRetentionSession(workCtx, cleanupCtx context.Context, pool *pgxpool.Pool, statementTimeout, lockTimeout time.Duration, idleSessionTimeoutMS string) (*HistoryRetentionSession, bool, error) {
	if statementTimeout < time.Millisecond || lockTimeout < time.Millisecond {
		return nil, false, fmt.Errorf("history retention: statement timeout %v and lock timeout %v must each be at least 1ms", statementTimeout, lockTimeout)
	}
	pc, err := pool.Acquire(workCtx)
	if err != nil {
		return nil, false, fmt.Errorf("history retention: acquire connection: %w", err)
	}
	conn := pc.Hijack()
	var set string
	if err := conn.QueryRow(workCtx,
		`SELECT set_config('statement_timeout', $1, false) || set_config('lock_timeout', $2, false)
		     || set_config('idle_session_timeout', $3, false) || set_config('client_connection_check_interval', $4, false)`,
		strconv.FormatInt(statementTimeout.Milliseconds(), 10), strconv.FormatInt(lockTimeout.Milliseconds(), 10),
		idleSessionTimeoutMS, historyRetentionConnCheckIntervalMS).Scan(&set); err != nil {
		closeHistoryRetentionConn(cleanupCtx, conn)
		return nil, false, fmt.Errorf("history retention: session setup: %w", err)
	}
	var got bool
	if err := conn.QueryRow(workCtx, "SELECT pg_try_advisory_lock($1, 0)", HistoryRetentionLockClass).Scan(&got); err != nil {
		// Unknown whether the session now holds the lock: end the session.
		closeHistoryRetentionConn(cleanupCtx, conn)
		return nil, false, fmt.Errorf("history retention: try lock: %w", err)
	}
	if !got {
		closeHistoryRetentionConn(cleanupCtx, conn)
		return nil, false, nil
	}
	return &HistoryRetentionSession{conn: conn, q: New(conn)}, true, nil
}

// Cutoff returns the database clock reading and the retention cutoff from ONE statement, so both
// share one instant: cutoff is exactly retention before reference, to the microsecond. A
// retention that is not positive is refused.
func (s *HistoryRetentionSession) Cutoff(ctx context.Context, retention time.Duration) (reference, cutoff time.Time, err error) {
	if s.conn == nil {
		return time.Time{}, time.Time{}, errors.New("history retention: session is closed")
	}
	if retention <= 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("history retention: retention %v must be positive", retention)
	}
	var ref, cut pgtype.Timestamptz
	if err := s.conn.QueryRow(ctx, `SELECT statement_timestamp(), statement_timestamp() - $1::interval`,
		pgtype.Interval{Microseconds: retention.Microseconds(), Valid: true}).Scan(&ref, &cut); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("history retention: read cutoff: %w", err)
	}
	return ref.Time, cut.Time, nil
}

// DeleteBatch deletes at most limit rows of table strictly older than cutoff, as one autocommitted
// statement, and returns the confirmed row count. A *HistoryRetentionRollbackError means the
// server rolled the batch back (55P03 or 57014) with the connection idle: 0 rows, a known
// outcome. Any other error is an unknown outcome; the batch may or may not have committed.
func (s *HistoryRetentionSession) DeleteBatch(ctx context.Context, table HistoryRetentionTable, cutoff time.Time, limit int32) (int64, error) {
	if s.conn == nil {
		return 0, errors.New("history retention: session is closed")
	}
	if limit < 1 {
		return 0, fmt.Errorf("history retention: batch limit %d must be positive", limit)
	}
	ts := pgtype.Timestamptz{Time: cutoff, Valid: true}
	var n int64
	var err error
	switch table {
	case HistoryRetentionPRHistoryEvents:
		n, err = s.q.DeleteExpiredPRHistoryEvents(ctx, DeleteExpiredPRHistoryEventsParams{Cutoff: ts, BatchSize: limit})
	case HistoryRetentionAnthropicUsage:
		n, err = s.q.DeleteExpiredAnthropicUsageSamples(ctx, DeleteExpiredAnthropicUsageSamplesParams{Cutoff: ts, BatchSize: limit})
	case HistoryRetentionCodexUsage:
		n, err = s.q.DeleteExpiredCodexUsageSamples(ctx, DeleteExpiredCodexUsageSamplesParams{Cutoff: ts, BatchSize: limit})
	case HistoryRetentionWorkerCapacitySample:
		n, err = s.q.DeleteExpiredWorkerCapacitySamples(ctx, DeleteExpiredWorkerCapacitySamplesParams{Cutoff: ts, BatchSize: limit})
	default:
		return 0, fmt.Errorf("history retention: unknown table %q", table)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && s.Idle() &&
			(pgErr.Code == historyRetentionSQLStateLockTimeout || pgErr.Code == historyRetentionSQLStateStatementTimeout) {
			return 0, &HistoryRetentionRollbackError{Table: table, Code: pgErr.Code, Err: err}
		}
		return 0, fmt.Errorf("history retention: delete %s: %w", table, err)
	}
	return n, nil
}

// Idle reports whether the connection is open with no transaction open or failed, the only state
// in which a further statement (the unlock) is meaningful.
func (s *HistoryRetentionSession) Idle() bool {
	return s.conn != nil && !s.conn.IsClosed() && s.conn.PgConn().TxStatus() == 'I'
}

// Close releases the lock and ends the session. On an idle session it runs pg_advisory_unlock
// under ctx and reports unlocked=true only when PostgreSQL answered true; on any other state
// unlocked is false and the lock ends with the connection. The connection is ALWAYS closed
// (never returned to a pool). A false answer or an error is returned as an error. Close is safe
// to call twice.
func (s *HistoryRetentionSession) Close(ctx context.Context) (unlocked bool, err error) {
	if s.conn == nil {
		return false, nil
	}
	conn := s.conn
	s.conn = nil
	if !conn.IsClosed() && conn.PgConn().TxStatus() == 'I' {
		var got bool
		qerr := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, 0)", HistoryRetentionLockClass).Scan(&got)
		switch {
		case qerr != nil:
			err = fmt.Errorf("history retention: unlock: %w", qerr)
		case !got:
			err = errors.New("history retention: " + historyRetentionUnlockRepliedFalseReason)
		default:
			unlocked = true
		}
	}
	closeHistoryRetentionConn(ctx, conn)
	return unlocked, err
}

// closeHistoryRetentionConn closes a hijacked connection under the caller's ctx, so the close
// counts against the caller's deadline. pgconn.PgConn.Close closes the socket (a deferred
// net.Conn.Close) even when ctx is already expired; the ctx only bounds the Terminate message
// flush. Closing the socket ends the session and releases any session advisory lock it holds.
func closeHistoryRetentionConn(ctx context.Context, conn *pgx.Conn) {
	_ = conn.Close(ctx)
}
