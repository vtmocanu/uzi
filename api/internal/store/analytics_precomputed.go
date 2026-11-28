package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// analytics_precomputed.go is the store half of the stored analytics documents (issue #4771):
// a session that holds the single-flight advisory lock, the snapshot build run on it, and the
// guarded publish.
//
// Scoped claims, and what is not claimed:
//   - A single statement that reads a section (ListAnalyticsPrecomputedDocs) or one document
//     (GetAnalyticsPrecomputedDoc) sees one complete published generation: Publish replaces a
//     section in one transaction, so a reader sees the old set or the new set, never a mix.
//   - Failure handling covers an API-process failure (crash, kill, lost connection, cancelled
//     context) while PostgreSQL survives. The guarantee covers an OBSERVED disconnect only: the
//     idle timers apply only while the session is idle, the check during an active statement
//     (client_connection_check_interval) depends on the server's kernel observing the
//     disconnect, and the TCP keepalive settings below are best effort. A half-open connection
//     that neither side observes is NOT claimed to release promptly.
//   - A committed delete of a users or repos row (migration 00554's triggers) empties every
//     stored set and moves the invalidation counter in its own transaction; Publish refuses to
//     publish a build whose counter is stale.

// Session settings applied once per session with set_config(..., false). Server-side timers end
// a session whose client vanished, so its advisory lock cannot outlive a lost process for long.
const (
	analyticsPrecomputeStatementTimeout         = "10min"
	analyticsPrecomputeLockTimeout              = "10s"
	analyticsPrecomputeIdleSessionTimeout       = "60s"
	analyticsPrecomputeIdleInTxTimeout          = "60s"
	analyticsPrecomputeConnCheckInterval        = "1s"
	analyticsPrecomputeTCPKeepalivesIdle        = "30"
	analyticsPrecomputeTCPKeepalivesInterval    = "10"
	analyticsPrecomputeTCPKeepalivesCount       = "3"
	analyticsPrecomputeTCPUserTimeoutMS         = "60000"
	analyticsPrecomputePublishStatementTimeout  = "30s"
	analyticsPrecomputeRollbackTimeout          = 5 * time.Second
	analyticsPrecomputeUnlockRepliedFalseReason = "advisory lock was not held at unlock"
)

// ErrAnalyticsPrecomputedInvalidated is returned by Publish when a users or repos delete committed
// after the build read its counter: nothing was published.
var ErrAnalyticsPrecomputedInvalidated = errors.New("analytics precompute: build invalidated by a delete since its counter was read")

// ErrAnalyticsPrecomputePublishUnresolved wraps the error of a Publish whose commit returned an
// error or whose acknowledgement was lost (COMMIT may not have been sent if ctx was already done);
// the set may or may not have been published and the session is closed. Every other Publish error
// means the transaction failed before commit and nothing was published.
var ErrAnalyticsPrecomputePublishUnresolved = errors.New("analytics precompute: publish commit result unknown")

// AnalyticsPrecomputedDoc is one document of a section's set: the variant key and its exact bytes.
type AnalyticsPrecomputedDoc struct {
	Key      string
	Document []byte
}

// analyticsPrecomputeAfterTryLock is a test-only fault hook (nil in production): when it returns an
// error after a successful try-lock, the opener must close the connection and return the error.
var analyticsPrecomputeAfterTryLock func() error

// analyticsPrecomputeHooks are test-only pause and fault points on one session (all nil in
// production).
type analyticsPrecomputeHooks struct {
	afterPublishDelete func()
	afterPublishInsert func(i int)
	// afterPublishCommit runs after a successful COMMIT; an error simulates a lost acknowledgement.
	afterPublishCommit func() error
	// rollbackFault replaces the build's rollback: a non-nil error is its result and the real
	// rollback is skipped.
	rollbackFault func() error
}

