package handler

import (
	"testing"

	"github.com/google/uuid"
)

// seedClaimProvenance steps a run from claim_generation 0 up to gen on behalf of worker, one +1 UPDATE
// per step, each setting claim_generation, worker_id and the provenance pair together, the only shape
// trg_runs_claim_provenance_guard accepts. A fixture that wants a Codex run to sit at
// claim_generation >= 1 and still be claimable by that worker uses this instead of writing the
// generation directly (an INSERT carrying provenance is refused).
func seedClaimProvenance(t *testing.T, exec func(sql string, args ...any), run, worker uuid.UUID, gen int64) {
	t.Helper()
	for g := int64(1); g <= gen; g++ {
		exec(`UPDATE runs SET claim_generation=$3, worker_id=$2,
			claim_provenance_worker_id=$2, claim_provenance_generation=$3 WHERE id=$1`, run, worker, g)
	}
}
