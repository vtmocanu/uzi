package handler

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunToDTOSetsClaimGeneration pins that the run DTO always carries the claim generation
// (PRD #3166): a real 0 (never claimed) stays distinct from a missing field.
func TestRunToDTOSetsClaimGeneration(t *testing.T) {
	for _, gen := range []int64{0, 3} {
		dto := runToDTO(store.Run{ClaimGeneration: gen}, "", 0, 0, 0, time.Now(), 0)
		if dto.ClaimGeneration == nil || *dto.ClaimGeneration != gen {
			t.Fatalf("ClaimGeneration for %d = %v, want non-nil %d", gen, dto.ClaimGeneration, gen)
		}
	}
}