// AnalyticsPrecomputeSession is a hijacked connection holding the build's advisory lock. The
// opener owns the connection until it hands the session over; after that the holder must Close it.
type AnalyticsPrecomputeSession struct {
	conn   *pgx.Conn
	broken bool
	hooks  analyticsPrecomputeHooks
}

// OpenAnalyticsPrecomputeSession acquires a pool connection, removes it from the pool at once,
// applies the session settings and tries the session advisory lock. It returns (nil, false, nil)
// when another session holds the lock. On a setup error, an unknown try result, a busy lock or a
// failed test fault the connection is closed before returning, so an unknown lock state never
// survives. The session settings end the session on the server after 60 seconds idle, so any
// Go-side gap over 60s (between statements inside a RunBuild fn, or between RunBuild and Publish)
// makes the server close the connection and the build then fails.
func OpenAnalyticsPrecomputeSession(ctx context.Context, pool *pgxpool.Pool) (*AnalyticsPrecomputeSession, bool, error) {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("analytics precompute: acquire connection: %w", err)
	}
	conn := pc.Hijack()
	var set string
	if err := conn.QueryRow(ctx,
		`SELECT set_config('statement_timeout', $1, false) || set_config('lock_timeout', $2, false)
		     || set_config('idle_session_timeout', $3, false) || set_config('idle_in_transaction_session_timeout', $4, false)
		     || set_config('client_connection_check_interval', $5, false)`,
		analyticsPrecomputeStatementTimeout, analyticsPrecomputeLockTimeout,
		analyticsPrecomputeIdleSessionTimeout, analyticsPrecomputeIdleInTxTimeout,
		analyticsPrecomputeConnCheckInterval).Scan(&set); err != nil {
		_ = conn.Close(ctx)
		return nil, false, fmt.Errorf("analytics precompute: session setup: %w", err)
	}
	// The TCP settings are best effort: a server or platform that refuses one (for example a
	// socket type without keepalives) must not stop the build. They run outside any transaction,
	// so a failure leaves the connection usable.
	if err := applyAnalyticsPrecomputeTCPSettings(ctx, conn); err != nil {
		slog.Warn("analytics precompute: TCP session settings not applied", "err", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, 0)", AnalyticsPrecomputeLockClass).Scan(&got); err != nil {
		// Unknown whether the session now holds the lock: end the session.
		_ = conn.Close(ctx)
		return nil, false, fmt.Errorf("analytics precompute: try lock: %w", err)
	}
	if !got {
		_ = conn.Close(ctx)
		return nil, false, nil
	}
	if hook := analyticsPrecomputeAfterTryLock; hook != nil {
		if herr := hook(); herr != nil {
			_ = conn.Close(ctx)
			return nil, false, herr
		}
	}
	return &AnalyticsPrecomputeSession{conn: conn}, true, nil
}

// analyticsPrecomputeTCPSettingsFault is a test-only fault hook (nil in production): a non-nil
// error replaces the TCP settings statement's result.
var analyticsPrecomputeTCPSettingsFault func() error

func applyAnalyticsPrecomputeTCPSettings(ctx context.Context, conn *pgx.Conn) error {
	if hook := analyticsPrecomputeTCPSettingsFault; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	var set string
	return conn.QueryRow(ctx,
		`SELECT set_config('tcp_keepalives_idle', $1, false) || set_config('tcp_keepalives_interval', $2, false)
		     || set_config('tcp_keepalives_count', $3, false) || set_config('tcp_user_timeout', $4, false)`,
		analyticsPrecomputeTCPKeepalivesIdle, analyticsPrecomputeTCPKeepalivesInterval,
		analyticsPrecomputeTCPKeepalivesCount, analyticsPrecomputeTCPUserTimeoutMS).Scan(&set)
}

// idle reports whether the connection is open with no transaction open or failed.
func (s *AnalyticsPrecomputeSession) idle() bool {
	return s.conn != nil && !s.conn.IsClosed() && s.conn.PgConn().TxStatus() == 'I'
}

