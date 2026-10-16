package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPolicyOwnerLiveCompletionPresentedToken(t *testing.T) {
	r := store.Run{Status: "awaiting_input", Kind: "issue", Harness: "codex", PolicyRefusalCount: 1, ClaimGeneration: 2,
		PolicyLastOutcomeID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}}
	dto := runToDTO(r, "normal", time.Hour, 3600, 3, time.Now(), 3)
	if dto.PolicyRefusal == nil || dto.PolicyRefusal.CanResume || dto.PolicyRefusal.ActionIfMatch != workersvc.PolicyHoldActionToken(r) {
		t.Fatalf("live completion must present the policy token independently of generic Resume availability: %+v", dto.PolicyRefusal)
	}
}
