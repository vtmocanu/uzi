package store

import (
	"context"
	"hash/fnv"
)

// JudgeCoordLockObjID derives the objid half of the JudgeDispositionCoordLockClass advisory lock
// from a judge recommendation coordinate. It is computed in Go and passed as an int4 param —
// never as a Postgres text/hashtext argument, because a NUL separator in text raises SQLSTATE
// 22021. A hash collision merely serializes two unrelated coordinates for a moment: a contention
// non-event, never a correctness one (the same property HostedProvisionLockClass records).
func JudgeCoordLockObjID(category, target string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(category))
	_, _ = h.Write([]byte{0}) // separator; a Go byte hash, never sent to Postgres
	_, _ = h.Write([]byte(target))
	return int32(h.Sum32()) //nolint:gosec // wraparound is fine: a lock key, not a number
}

// LockJudgeCoord takes the per-coordinate advisory lock as the first statement of a transaction,
// serializing the admin cross-user Mark-done write against the issue-filing settle on the same
// (category, target). db is a tx-bound DBTX (pgx.Tx satisfies it). See JudgeDispositionCoordLockClass.
func LockJudgeCoord(ctx context.Context, db DBTX, category, target string) error {
	_, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", JudgeDispositionCoordLockClass, JudgeCoordLockObjID(category, target))
	return err
}
