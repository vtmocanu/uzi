package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// provenanceReader is the one pool method seedClaimProvenance needs; both live-DB environments'
// pools satisfy it.
type provenanceReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// seedClaimProvenance puts a run at claim generation gen with its Codex claim provenance recorded
// for worker (issue #4476). The run_claim_provenance trigger accepts a provenance change only in an
// UPDATE that moves claim_generation by exactly +1 and sets worker_id and the pair together, so a
// generation seeded directly (as the legacy fixtures do) leaves a Codex row that no worker is
// admitted to. This rewinds the generation to 0 (no provenance change, so the trigger stays out of
// it) and then replays one +1 claim per generation, each by worker. The run must carry no
// provenance yet, and the helper fails the test if it does. exec is the fixture's own statement runner (the two live-DB environments differ
// in signature).
func seedClaimProvenance(t *testing.T, ctx context.Context, pool provenanceReader, exec func(query string, args ...any), run, worker uuid.UUID, gen int64) {
	t.Helper()
	if gen < 1 {
		t.Fatalf("seedClaimProvenance: gen %d, want >= 1", gen)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT claim_provenance_worker_id IS NOT NULL OR claim_provenance_generation IS NOT NULL
		FROM runs WHERE id = $1`, run).Scan(&recorded); err != nil {
		t.Fatalf("seedClaimProvenance: read run %s: %v", run, err)
	}
	if recorded {
		t.Fatalf("seedClaimProvenance: run %s already carries claim provenance", run)
	}
	exec(`UPDATE runs SET claim_generation = 0 WHERE id = $1`, run)
	for g := int64(1); g <= gen; g++ {
		exec(`UPDATE runs SET claim_generation = $3, worker_id = $2,
			claim_provenance_worker_id = $2, claim_provenance_generation = $3 WHERE id = $1`, run, worker, g)
	}
}
