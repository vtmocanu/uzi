package store

import (
	"context"

	"github.com/google/uuid"
)

// RunEnvVersionHasReceipt reports whether any run holds a delivery receipt for one
// version of a run environment variable: a receipted secret version is retired (or,
// on delete, kept pending deletion) so the run can still mask it, and an unreceipted
// one is dropped at once.
//
// PRD #4416 M1 stores variables but injects nothing, so no run_env_receipts table
// exists yet and no version can have a receipt: this answers false. M2 adds the
// receipts table and replaces this body with a query on (var_id, rev). Every write
// path that retires or keeps a version asks this one function, so nothing else
// changes when M2 lands.
func (q *Queries) RunEnvVersionHasReceipt(_ context.Context, _ uuid.UUID, _ int32) (bool, error) {
	return false, nil
}
