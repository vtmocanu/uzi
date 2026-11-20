package store

import (
	"context"

	"github.com/google/uuid"
)

// RunEnvVersionHasReceipt reports whether any run holds a secret delivery receipt for one
// version of a run environment variable: a receipted secret version is retired (or, on
// delete, kept pending deletion) so the run can still mask it, and an unreceipted one is
// dropped at once. Plain deliveries are receipted too (the run page lists them) but never
// retained, so only secret receipts answer true. Every write path that retires or keeps a
// version asks this one function.
func (q *Queries) RunEnvVersionHasReceipt(ctx context.Context, varID uuid.UUID, rev int32) (bool, error) {
	return q.ExistsRunEnvSecretReceipt(ctx, ExistsRunEnvSecretReceiptParams{VarID: varID, Rev: rev})
}
