package store

import (
	"context"

	"github.com/google/uuid"
)

// RunEnvVersionHasReceipt reports whether any run holds a delivery receipt for one
// secret version of a run environment variable: a receipted secret version is retired
// (or, on delete, kept pending deletion) so the run can still mask it, and an unreceipted
// one is dropped at once. Plain receipts never retain a version. Every write path that
// retires or keeps a version asks this one function.
func (q *Queries) RunEnvVersionHasReceipt(ctx context.Context, varID uuid.UUID, rev int32) (bool, error) {
	return q.RunEnvSecretVersionReceiptExists(ctx, RunEnvSecretVersionReceiptExistsParams{VarID: varID, Rev: rev})
}
