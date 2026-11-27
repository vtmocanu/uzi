package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// worker_capacity_sample_lock.go is the pinned-session half of the worker_capacity_samples
// sweeper pass (issue #4693). The pass holds a SESSION advisory lock on one pool connection
// that is hijacked out of the pool for good, so a lock or a session setting can never leak into
// pooled use. The pattern mirrors workersvc/checkpoint_retention.go's try-lock and proven unlock.

// Settings applied once per session. Server-side idle and connection-check timers end a session
// whose client vanished, so its advisory lock cannot outlive a lost process for long.
const (
	workerCapacitySampleIdleSessionTimeoutMS     = "5000"
	workerCapacitySampleConnCheckIntervalMS      = "250"
	workerCapacitySampleUnlockRepliedFalseReason = "advisory lock was not held at unlock"
)

// WorkerCapacitySampleSession is a hijacked connection holding the pass's advisory lock.
type WorkerCapacitySampleSession struct {
	conn *pgx.Conn
	q    *Queries
}

// OpenWorkerCapacitySampleSession acquires a pool connection, removes it from the pool at once,
// applies the session settings (statement and lock timeouts in milliseconds) and tries the
// session advisory lock. It returns (nil, false, nil) when another session holds the lock, and
// an error when the setup or the try failed; in both non-acquired cases the connection is
// closed, so an unknown lock result never survives. Either timeout under one millisecond is
// refused before anything is acquired: it would render as "0", which disables the server timer.
func OpenWorkerCapacitySampleSession(ctx context.Context, pool *pgxpool.Pool, statementTimeout, lockTimeout time.Duration) (*WorkerCapacitySampleSession, bool, error) {
	if statementTimeout < time.Millisecond || lockTimeout < time.Millisecond {
		return nil, false, fmt.Errorf("worker capacity samples: statement timeout %v and lock timeout %v must each be at least 1ms", statementTimeout, lockTimeout)
	}
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("worker capacity samples: acquire connection: %w", err)
	}
	conn := pc.Hijack()
	var set string
	if err := conn.QueryRow(ctx,
		`SELECT set_config('statement_timeout', $1, false) || set_config('lock_timeout', $2, false)
		     || set_config('idle_session_timeout', $3, false) || set_config('client_connection_check_interval', $4, false)`,
		strconv.FormatInt(statementTimeout.Milliseconds(), 10), strconv.FormatInt(lockTimeout.Milliseconds(), 10),
		workerCapacitySampleIdleSessionTimeoutMS, workerCapacitySampleConnCheckIntervalMS).Scan(&set); err != nil {
		closeWorkerCapacitySampleConn(ctx, conn)
		return nil, false, fmt.Errorf("worker capacity samples: session setup: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, 0)", WorkerCapacitySampleLockClass).Scan(&got); err != nil {
		// Unknown whether the session now holds the lock: end the session.
		closeWorkerCapacitySampleConn(ctx, conn)
		return nil, false, fmt.Errorf("worker capacity samples: try lock: %w", err)
	}
	if !got {
		closeWorkerCapacitySampleConn(ctx, conn)
		return nil, false, nil
	}
	return &WorkerCapacitySampleSession{conn: conn, q: New(conn)}, true, nil
}

// SampleBatch runs SampleWorkerCapacityBatch on the pinned connection: one autocommitted
// statement. The caller bounds it with ctx; the server bounds it with the session's timeouts.
func (s *WorkerCapacitySampleSession) SampleBatch(ctx context.Context, arg SampleWorkerCapacityBatchParams) (SampleWorkerCapacityBatchRow, error) {
	if s.conn == nil {
		return SampleWorkerCapacityBatchRow{}, errors.New("worker capacity samples: session is closed")
	}
	return s.q.SampleWorkerCapacityBatch(ctx, arg)
}

// Idle reports whether the connection is open with no transaction open or failed, the only state
// in which a further statement (the unlock) is meaningful.
func (s *WorkerCapacitySampleSession) Idle() bool {
	return s.conn != nil && !s.conn.IsClosed() && s.conn.PgConn().TxStatus() == 'I'
}

// Close releases the lock and ends the session. On an idle session it runs pg_advisory_unlock
// under ctx and reports unlocked=true only when PostgreSQL answered true; on any other state
// unlocked is false and the lock ends with the connection. The connection is ALWAYS closed
// (never returned to a pool). A false answer or an error is returned as an error. Close is safe
// to call twice.
func (s *WorkerCapacitySampleSession) Close(ctx context.Context) (unlocked bool, err error) {
	if s.conn == nil {
		return false, nil
	}
	conn := s.conn
	s.conn = nil
	if !conn.IsClosed() && conn.PgConn().TxStatus() == 'I' {
		var got bool
		qerr := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, 0)", WorkerCapacitySampleLockClass).Scan(&got)
		switch {
		case qerr != nil:
			err = fmt.Errorf("worker capacity samples: unlock: %w", qerr)
		case !got:
			err = errors.New("worker capacity samples: " + workerCapacitySampleUnlockRepliedFalseReason)
		default:
			unlocked = true
		}
	}
	closeWorkerCapacitySampleConn(ctx, conn)
	return unlocked, err
}

// closeWorkerCapacitySampleConn closes a hijacked connection under the caller's ctx, so the close
// counts against the caller's deadline. pgconn.PgConn.Close closes the socket (a deferred
// net.Conn.Close) even when ctx is already expired; the ctx only bounds the Terminate message
// flush. Closing the socket ends the session and releases any session advisory lock it holds.
func closeWorkerCapacitySampleConn(ctx context.Context, conn *pgx.Conn) {
	_ = conn.Close(ctx)
}
