package store

import (
	"context"
	"hash/fnv"

	"github.com/google/uuid"
)

// RunBranchLockObjID derives the objid half of the RunBranchLockClass advisory lock from a
// (repo, branch) pair. It is computed in Go and passed as an int4 param, never as a Postgres
// text/hashtext argument, because a NUL separator in text raises SQLSTATE 22021. A hash
// collision merely serializes two unrelated branches for a moment: a contention non-event,
// never a correctness one (the same property HostedProvisionLockClass records). Exported so
// the live-DB tests can find a waiter on exactly this key in pg_locks.
func RunBranchLockObjID(repoID uuid.UUID, branch string) int32 {
	h := fnv.New32a()
	_, _ = h.Write(repoID[:])
	_, _ = h.Write([]byte{0}) // separator; a Go byte hash, never sent to Postgres
	_, _ = h.Write([]byte(branch))
	return int32(h.Sum32()) //nolint:gosec // wraparound is fine: a lock key, not a number
}

// LockRunBranch takes the per-(repo, branch) advisory lock that serializes run creation on
// one agent branch across the issue, mr_rework and ci_fix kinds (issue #1626). Call it as the
// first statement of the create transaction's insert closure, before the cross-kind checks,
// on a tx-bound Queries (store.New(tx)). XACT-scoped, so it releases on commit or rollback.
//
// On a pool-bound Queries (no transaction) the statement runs in its own implicit transaction
// and the lock is released as soon as it returns, so it serializes nothing. Production always
// runs the create inside a transaction (cmd/server/main.go wires SetTxBeginner). See
// RunBranchLockClass.
func (q *Queries) LockRunBranch(ctx context.Context, repoID uuid.UUID, branch string) error {
	_, err := q.db.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", RunBranchLockClass, RunBranchLockObjID(repoID, branch))
	return err
}
