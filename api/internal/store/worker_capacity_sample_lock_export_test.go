package store

import "context"

// ExecForTest runs one statement on the session's pinned connection so a test can leave it in an
// open or failed transaction. Compiled only into this package's test binary.
func (s *WorkerCapacitySampleSession) ExecForTest(ctx context.Context, sql string) error {
	_, err := s.conn.Exec(ctx, sql)
	return err
}

// TxStatusForTest reports the pinned connection's transaction status byte ('I', 'T' or 'E').
func (s *WorkerCapacitySampleSession) TxStatusForTest() byte {
	return s.conn.PgConn().TxStatus()
}
