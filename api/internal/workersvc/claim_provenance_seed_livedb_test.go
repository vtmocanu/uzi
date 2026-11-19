package workersvc

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// seedClaimProvenance puts a run at claim generation gen with its Codex claim provenance recorded
// for worker (issue #4476). The run_claim_provenance trigger accepts a provenance change only in an
// UPDATE that moves claim_generation by exactly +1 and sets worker_id and the pair together, so a
// generation seeded directly (as the legacy fixtures do) leaves a Codex row that no worker is
// admitted to. This rewinds the generation to 0 (no provenance change, so the trigger stays out of
// it) and then replays one +1 claim per generation, each by worker. The run must carry no
// provenance yet. exec is the fixture's own statement runner (the two live-DB environments differ
// in signature).
func seedClaimProvenance(t *testing.T, exec func(query string, args ...any), run, worker uuid.UUID, gen int64) {
	t.Helper()
	if gen < 1 {
		t.Fatalf("seedClaimProvenance: gen %d, want >= 1", gen)
	}
	exec(`UPDATE runs SET claim_generation = 0 WHERE id = $1`, run)
	for g := int64(1); g <= gen; g++ {
		exec(fmt.Sprintf(`UPDATE runs SET claim_generation = %d, worker_id = $2,
			claim_provenance_worker_id = $2, claim_provenance_generation = %d WHERE id = $1`, g, g), run, worker)
	}
}