// Idle reports whether the session is usable: open, not broken, and with no transaction open.
func (s *AnalyticsPrecomputeSession) Idle() bool {
	return !s.broken && s.idle()
}

// markBroken ends the session's connection: its transaction or lock state is unknown, so no
// further work may run on it. The lock ends with the connection.
func (s *AnalyticsPrecomputeSession) markBroken() {
	s.broken = true
	if s.conn != nil {
		cctx, cancel := context.WithTimeout(context.Background(), analyticsPrecomputeRollbackTimeout)
		defer cancel()
		_ = s.conn.Close(cctx)
	}
}

// Close releases the lock and ends the session. On an idle, unbroken session it runs
// pg_advisory_unlock under ctx and reports unlocked=true only when PostgreSQL answered true; in any
// other state unlocked is false and the lock ends with the connection. The connection is ALWAYS
// closed (never returned to a pool). Callers should pass a ctx with a deadline, since the unlock
// query and the connection teardown run under it. A false answer or an error is returned as an error. Close is
// idempotent: later calls return (false, nil). A session left in a transaction (not broken) is rolled
// back under ctx first, so the unlock can still be proven.
func (s *AnalyticsPrecomputeSession) Close(ctx context.Context) (unlocked bool, err error) {
	if s.conn == nil {
		return false, nil
	}
	conn := s.conn
	if !s.broken && !conn.IsClosed() && conn.PgConn().TxStatus() != 'I' {
		// A transaction is still open (for example after a panic in a RunBuild fn): end it,
		// bounded by ctx, so the unlock below can run on an idle connection.
		if _, rerr := conn.Exec(ctx, "ROLLBACK"); rerr != nil {
			slog.Debug("analytics precompute: close rollback failed", "err", rerr)
		}
	}
	idle := s.Idle()
	s.conn = nil
	if idle {
		var got bool
		qerr := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1, 0)", AnalyticsPrecomputeLockClass).Scan(&got)
		switch {
		case qerr != nil:
			err = fmt.Errorf("analytics precompute: unlock: %w", qerr)
		case !got:
			err = errors.New("analytics precompute: " + analyticsPrecomputeUnlockRepliedFalseReason)
		default:
			unlocked = true
		}
	}
	// pgconn closes the socket even when ctx has expired; ctx only bounds the Terminate flush.
	_ = conn.Close(ctx)
	return unlocked, err
}

// rollback ends tx and proves the connection is idle again. Any failure marks the session broken.
func (s *AnalyticsPrecomputeSession) rollback(ctx context.Context, tx pgx.Tx) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analyticsPrecomputeRollbackTimeout)
	defer cancel()
	var rerr error
	if s.hooks.rollbackFault != nil {
		rerr = s.hooks.rollbackFault()
	} else if err := tx.Rollback(rctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		rerr = err
	}
	if rerr == nil && !s.idle() {
		rerr = errors.New("connection is not idle after rollback")
	}
	if rerr != nil {
		s.markBroken()
		return fmt.Errorf("analytics precompute: rollback: %w", rerr)
	}
	return nil
}

// RunBuild runs fn inside a REPEATABLE READ READ ONLY transaction on the session connection. The
// first statement reads the invalidation counter, inside the same snapshot as everything fn reads,
// and fn receives it. Statements fn runs through s.Q share that one snapshot.
//
// The transaction is ALWAYS rolled back explicitly and the rollback's error is returned (joined
// with fn's). If the rollback fails or the connection is not idle afterwards, the session is
// marked broken and its connection closed: its lock ends with it, and nothing may publish on it.
// The session settings (10 minute statement timeout, 60 second idle-in-transaction timeout) bound
// each statement and each idle gap between statements, not the whole build. A Go-side gap over 60s
// inside fn, or between RunBuild returning and Publish, makes the server end the session, so the
// build then fails.
func (s *AnalyticsPrecomputeSession) RunBuild(ctx context.Context, fn func(ctx context.Context, s *AnalyticsSnapshot, counter int64) error) error {
	if !s.Idle() {
		return errors.New("analytics precompute: session is closed, broken or busy")
	}
	// JIT off for the reason RunAnalyticsSnapshot records; force_custom_plan keeps a long-lived
	// session from planning the build statements with a generic plan.
	tx, snap, err := beginAnalyticsTx(ctx, s.conn, []string{"SET LOCAL jit = off", "SET LOCAL plan_cache_mode = force_custom_plan"})
	if tx == nil {
		s.markBroken()
		return fmt.Errorf("analytics precompute: begin build: %w", err)
	}
	runErr := err
	if runErr == nil {
		runErr = s.build(ctx, snap, fn)
	}
	if rerr := s.rollback(ctx, tx); rerr != nil {
		return errors.Join(runErr, rerr)
	}
	return runErr
}

