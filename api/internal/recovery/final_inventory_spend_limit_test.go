package recovery

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3767: a worker's FINAL proof must not end custody under an owner-held spend-limit hold,
// even when its claim was released (the owner decides via Resume, set-token or Cancel).
func TestFinalInventoryEligibleRefusesSpendLimitHold(t *testing.T) {
	w := uuid.New()
	hold := store.RecoveryCustodyHold{State: "open", InventoryGuarded: true, LiveWorkerID: pgtype.UUID{Bytes: w, Valid: true}}
	released := pgtype.Timestamptz{Valid: true}
	facts := finalRunFacts{status: "recovery_wait", gen: 2, released: released, parkCause: pgtype.Text{String: "anthropic_spend_limit", Valid: true}}
	if finalInventoryEligible(hold, w, facts, 2, false) {
		t.Fatal("spend-limit hold must not be releasable by a final proof")
	}
	// Control: the same facts with another timed cause stay releasable, so the refusal is the cause's.
	facts.parkCause = pgtype.Text{String: "provider_outage", Valid: true}
	if !finalInventoryEligible(hold, w, facts, 2, false) {
		t.Fatal("control cause should remain eligible")
	}
}
