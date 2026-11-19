package store

import (
	"context"

	"github.com/google/uuid"
)

// RunEnvVersionHasReceipt reports whether any run holds a delivery receipt for one secret version
// of a run environment variable: a receipted secret version is retired (or, on delete, kept pending
// deletion) so the run can still mask it, and an unreceipted one is dropped at once. It reads
// run_env_receipts (PRD #4416 M2). Every write path that retires or keeps a version asks this one
// function. The generated query takes a parameter struct; this keeps the positional form the
// handler's retention seam uses.
func (q *Queries) RunEnvVersionHasReceipt(ctx context.Context, varID uuid.UUID, rev int32) (bool, error) {
	return q.RunEnvSecretReceiptExists(ctx, RunEnvSecretReceiptExistsParams{VarID: varID, Rev: rev})
}
