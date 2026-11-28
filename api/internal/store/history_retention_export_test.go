package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Test-only seams for the history retention session tests (issue #4695). Compiled only into this
// package's test binary, so the production surface stays unchanged.

// OpenHistoryRetentionSessionWithIdleForTest is OpenHistoryRetentionSession with the
// idle_session_timeout value chosen by the test: an invalid value makes the settings statement
// fail on the real server, which is the setup-error branch.
func OpenHistoryRetentionSessionWithIdleForTest(workCtx, cleanupCtx context.Context, pool *pgxpool.Pool, statementTimeout, lockTimeout time.Duration, idleSessionTimeoutMS string) (*HistoryRetentionSession, bool, error) {
	return openHistoryRetentionSession(workCtx, cleanupCtx, pool, statementTimeout, lockTimeout, idleSessionTimeoutMS)
}

// ExecForTest runs one statement on the session's pinned connection so a test can leave it in an
// open or failed transaction.
func (s *HistoryRetentionSession) ExecForTest(ctx context.Context, sql string) error {
	_, err := s.conn.Exec(ctx, sql)
	return err
}

// TxStatusForTest reports the pinned connection's transaction status byte ('I', 'T' or 'E').
func (s *HistoryRetentionSession) TxStatusForTest() byte {
	return s.conn.PgConn().TxStatus()
}

// PIDForTest reports the backend pid of the pinned connection.
func (s *HistoryRetentionSession) PIDForTest() uint32 {
	return s.conn.PgConn().PID()
}

// HistoryRetentionDeleteSQLForTest returns the generated DELETE statement text of one table, so a
// plan test explains exactly what the session runs.
func HistoryRetentionDeleteSQLForTest(table HistoryRetentionTable) string {
	switch table {
	case HistoryRetentionPRHistoryEvents:
		return deleteExpiredPRHistoryEvents
	case HistoryRetentionAnthropicUsage:
		return deleteExpiredAnthropicUsageSamples
	case HistoryRetentionCodexUsage:
		return deleteExpiredCodexUsageSamples
	case HistoryRetentionWorkerCapacitySample:
		return deleteExpiredWorkerCapacitySamples
	}
	return ""
}