func (s *AnalyticsPrecomputeSession) build(ctx context.Context, snap *AnalyticsSnapshot, fn func(ctx context.Context, s *AnalyticsSnapshot, counter int64) error) error {
	counter, err := snap.Q.ReadAnalyticsPrecomputeCounter(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("analytics precompute: analytics_precomputed_state row is missing")
	}
	if err != nil {
		return fmt.Errorf("analytics precompute: read counter: %w", err)
	}
	return fn(ctx, snap, counter)
}

// Publish replaces the stored set of section with docs in ONE READ COMMITTED transaction: the
// guarded UPDATE (counter must still equal the one the build read), DELETE of the section's rows,
// one INSERT per document, COMMIT. A reader of the section in one statement sees the old set
// complete or the new set complete. generatedAt and counter are stored on every row.
//
// The session must still be alive: a Go-side gap over 60s since the last statement (including the
// gap between RunBuild and Publish) lets the server end it, and Publish then fails.
//
// Errors: ErrAnalyticsPrecomputedInvalidated means a delete committed since the build read its
// counter and nothing was published. An error wrapping ErrAnalyticsPrecomputePublishUnresolved
// means the commit returned an error or the acknowledgement was lost; the set may or may not have
// been published (COMMIT may not have been sent if ctx was already done; that case is conservatively
// reported unresolved) and the session is closed. Any other error means the transaction failed
// before COMMIT and nothing was published.
func (s *AnalyticsPrecomputeSession) Publish(ctx context.Context, section string, counter int64, generatedAt time.Time, docs []AnalyticsPrecomputedDoc) error {
	if !s.Idle() {
		return errors.New("analytics precompute: session is closed, broken or busy")
	}
	tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		s.markBroken()
		return fmt.Errorf("analytics precompute: begin publish: %w", err)
	}
	if perr := s.publishTx(ctx, tx, section, counter, generatedAt, docs); perr != nil {
		if rerr := s.rollback(ctx, tx); rerr != nil {
			return errors.Join(perr, rerr)
		}
		return perr
	}
	if err := tx.Commit(ctx); err != nil {
		s.markBroken()
		return fmt.Errorf("%w: %w", ErrAnalyticsPrecomputePublishUnresolved, err)
	}
	if hook := s.hooks.afterPublishCommit; hook != nil {
		if herr := hook(); herr != nil {
			s.markBroken()
			return fmt.Errorf("%w: %w", ErrAnalyticsPrecomputePublishUnresolved, herr)
		}
	}
	return nil
}

func (s *AnalyticsPrecomputeSession) publishTx(ctx context.Context, tx pgx.Tx, section string, counter int64, generatedAt time.Time, docs []AnalyticsPrecomputedDoc) error {
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+analyticsPrecomputePublishStatementTimeout+"'"); err != nil {
		return err
	}
	q := New(tx)
	// Pre-check: a missing state row is a distinct error, not ErrAnalyticsPrecomputedInvalidated
	// (the guarded UPDATE below would report 0 rows for it). Only the error is used.
	if _, err := q.ReadAnalyticsPrecomputeCounter(ctx); errors.Is(err, pgx.ErrNoRows) {
		return errors.New("analytics precompute: analytics_precomputed_state row is missing")
	} else if err != nil {
		return fmt.Errorf("analytics precompute: read counter: %w", err)
	}
	n, err := q.GuardAnalyticsPrecomputePublish(ctx, counter)
	if err != nil {
		return fmt.Errorf("analytics precompute: guard: %w", err)
	}
	if n == 0 {
		return ErrAnalyticsPrecomputedInvalidated
	}
	if err := q.DeleteAnalyticsPrecomputedSection(ctx, section); err != nil {
		return fmt.Errorf("analytics precompute: delete section: %w", err)
	}
	if hook := s.hooks.afterPublishDelete; hook != nil {
		hook()
	}
	ts := pgtype.Timestamptz{Time: generatedAt, Valid: true}
	for i, d := range docs {
		if err := q.InsertAnalyticsPrecomputedDoc(ctx, InsertAnalyticsPrecomputedDocParams{
			Section: section, VariantKey: d.Key, GeneratedAt: ts, Invalidation: counter, Document: d.Document,
		}); err != nil {
			return fmt.Errorf("analytics precompute: insert %q: %w", d.Key, err)
		}
		if hook := s.hooks.afterPublishInsert; hook != nil {
			hook(i)
		}
	}
	return nil
}

// RecordAnalyticsPrecomputeFailure counts one failed build on the state row. A missing state row
// (0 rows updated) is an error.
func RecordAnalyticsPrecomputeFailure(ctx context.Context, db DBTX, at time.Time) error {
	n, err := New(db).RecordAnalyticsPrecomputeFailure(ctx, pgtype.Timestamptz{Time: at, Valid: true})
	if err != nil {
		return fmt.Errorf("analytics precompute: record failure: %w", err)
	}
	if n == 0 {
		return errors.New("analytics precompute: analytics_precomputed_state row is missing")
	}
	return nil
}

// AnalyticsPrecomputeState is what a scheduler decides a build from: how many of a section's keys
// are stored, the oldest generated_at among them, and the shared failure status. A zero time means
// NULL (no stored key, or no recorded failure).
type AnalyticsPrecomputeState struct {
	Present           int64
	OldestGeneratedAt time.Time
	FailureCount      int32
	LastFailureAt     time.Time
}

// ReadAnalyticsPrecomputeStatus reads the state of section's keys in one statement on db.
func ReadAnalyticsPrecomputeStatus(ctx context.Context, db DBTX, section string, keys []string) (AnalyticsPrecomputeState, error) {
	row, err := New(db).AnalyticsPrecomputeStatus(ctx, AnalyticsPrecomputeStatusParams{Section: section, Keys: keys})
	if errors.Is(err, pgx.ErrNoRows) {
		return AnalyticsPrecomputeState{}, errors.New("analytics precompute: analytics_precomputed_state row is missing")
	}
	if err != nil {
		return AnalyticsPrecomputeState{}, fmt.Errorf("analytics precompute: read status: %w", err)
	}
	st := AnalyticsPrecomputeState{Present: row.Present, FailureCount: row.FailureCount}
	if row.OldestGeneratedAt.Valid {
		st.OldestGeneratedAt = row.OldestGeneratedAt.Time
	}
	if row.LastFailureAt.Valid {
		st.LastFailureAt = row.LastFailureAt.Time
	}
	return st, nil
}

// Status reads the same state on the session connection, after the lock was taken, so a scheduler
// can re-check freshness while it is the only builder. The session must be usable (see Idle).
func (s *AnalyticsPrecomputeSession) Status(ctx context.Context, section string, keys []string) (AnalyticsPrecomputeState, error) {
	if !s.Idle() {
		return AnalyticsPrecomputeState{}, errors.New("analytics precompute: session is closed, broken or busy")
	}
	return ReadAnalyticsPrecomputeStatus(ctx, s.conn, section, keys)
}
